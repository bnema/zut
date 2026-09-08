package provider

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"golang.org/x/net/websocket"
)

// newContinuationWebSocketServer serves two sequential Responses on one
// WebSocket connection: interim text with explicit end_turn:false, then
// final text with no end_turn field (missing still terminates).
func newContinuationWebSocketServer(t *testing.T, mu *sync.Mutex, requests *[]map[string]any) *httptest.Server {
	t.Helper()
	texts := []string{"part one", "final"}
	endTurns := []any{false, nil}
	return httptest.NewServer(websocket.Handler(func(conn *websocket.Conn) {
		defer conn.Close()
		for i := 0; i < 2; i++ {
			var payload map[string]any
			if err := websocket.JSON.Receive(conn, &payload); err != nil {
				t.Errorf("receive websocket request: %v", err)
				return
			}
			mu.Lock()
			*requests = append(*requests, payload)
			mu.Unlock()
			response := map[string]any{
				"id":    "resp_" + string(rune('1'+i)),
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
			}
			if endTurns[i] != nil {
				response["end_turn"] = endTurns[i]
			}
			for _, event := range []map[string]any{
				{"type": "response.created", "response": map[string]any{"id": response["id"]}},
				{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message"}},
				{"type": "response.output_text.delta", "output_index": 0, "delta": texts[i]},
				{"type": "response.completed", "response": response},
			} {
				_ = websocket.JSON.Send(conn, event)
			}
		}
	}))
}

// streamContinuationResponse collects one Responses stream into its stop
// reason and assembled text.
func streamContinuationResponse(t *testing.T, client Client, req Request) (StopReason, string) {
	t.Helper()
	stream, err := client.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var (
		done EventDone
		text strings.Builder
	)
	for event := range stream {
		switch e := event.(type) {
		case EventTextDelta:
			text.WriteString(e.Delta)
		case EventDone:
			done = e
		}
	}
	if done.Err != nil {
		t.Fatalf("stream error: %v", done.Err)
	}
	return done.Stop, text.String()
}

// TestResponsesContinuationWebSocket pins explicit continuation over the
// Responses WebSocket transport: the first response carries
// end_turn:false, the second request reuses the connection with the
// expected previous-response/incremental state, and the second response
// terminates normally.
func TestResponsesContinuationWebSocket(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []map[string]any
	)
	server := newContinuationWebSocketServer(t, &mu, &requests)
	defer server.Close()

	inner := &codexClient{
		token:        "test-token",
		baseURL:      server.URL + "/v1/responses",
		providerName: "openai",
		capabilities: responsesCapabilities{StablePromptCacheKey: true},
	}
	client := newResponsesWebSocketClient(inner)

	first := Request{
		Model:    "gpt-5.6-sol",
		Context:  RequestContext{CacheSessionID: "cache-root-1", ThreadID: "thread-1", TurnID: "turn-1"},
		Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "first"}}}},
	}
	if stop, text := streamContinuationResponse(t, client, first); stop != StopContinue || text != "part one" {
		t.Fatalf("first response = (%q, %q), want (continue, part one)", stop, text)
	}

	second := Request{
		Model:   "gpt-5.6-sol",
		Context: RequestContext{CacheSessionID: "cache-root-1", ThreadID: "thread-1", TurnID: "turn-2"},
		Messages: append(append([]Message(nil), first.Messages...),
			Message{Role: RoleAssistant, Content: []Content{TextBlock{Text: "part one"}}},
			Message{Role: RoleUser, Content: []Content{TextBlock{Text: "second"}}},
		),
	}
	if stop, text := streamContinuationResponse(t, client, second); stop != StopEnd || text != "final" {
		t.Fatalf("second response = (%q, %q), want (end, final)", stop, text)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("websocket requests = %d, want 2 on one connection", len(requests))
	}
	if got := requests[1]["previous_response_id"]; got != "resp_1" {
		t.Fatalf("previous_response_id = %#v, want resp_1", got)
	}
	input, ok := requests[1]["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("incremental input = %#v", requests[1]["input"])
	}
	encoded, _ := json.Marshal(input[0])
	if !strings.Contains(string(encoded), "second") || strings.Contains(string(encoded), "part one") {
		t.Fatalf("incremental input = %s", encoded)
	}
}

// TestResponsesReasoningWebSocketTerminalBackfill pins terminal-only
// encrypted reasoning over the shared WebSocket path: the streamed item
// carries only an ID, the terminal response output supplies the payload,
// and the final assistant message keeps it for full-history replay.
func TestResponsesReasoningWebSocketTerminalBackfill(t *testing.T) {
	server := httptest.NewServer(websocket.Handler(func(conn *websocket.Conn) {
		defer conn.Close()
		var payload map[string]any
		if err := websocket.JSON.Receive(conn, &payload); err != nil {
			return
		}
		response := map[string]any{
			"id":    "resp-ws-1",
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
			"output": []any{
				map[string]any{"type": "reasoning", "id": "rs-ws-a", "encrypted_content": "blob-ws", "summary": []any{}},
				map[string]any{"type": "message", "id": "msg-ws-1", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "hello", "annotations": []any{}}}},
			},
		}
		for _, event := range []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": "resp-ws-1"}},
			{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "reasoning", "id": "rs-ws-a"}},
			{"type": "response.output_item.added", "output_index": 1, "item": map[string]any{"type": "message"}},
			{"type": "response.output_text.delta", "output_index": 1, "delta": "hello"},
			{"type": "response.completed", "response": response},
		} {
			_ = websocket.JSON.Send(conn, event)
		}
	}))
	defer server.Close()

	inner := &codexClient{
		token:        "test-token",
		baseURL:      server.URL + "/v1/responses",
		providerName: "openai",
		capabilities: responsesCapabilities{StablePromptCacheKey: true},
	}
	client := newResponsesWebSocketClient(inner)

	stream, err := client.Stream(context.Background(), Request{
		Model:    "gpt-5.6-sol",
		Context:  RequestContext{CacheSessionID: "cache-ws", ThreadID: "thread-ws-reasoning", TurnID: "turn-1"},
		Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var done EventDone
	for event := range stream {
		if e, ok := event.(EventDone); ok {
			done = e
		}
	}
	if done.Err != nil {
		t.Fatalf("stream error: %v", done.Err)
	}
	var sawReasoning bool
	for _, c := range done.Message.Content {
		if rb, ok := c.(ReasoningBlock); ok && rb.ID == "rs-ws-a" && rb.Encrypted == "blob-ws" {
			sawReasoning = true
		}
	}
	if !sawReasoning {
		t.Fatalf("websocket reasoning not backfilled: %#v", done.Message.Content)
	}

	wire, err := inner.buildRequest(Request{
		Model: "gpt-5.6-sol",
		Messages: []Message{
			{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}},
			done.Message,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var replayed bool
	for _, item := range wire.Input {
		if reasoning, ok := item.(codexReasoningItem); ok && reasoning.ID == "rs-ws-a" && reasoning.EncryptedContent == "blob-ws" {
			replayed = true
		}
	}
	if !replayed {
		t.Fatalf("terminal websocket payload did not reach full-history request: %#v", wire.Input)
	}
}
