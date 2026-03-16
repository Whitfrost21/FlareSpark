// Package filereader requests file contents from Zed via fs/read_text_file.
//
// Zed sends resource_link blocks with a URI but no content.
// To get the content we send a fs/read_text_file request back to Zed
// and wait for the response on the same stdio channel.
//
// Flow:
//  1. Read(path) sends the request and registers a pending channel.
//  2. The main loop sees a response with no Method and calls Deliver().
//  3. Read() unblocks and returns the content.
//
// NOTE: selected_text blocks already carry their content inline inside the
// prompt payload — no outgoing RPC is needed for selections.
package filereader

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Whitfrost21/FlareSpark/internal/rpc"
	"github.com/Whitfrost21/FlareSpark/internal/transport"
)

// Reader sends file read requests to Zed and waits for their responses.
type Reader struct {
	mu      sync.Mutex
	pending map[string]chan result
	counter atomic.Int64
}

type result struct {
	content string
	err     error
}

func New() *Reader {
	return &Reader{pending: make(map[string]chan result)}
}

// Read requests the content of a file from Zed by URI.
// Blocks until Zed responds or times out after 10 seconds.
func (r *Reader) Read(uri string, sessionID string) (string, error) {
	id := fmt.Sprintf("fs-%d", r.counter.Add(1))
	rawID, _ := json.Marshal(id)
	jsonID := json.RawMessage(rawID)

	ch := make(chan result, 1)
	r.mu.Lock()
	r.pending[id] = ch
	r.mu.Unlock()

	// Zed's fs/read_text_file expects a plain path, not a file:// URI.
	path := strings.TrimPrefix(uri, "file://")

	transport.Write(map[string]any{
		"jsonrpc": "2.0",
		"id":      &jsonID,
		"method":  "fs/read_text_file",
		"params": map[string]any{
			"sessionId": sessionID,
			"path":      path,
		},
	})

	select {
	case res := <-ch:
		return res.content, res.err
	case <-time.After(10 * time.Second):
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
		return "", fmt.Errorf("timeout reading file: %s", path)
	}
}

// Deliver is called by the main loop when Zed responds to a fs/read_text_file request.
func (r *Reader) Deliver(id string, content string, errMsg string) {
	r.mu.Lock()
	ch, ok := r.pending[id]
	if ok {
		delete(r.pending, id)
	}
	r.mu.Unlock()

	if !ok {
		return
	}
	if errMsg != "" {
		ch <- result{err: fmt.Errorf("%s", errMsg)}
	} else {
		ch <- result{content: content}
	}
}

// IsFileRequest returns true if the message ID belongs to a Read() call.
func IsFileRequest(id string) bool {
	return len(id) > 3 && id[:3] == "fs-"
}

// ParseResponse extracts id, text content, and error message from a raw Zed response.
func ParseResponse(msg rpc.Message) (id, content, errMsg string) {
	if err := json.Unmarshal(*msg.ID, &id); err != nil {
		return
	}

	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(msg.Raw, &envelope); err != nil {
		return
	}

	if envelope.Error.Message != "" {
		return id, "", envelope.Error.Message
	}

	var res struct {
		Content string `json:"content"`
		Text    string `json:"text"`
	}
	json.Unmarshal(envelope.Result, &res)
	content = res.Content
	if content == "" {
		content = res.Text
	}
	return id, content, ""
}
