// Package handler wires ACP methods to the groq client and session store.
package handler

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Whitfrost21/FlareSpark/internal/filereader"
	"github.com/Whitfrost21/FlareSpark/internal/filewriter"
	"github.com/Whitfrost21/FlareSpark/internal/groq"
	"github.com/Whitfrost21/FlareSpark/internal/projectreader"
	"github.com/Whitfrost21/FlareSpark/internal/rpc"
	"github.com/Whitfrost21/FlareSpark/internal/session"
)

// Handler dispatches ACP JSON-RPC methods.
type Handler struct {
	sessions *session.Store
	groq     *groq.Client
	files    *filereader.Reader
	writer   *filewriter.Writer
}

func New(sessions *session.Store, groqClient *groq.Client, fr *filereader.Reader, fw *filewriter.Writer) *Handler {
	return &Handler{sessions: sessions, groq: groqClient, files: fr, writer: fw}
}

func (h *Handler) Dispatch(msg rpc.Message) {
	if msg.ID == nil {
		fmt.Fprintln(os.Stderr, "[flarespark] notification:", msg.Method)
		return
	}

	switch msg.Method {
	case "initialize":
		h.handleInitialize(msg)
	case "session/new":
		h.handleSessionNew(msg)
	case "session/prompt":
		go h.handleSessionPrompt(msg)
	case "session/cancel":
		rpc.SendResult(msg.ID, map[string]any{})
	case "session/config":
		h.handleSessionConfig(msg)
	case "session/models":
		h.handleSessionModels(msg)
	default:
		fmt.Fprintln(os.Stderr, "[flarespark] unknown method:", msg.Method)
		rpc.SendError(msg.ID, -32601, "Method not found: "+msg.Method)
	}
}

func (h *Handler) handleInitialize(msg rpc.Message) {
	var params rpc.InitializeParams
	json.Unmarshal(msg.Params, &params)
	v := params.ProtocolVersion
	if v == 0 {
		v = 1
	}
	rpc.SendResult(msg.ID, map[string]any{
		"protocolVersion": v,
		"agentInfo": map[string]any{
			"name":    "flarespark",
			"version": "0.1.0",
		},
		"agentCapabilities": map[string]any{
			"loadSession":        false,
			"promptCapabilities": map[string]any{},
		},
	})
}

func (h *Handler) handleSessionNew(msg rpc.Message) {
	id := h.sessions.New()
	rpc.SendResult(msg.ID, map[string]any{"sessionId": id})
}

func (h *Handler) handleSessionConfig(msg rpc.Message) {
	var params rpc.ConfigParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		rpc.SendError(msg.ID, -32602, "invalid params: "+err.Error())
		return
	}
	if err := h.sessions.SetModel(params.SessionID, params.Model); err != nil {
		rpc.SendError(msg.ID, -32602, err.Error())
		return
	}
	fmt.Fprintf(os.Stderr, "[flarespark] session %s switched to model %s\n", params.SessionID, params.Model)
	rpc.SendResult(msg.ID, map[string]any{
		"sessionId": params.SessionID,
		"model":     params.Model,
	})
}

func (h *Handler) handleSessionModels(msg rpc.Message) {
	models := make([]map[string]any, len(groq.Available))
	for i, m := range groq.Available {
		models[i] = map[string]any{
			"id":          m.ID,
			"displayName": m.DisplayName,
			"description": m.Description,
		}
	}
	rpc.SendResult(msg.ID, map[string]any{"models": models})
}

