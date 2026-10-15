package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// cancelNotifyTimeout bounds one best-effort notifications/cancelled send.
const cancelNotifyTimeout = 2 * time.Second

// shouldSendCancel reports whether abandoning a request of this method may be
// announced with notifications/cancelled. The MCP lifecycle forbids cancelling
// initialize; an abandoned initialize just fails and closes the connection.
func shouldSendCancel(method string) bool { return method != "initialize" }

// scrubRPCError returns a copy of e whose message is safe to print.
func (r *redactor) scrubRPCError(e *RPCError) *RPCError {
	if e == nil {
		return nil
	}
	cp := *e
	cp.Message = r.scrubOneLine(e.Message)
	return &cp
}

// message is one JSON-RPC 2.0 frame: request, notification, or response.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

func (m message) isResponse() bool { return m.Method == "" && len(m.ID) > 0 }
func (m message) isRequest() bool  { return m.Method != "" && len(m.ID) > 0 }

// RPCError is a JSON-RPC error object returned by a server.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("json-rpc error %d: %s", e.Code, e.Message) }

const (
	codeMethodNotFound = -32601
	codeInternalError  = -32603
)

// incomingHandler processes a server-initiated request or notification. For
// requests it returns the result or an error; for notifications the return
// values are ignored.
type incomingHandler func(msg message) (any, *RPCError)

// transport moves JSON-RPC frames to one server. request blocks until the
// response with id arrives, ctx ends, or the connection fails. Frames that
// arrive while waiting (notifications and server requests) are delivered to
// the handler installed by setHandler.
type transport interface {
	request(ctx context.Context, id int64, method string, params any) (json.RawMessage, error)
	notify(ctx context.Context, method string, params any) error
	setHandler(incomingHandler)
	// setProtocolVersion is called after initialize negotiates a version.
	setProtocolVersion(string)
	// done is closed when the connection is unusable.
	done() <-chan struct{}
	close() error
}

func encodeRequest(id int64, method string, params any) ([]byte, error) {
	frame := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		frame["params"] = params
	}
	return json.Marshal(frame)
}

func encodeNotification(method string, params any) ([]byte, error) {
	frame := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		frame["params"] = params
	}
	return json.Marshal(frame)
}

func encodeResponse(id json.RawMessage, result any, rpcErr *RPCError) ([]byte, error) {
	frame := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		frame["error"] = rpcErr
	} else {
		if result == nil {
			result = struct{}{}
		}
		frame["result"] = result
	}
	return json.Marshal(frame)
}

// parseFrames decodes either one JSON-RPC object or a batch array.
func parseFrames(raw []byte) ([]message, error) {
	raw = trimSpace(raw)
	if len(raw) == 0 {
		return nil, nil
	}
	if raw[0] == '[' {
		var batch []message
		if err := json.Unmarshal(raw, &batch); err != nil {
			return nil, err
		}
		return batch, nil
	}
	var msg message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, err
	}
	return []message{msg}, nil
}

func trimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\t' || b[start] == '\r' || b[start] == '\n') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\r' || b[end-1] == '\n') {
		end--
	}
	return b[start:end]
}

// idKey normalizes a JSON id for map lookup ("7" and 7 are distinct ids in
// JSON-RPC, but zut only issues integers, so compare the canonical text).
func idKey(id json.RawMessage) string { return string(trimSpace(id)) }
