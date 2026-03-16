`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{...}}`

Zed sends requests, the agent responds. It's fully bidirectional — the agent can also send requests back to Zed (for file reads, file writes, etc).

---

## What I Built, Layer by Layer

### Transport (`internal/transport`)

The bare minimum I/O layer. Reads lines from stdin with a buffered scanner, writes JSON to stdout with a mutex so concurrent goroutines don't interleave output.

### RPC (`internal/rpc`)

The JSON-RPC message type and helpers. `SendResult` and `SendError` write properly formatted responses. The `Raw` field preserves the original bytes for routing decisions.

`PromptBlock` is now a named struct (previously an anonymous inline struct) with full support for all three block types Zed sends:

- `"text"` — plain user input
- `"resource_link"` — `@file` attachment (URI only, content fetched via RPC)
- `"selected_text"` — highlighted editor selection, delivered inline with `StartLine`/`EndLine` metadata

### Session (`internal/session`)

Manages per-session conversation history. Each session has a message history (system prompt + turns), an active model, and a list of seen file URIs used for project root detection.

**Hybrid compaction** — every 10 turns, old turns get summarized into a `[memory]` block using `llama-3.1-8b-instant`, keeping only the last 4 turns verbatim. This keeps tokens flat while preserving context.

Four bugs were fixed here (see _Bugs Fixed_ below).

### Groq Client (`internal/groq`)

A streaming HTTP client for Groq's OpenAI-compatible API. Sends a request, reads the SSE stream, calls a callback for each delta chunk. Supports all models. Buffer is 1MB to handle large tool-call responses.

### File Reader (`internal/filereader`)

Implements the `fs/read_text_file` bidirectional RPC. When you `@mention` a file, Flarespark sends a request to Zed, parks a channel waiting for the response, and the main loop routes the response back to unblock it. Each request gets a unique `fs-N` ID.

**Selection reading was removed.** The previous `ReadSelection` method sent a `selection/get_content` RPC that does not exist in Zed's ACP. Selected text is already delivered inline in the prompt payload — no outgoing request is needed.

### File Writer (`internal/filewriter`)

Same pattern as the file reader but for writes. Sends `fs/write_text_file` to Zed which triggers the **native accept/reject diff UI**. Zed responds after the user accepts or rejects. Uses `fw-N` IDs so the router can distinguish them from read responses.

### Project Reader (`internal/projectreader`)

Reads the project on disk directly — no Zed RPC needed since the agent binary runs on the same machine.

**Root detection** — `ResolveRoot` tries an explicit path first (from `!project /path/to/project`), then falls back to `DetectRoot` which walks upward from any known file URI looking for `go.mod`, `package.json`, `Cargo.toml`, `pyproject.toml`, `.git`, `Makefile`, or `CMakeLists.txt`. Works for any language or project type.

**Secret blocking** — two independent enforcement points ensure secrets are never exposed:

- `Walk` skips secret files so they never appear in the tree Groq sees
- `ReadFiles` blocks them again so Groq cannot request them even if it hallucinates a path

Blocked categories include: all `.env` variants (`.env`, `.env.local`, `.env.production`, etc.), credential files (`.netrc`, `.npmrc`, `.pypirc`, `kubeconfig`, `serviceaccount.json`), secret configs (`secrets.yaml/json/toml`), SSH keys (`id_rsa`, `id_ed25519`), and certificate/key extensions (`.pem`, `.key`, `.p12`, `.crt`). `.env.example` is explicitly allowed since it contains only fake values.

**Two-pass query flow:**

1. `Tree(root, 400)` builds a compact indented file tree (~1 token per path) and sends it to Groq with the user's question
2. Groq returns a `<files>...</files>` block listing only the files it needs
3. `ReadFiles` reads those files from disk (max 10, max 32 KB each) and sends a second request with full content

This means a 500-file project costs roughly ~500 tokens for the tree in pass 1, plus only the content of the few files actually needed in pass 2 — never the full project.