func (h *Handler) handleSessionPrompt(msg rpc.Message) {
	var params rpc.PromptParams
	json.Unmarshal(msg.Params, &params)

	instructionText := ""
	userText := ""
	fileURIs := map[string]string{}

	for _, block := range params.Prompt {
		switch block.Type {

		case "text":
			instructionText += block.Text
			userText += block.Text

		case "resource_link":
			name := block.Name
			if name == "" {
				name = block.URI
			}
			fileURIs[name] = block.URI
			// Track URI so !project can detect the root automatically.
			h.sessions.AddURI(params.SessionID, block.URI)

			sendChunk(params.SessionID, fmt.Sprintf("📂 Reading `%s`...", name))
			content, err := h.files.Read(block.URI, params.SessionID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[flarespark] file read error %s: %v\n", block.URI, err)
				userText += fmt.Sprintf("\n\n[Could not read %s: %v]", name, err)
			} else {
				userText += fmt.Sprintf("\n\n--- %s ---\n```\n%s\n```", name, content)
			}

		case "selected_text":
			if block.Text == "" {
				continue
			}
			if block.URI != "" {
				h.sessions.AddURI(params.SessionID, block.URI)
			}
			source := block.URI
			if source == "" {
				source = "editor"
			} else {
				source = strings.TrimPrefix(source, "file://")
				if idx := strings.LastIndex(source, "/"); idx >= 0 {
					source = source[idx+1:]
				}
			}
			location := source
			if block.StartLine > 0 && block.EndLine > 0 {
				location = fmt.Sprintf("%s lines %d–%d", source, block.StartLine, block.EndLine)
			}
			userText += fmt.Sprintf("\n\n--- selected text (%s) ---\n```\n%s\n```", location, block.Text)
			fmt.Fprintf(os.Stderr, "[flarespark] got selection from %s (%d chars)\n", location, len(block.Text))
		}
	}

	trimmed := strings.TrimSpace(instructionText)
	switch {
	case trimmed == "!help":
		h.handleHelpCommand(msg.ID, params.SessionID)
		return
	case strings.HasPrefix(trimmed, "!model"):
		h.handleModelCommand(msg.ID, params.SessionID, trimmed)
		return
	case strings.HasPrefix(trimmed, "!edit"):
		h.handleEditCommand(msg.ID, params.SessionID, trimmed, fileURIs)
		return
	case strings.HasPrefix(trimmed, "!project"):
		h.handleProjectCommand(msg.ID, params.SessionID, trimmed)
		return
	}

	h.streamPrompt(msg.ID, params.SessionID, userText)
}

// handleProjectCommand runs the two-pass project analysis:
//
//	Pass 1 — send file tree + question → Groq returns which files to read.
//	Pass 2 — read those files from disk → Groq streams the answer.
//
// Usage:
//
//	!project <question>                     — auto-detect root from open files
//	!project /path/to/project <question>    — explicit root (no open file needed)
func (h *Handler) handleProjectCommand(id *json.RawMessage, sessionID, text string) {
	arg := strings.TrimSpace(strings.TrimPrefix(text, "!project"))

	// If the first token looks like an absolute path, treat it as an explicit root.
	var explicitRoot, question string
	if strings.HasPrefix(arg, "/") || (len(arg) > 2 && arg[1] == ':') { // unix or windows abs path
		parts := strings.SplitN(arg, " ", 2)
		explicitRoot = parts[0]
		if len(parts) > 1 {
			question = strings.TrimSpace(parts[1])
		}
	} else {
		question = arg
	}

	if question == "" {
		question = "Give me a high-level overview of this project: its purpose, structure, and main components."
	}

	// Resolve root: explicit path wins, otherwise auto-detect from session URIs.
	uris := h.sessions.SeenURIs(sessionID)
	root, err := projectreader.ResolveRoot(explicitRoot, uris)
	if err != nil {
		sendChunk(sessionID, fmt.Sprintf("⚠️ %v", err))
		rpc.SendResult(id, map[string]any{"stopReason": "end_turn"})
		return
	}
	if root == "" {
		sendChunk(sessionID,
			"⚠️ Could not detect the project root.\n\n"+
				"Either open/attach any file from your project first, or provide the path explicitly:\n"+
				"`!project /path/to/my/project <question>`")
		rpc.SendResult(id, map[string]any{"stopReason": "end_turn"})
		return
	}

	sendChunk(sessionID, fmt.Sprintf("🔍 Scanning `%s`...", root))

	tree, err := projectreader.Tree(root, 400)
	if err != nil {
		sendChunk(sessionID, fmt.Sprintf("⚠️ Could not walk project: %v", err))
		rpc.SendResult(id, map[string]any{"stopReason": "end_turn"})
		return
	}

	// ── Pass 1: Groq picks which files it needs ───────────────────────────
	pass1Prompt := fmt.Sprintf(`You are a coding assistant helping analyze a project.

Here is the complete file tree:

%s

The user's question is:
%q

List ONLY the relative file paths you need to read to answer this question accurately.
Return them inside <files> tags, one path per line.
Pick the minimum necessary set (max 10 files).
Do NOT include test files, generated files, or lock files unless the question is specifically about them.

Example format:
<files>
internal/handler/handler.go
internal/rpc/params.go
</files>`, tree, question)

	sendChunk(sessionID, "🤔 Identifying relevant files...")

	_, model := h.sessions.AppendUser(sessionID, text)

	pass1Response, err := h.groq.Complete(model, []groq.Message{
		{Role: "system", Content: "You are a coding assistant. Follow instructions exactly."},
		{Role: "user", Content: pass1Prompt},
	}, func(_ string) {}) // pass 1 is internal — don't stream it to chat

	if err != nil {
		sendChunk(sessionID, fmt.Sprintf("⚠️ Groq error (pass 1): %v", err))
		rpc.SendResult(id, map[string]any{"stopReason": "end_turn"})
		return
	}

	filePaths := projectreader.ParseFileList(pass1Response)
	if len(filePaths) == 0 {
		sendChunk(sessionID,
			"⚠️ Could not determine which files to read.\n\nTry a more specific question, e.g.:\n"+
				"`!project How does the session compaction work?`")
		rpc.SendResult(id, map[string]any{"stopReason": "end_turn"})
		return
	}
	if len(filePaths) > 10 {
		filePaths = filePaths[:10]
	}

	sendChunk(sessionID, fmt.Sprintf("📂 Reading: `%s`", strings.Join(filePaths, "`, `")))

	// ── Pass 2: read files from disk, stream the answer ───────────────────
	fileContents := projectreader.ReadFiles(root, filePaths)

	pass2Prompt := fmt.Sprintf(`Here is the relevant source code:
%s

Answer this question about the project:
%s`, fileContents, question)

	fmt.Fprintf(os.Stderr, "[flarespark] !project pass2: %d chars, files: %v\n",
		len(pass2Prompt), filePaths)

	fullResponse, err := h.groq.Complete(model, []groq.Message{
		{Role: "system", Content: "You are a coding assistant. Be concise. Prefer code over explanation."},
		{Role: "user", Content: pass2Prompt},
	}, func(delta string) {
		sendChunk(sessionID, delta)
	})

	if err != nil {
		sendChunk(sessionID, fmt.Sprintf("⚠️ Groq error (pass 2): %v", err))
	} else {
		h.sessions.AppendAssistant(sessionID, fullResponse)
	}

	rpc.SendResult(id, map[string]any{"stopReason": "end_turn"})
}

