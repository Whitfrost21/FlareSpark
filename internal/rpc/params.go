// params.go holds ACP request/response param structs.
package rpc

// InitializeParams is sent by Zed on startup.
type InitializeParams struct {
	ProtocolVersion int `json:"protocolVersion"`
}

// PromptBlock is a single block inside the prompt array.
// Zed sends three relevant types:
//
//	"text"          – plain user text
//	"resource_link" – an @file attachment (URI only, content fetched via fs/read_text_file)
//	"selected_text" – highlighted editor selection (text is inline, no RPC needed)
type PromptBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"` // "text" and "selected_text" blocks
	URI  string `json:"uri,omitempty"`  // "resource_link" and "selected_text" blocks
	Name string `json:"name,omitempty"` // human-readable filename for resource_link

	// Selection metadata — present on "selected_text" blocks.
	StartLine int `json:"startLine,omitempty"`
	EndLine   int `json:"endLine,omitempty"`
}

// PromptParams carries the session ID and prompt blocks.
type PromptParams struct {
	SessionID string        `json:"sessionId"`
	Prompt    []PromptBlock `json:"prompt"`
}

// ConfigParams is sent by session/config to change session settings.
type ConfigParams struct {
	SessionID string `json:"sessionId"`
	Model     string `json:"model"`
}
