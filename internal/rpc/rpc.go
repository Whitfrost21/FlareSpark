// Package rpc defines JSON-RPC 2.0 message types and send helpers.
package rpc

import (
	"encoding/json"

	"github.com/Whitfrost21/FlareSpark/internal/transport"
)

// Message is a raw JSON-RPC 2.0 envelope.
type Message struct {
	Jsonrpc string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Raw     []byte           `json:"-"` // full original line, set by the scan loop
}

func SendResult(id *json.RawMessage, result any) {
	transport.Write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func SendError(id *json.RawMessage, code int, message string) {
	transport.Write(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
}

func SendNotification(method string, params any) {
	transport.Write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}