func (h *Handler) streamPrompt(id *json.RawMessage, sessionID, userText string) {
	if h.sessions.NeedsCompaction(sessionID) {
		h.compact(sessionID)
	}
	history, model := h.sessions.AppendUser(sessionID, userText)
	fmt.Fprintf(os.Stderr, "[flarespark] using model %s, history len %d\n", model, len(history))

	fullResponse, err := h.groq.Complete(model, history, func(delta string) {
		sendChunk(sessionID, delta)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "[flarespark] groq error:", err)
		// BUG FIX: previously the error was logged but sendChunk was called
		// AFTER rpc.SendResult in some code paths, meaning Zed had already
		// closed the turn and the error chunk was dropped silently.
		// Now we always send the error chunk first, then close the turn.
		sendChunk(sessionID, "\n\n⚠️ **Groq error** — "+err.Error()+
			"\n\nThis may be a timeout or token-limit issue. Try `!model llama8b` for a faster model.")
	} else {
		h.sessions.AppendAssistant(sessionID, fullResponse)
	}
	rpc.SendResult(id, map[string]any{"stopReason": "end_turn"})
}

func (h *Handler) compact(sessionID string) {
	turns := h.sessions.HistoryForCompaction(sessionID)
	if len(turns) == 0 {
		// Nothing old enough — mark anyway so NeedsCompaction doesn't re-fire.
		h.sessions.MarkCompacted(sessionID)
		return
	}
	msgs := make([]groq.Message, 0, len(turns)+1)
	msgs = append(msgs, turns...)
	msgs = append(msgs, groq.Message{Role: "user", Content: session.CompactionPrompt})

	fmt.Fprintf(os.Stderr, "[flarespark] compacting %d turns\n", len(turns)/2)
	summary, err := h.groq.Complete(session.CompactionModel, msgs, func(_ string) {})
	if err != nil {
		// Failed compaction: mark so we don't retry on every subsequent turn.
		fmt.Fprintf(os.Stderr, "[flarespark] compaction error: %v\n", err)
		h.sessions.MarkCompacted(sessionID)
		return
	}
	h.sessions.ApplyCompaction(sessionID, summary)
	h.sessions.MarkCompacted(sessionID)
}

