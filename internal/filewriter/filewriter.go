// Package filewriter writes file contents back to Zed via fs/write_text_file.
// Zed displays the change in its native diff UI so the user can accept or reject it.
package filewriter

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Whitfrost21/FlareSpark/internal/rpc"
	"github.com/Whitfrost21/FlareSpark/internal/transport"
)

// Writer sends file write requests to Zed and waits for acknowledgement.
type Writer struct {
	mu      sync.Mutex
	pending map[string]chan error
	counter atomic.Int64
}

func New() *Writer {
	return &Writer{pending: make(map[string]chan error)}
}

// Write sends new file content to Zed via fs/write_text_file.
// Zed shows a diff and lets the user accept or reject it.
// Blocks until Zed acknowledges or times out.
func (w *Writer) Write(uri, sessionID, content string) error {
	id := fmt.Sprintf("fw-%d", w.counter.Add(1))
	rawID, _ := json.Marshal(id)
	jsonID := json.RawMessage(rawID)

	ch := make(chan error, 1)
	w.mu.Lock()
	w.pending[id] = ch
	w.mu.Unlock()

	path := stripFilePrefix(uri)

	transport.Write(map[string]any{
		"jsonrpc": "2.0",
		"id":      &jsonID,
		"method":  "fs/write_text_file",
		"params": map[string]any{
			"sessionId": sessionID,
			"path":      path,
			"content":   content,
		},
	})

	select {
	case err := <-ch:
		return err
	case <-time.After(15 * time.Second):
		w.mu.Lock()
		delete(w.pending, id)
		w.mu.Unlock()
		return fmt.Errorf("timeout writing file: %s", path)
	}
}

// Deliver is called by the main loop when Zed responds to a write request.
func (w *Writer) Deliver(id string, errMsg string) {
	w.mu.Lock()
	ch, ok := w.pending[id]
	if ok {
		delete(w.pending, id)
	}
	w.mu.Unlock()

	if !ok {
		return
	}
	if errMsg != "" {
		ch <- fmt.Errorf("%s", errMsg)
	} else {
		ch <- nil
	}
}

// IsWriteResponse returns true if the ID belongs to a write request.
// id should already be the unwrapped string (not JSON-encoded).
func IsWriteResponse(id string) bool {
	return len(id) > 3 && id[:3] == "fw-"
}

// ParseResponse extracts the request ID and any error from a Zed response.
func ParseResponse(msg rpc.Message) (id, errMsg string) {
	if err := json.Unmarshal(*msg.ID, &id); err != nil {
		fmt.Fprintf(os.Stderr, "[filewriter] failed to unmarshal id: %v\n", err)
		return
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(msg.Raw, &envelope)
	return id, envelope.Error.Message
}

func stripFilePrefix(uri string) string {
	if len(uri) > 7 && uri[:7] == "file://" {
		return uri[7:]
	}
	return uri
}