### Handler (`internal/handler`)

The brain. Dispatches all ACP methods and implements the commands:

- `session/new` → creates a session, returns session ID
- `session/prompt` → the main chat handler, runs on a goroutine
- `session/config` → switches the active model
- `!help` → shows all commands
- `!model list / !model <fuzzy>` → model switching
- `!edit <instruction>` → reads file → asks Groq to rewrite → sends diff to Zed
- `@file` → reads file content and injects it into the prompt
- **`selected_text` blocks** → parsed inline from the prompt payload, injected into context with filename and line range
- **`!project <question>`** → two-pass project analysis (see Project Reader above)
- **`!project /path/to/project <question>`** → same but with an explicit root, no open file needed

Every `resource_link` and `selected_text` URI is recorded in the session's `SeenURIs` list passively, so `!project` can auto-detect the root without any extra user action.

The key insight for command detection: we track `instructionText` (only the user's typed text) separately from `userText` (text + file + selection contents), so commands are detected correctly even when files or selections are attached.

### Main Loop (`cmd/flarespark/main.go`)

Ties everything together. The critical design: **response routing**. When Zed responds to our outgoing file read/write requests, those messages have no `Method` field. The main loop checks the ID prefix — `fs-` goes to filereader, `fw-` goes to filewriter. Everything else goes to the handler.

The API key is resolved from multiple sources in order: env var → `~/.config/flarespark/config` → `~/.env` → `~/.env.local`.

---

## The Hardest Bugs Fixed

**Deadlock** — `session/prompt` originally ran synchronously, so when it tried to read a file via `fs/read_text_file`, the main loop was blocked waiting for the handler to return and couldn't process Zed's response. Fix: dispatch `session/prompt` with `go` so the main loop stays free.

**Response routing** — `IsWriteResponse("fw-1")` was doing `json.Unmarshal("fw-1")` trying to parse an already-unwrapped string as JSON, which always failed, so every write response fell through to the filereader. Fix: simple string prefix check, no JSON involved.

**`!edit` detection** — the instruction text was being checked after file content was appended, so `!edit` at the end of the assembled string was never at position 0. Fix: track instruction text separately before file content is mixed in.

**Selection reading** — `ReadSelection` sent `selection/get_content` as an outgoing RPC, which is not a Zed ACP method. The selected text is already inside the incoming `session/prompt` payload as a `"selected_text"` block. Fix: delete the method entirely, parse `block.Text` directly in the handler.

**Compaction re-firing** — `NeedsCompaction` used `Turns % compactAfterTurns == 0`, which is true at turns 10, 20, 30... forever. After the first compaction the history is short again, so `HistoryForCompaction` returned nil, `compact()` called Groq with a near-empty message list, got garbage back, and silently corrupted history. Fix: track `lastCompacted` in the session and only fire when `Turns >= lastCompacted + compactAfterTurns`.

**Compaction data race** — `HistoryForCompaction` returned a live slice into `sess.History`. The compaction goroutine held that slice while `AppendUser` or `ApplyCompaction` rewrote the underlying array on another goroutine. Fix: copy the slice before returning.

**Nil panic on missing session** — `AppendUser` did `sess := s.data[id]` with no nil check. If Zed sent a `session/prompt` for a session that didn't exist (e.g. after an agent restart), the goroutine panicked with no response sent, and Zed hung waiting. Fix: fall back to creating a fresh session.

**Silent Groq errors** — on a Groq timeout or API error, the error was logged to stderr and a `sendChunk` was called, but `rpc.SendResult` closed the turn in Zed immediately after — before the chunk could be displayed. Users saw silence instead of an error. Fix: always `sendChunk` the error first with a human-readable message and model-switch hint, then close the turn.

---

## The Stack

```
Zed Editor
↕ ACP (JSON-RPC over stdio)
Flarespark (Go binary)
↕ HTTPS streaming
Groq API (llama-3.3-70b / llama-3.1-8b-instant / etc)
```