func (h *Handler) handleModelCommand(id *json.RawMessage, sessionID, text string) {
	arg := strings.TrimSpace(strings.TrimPrefix(text, "!model"))
	var reply string
	switch {
	case arg == "" || arg == "list":
		var sb strings.Builder
		sb.WriteString("**Available models:**\n\n")
		current := h.sessions.GetOrCreate(sessionID).Model
		for _, m := range groq.Available {
			marker := "  "
			if m.ID == current {
				marker = "✓ "
			}
			sb.WriteString(fmt.Sprintf("%s`%s` (%s) — %s\n\n", marker, m.ID, m.Nick, m.Description))
		}
		sb.WriteString("\nTo switch: `!model <id or nickname>`")
		reply = sb.String()
	default:
		matched, ok := groq.Fuzzy(arg)
		if !ok {
			reply = fmt.Sprintf("⚠️ No model matched %q — try `!model list`", arg)
		} else if err := h.sessions.SetModel(sessionID, matched.ID); err != nil {
			reply = "⚠️ " + err.Error()
		} else {
			reply = fmt.Sprintf("✓ Switched to `%s`", matched.ID)
		}
	}
	sendChunk(sessionID, reply)
	rpc.SendResult(id, map[string]any{"stopReason": "end_turn"})
}

func sendChunk(sessionID, text string) {
	rpc.SendNotification("session/update", map[string]any{
		"sessionId": sessionID,
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content": map[string]any{
				"type": "text",
				"text": text,
			},
		},
	})
}

func (h *Handler) handleEditCommand(id *json.RawMessage, sessionID, text string, fileURIs map[string]string) {
	instruction := strings.TrimSpace(strings.TrimPrefix(text, "!edit"))
	if len(fileURIs) == 0 {
		sendChunk(sessionID, "⚠️ Attach a file with @filename before using `!edit`")
		rpc.SendResult(id, map[string]any{"stopReason": "end_turn"})
		return
	}
	if len(fileURIs) > 1 {
		sendChunk(sessionID, "⚠️ `!edit` works on one file at a time — attach a single @filename")
		rpc.SendResult(id, map[string]any{"stopReason": "end_turn"})
		return
	}
	var fileName, fileURI string
	for k, v := range fileURIs {
		fileName, fileURI = k, v
	}
	if instruction == "" {
		instruction = "improve this code"
	}
	sendChunk(sessionID, fmt.Sprintf("✏️ Editing `%s`...", fileName))
	current, err := h.files.Read(fileURI, sessionID)
	if err != nil {
		sendChunk(sessionID, fmt.Sprintf("⚠️ Could not read file: %v", err))
		rpc.SendResult(id, map[string]any{"stopReason": "end_turn"})
		return
	}
	editPrompt := fmt.Sprintf(
		"Rewrite the following file applying this instruction: %s\n\nReturn ONLY the complete new file content with no explanation, no markdown fences, no commentary.\n\n--- %s ---\n%s",
		instruction, fileName, current,
	)
	_, model := h.sessions.AppendUser(sessionID, text)
	newContent, err := h.groq.Complete(model, []groq.Message{
		{Role: "system", Content: "You are a code editor. Return only raw file content, nothing else."},
		{Role: "user", Content: editPrompt},
	}, func(_ string) {})
	if err != nil {
		sendChunk(sessionID, fmt.Sprintf("⚠️ Groq error: %v", err))
		rpc.SendResult(id, map[string]any{"stopReason": "end_turn"})
		return
	}
	if err := h.writer.Write(fileURI, sessionID, newContent); err != nil {
		sendChunk(sessionID, fmt.Sprintf("⚠️ Could not write file: %v", err))
	} else {
		sendChunk(sessionID, fmt.Sprintf("✅ Edit ready for `%s` — accept or reject in the diff view", fileName))
		h.sessions.AppendAssistant(sessionID, fmt.Sprintf("Edited %s: %s", fileName, instruction))
	}
	rpc.SendResult(id, map[string]any{"stopReason": "end_turn"})
}

func (h *Handler) handleHelpCommand(id *json.RawMessage, sessionID string) {
	reply := `**Flarespark commands**

| Command | Description |
|---------|-------------|
| ` + "`!help`" + ` | Show this help |
| ` + "`!model list`" + ` | List available models |
| ` + "`!model <n>`" + ` | Switch model (fuzzy match) |
| ` + "`@file`" + ` | Attach a file for context |
| ` + "`@file !edit <instruction>`" + ` | Rewrite file — shows diff in Zed |
| ` + "`!project <question>`" + ` | Analyze project (auto-detects root) |
| ` + "`!project /path <question>`" + ` | Analyze project at explicit path |`

	sendChunk(sessionID, reply)
	rpc.SendResult(id, map[string]any{"stopReason": "end_turn"})
}
