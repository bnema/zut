package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func TestResponsesWebSocketReconnectsMissingPreviousResponseWithFullContext(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []map[string]any
		visits   int
	)
	ws := websocket.Handler(func(conn *websocket.Conn) {
		defer conn.Close()
		mu.Lock()
		visits++
		visit := visits
		mu.Unlock()
		if visit == 1 {
			var first map[string]any
			if err := websocket.JSON.Receive(conn, &first); err != nil {
				return
			}
			mu.Lock()
			requests = append(requests, first)
			mu.Unlock()
			for _, event := range []map[string]any{
				{"type": "response.created", "response": map[string]any{"id": "resp_1"}},
				{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message"}},
				{"type": "response.output_text.delta", "output_index": 0, "delta": "ok"},
				{"type": "response.completed", "response": map[string]any{"id": "resp_1", "usage": map[string]any{}}},
			} {
				_ = websocket.JSON.Send(conn, event)
			}
			var second map[string]any
			if err := websocket.JSON.Receive(conn, &second); err != nil {
				return
			}
			mu.Lock()
			requests = append(requests, second)
			mu.Unlock()
			_ = websocket.JSON.Send(conn, map[string]any{
				"type": "error", "status": 400,
				"error": map[string]any{"code": "previous_response_not_found", "message": "evicted"},
			})
			return
		}
		var recovered map[string]any
		if err := websocket.JSON.Receive(conn, &recovered); err != nil {
			return
		}
		mu.Lock()
		requests = append(requests, recovered)
		mu.Unlock()
		_ = websocket.JSON.Send(conn, map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_2"}})
		_ = websocket.JSON.Send(conn, map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message"}})
		_ = websocket.JSON.Send(conn, map[string]any{"type": "response.output_text.delta", "output_index": 0, "delta": "reconnected"})
		_ = websocket.JSON.Send(conn, map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_2", "usage": map[string]any{}}})
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			ws.ServeHTTP(w, r)
			return
		}
		t.Errorf("unexpected HTTP fallback: %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := newResponsesWebSocketClient(&codexClient{
		token: "test-token", baseURL: server.URL + "/v1/responses", providerName: "openai",
		capabilities: responsesCapabilities{StablePromptCacheKey: true}, http: &http.Client{},
	})
	first := Request{Model: "gpt-5.6-sol", Context: RequestContext{CacheSessionID: "cache-root-1", ThreadID: "thread-1"}, Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "first"}}}}}
	assertCompletedResponse(t, client, first)
	lifecycle := &recordingRequestLifecycle{}
	second := Request{Model: "gpt-5.6-sol", Context: RequestContext{CacheSessionID: "cache-root-1", ThreadID: "thread-1"}, Lifecycle: lifecycle, Messages: append(append([]Message(nil), first.Messages...), Message{Role: RoleAssistant, Content: []Content{TextBlock{Text: "ok"}}}, Message{Role: RoleUser, Content: []Content{TextBlock{Text: "second"}}})}
	assertCompletedResponseText(t, client, second, "reconnected")
	mu.Lock()
	defer mu.Unlock()
	if visits != 2 || len(requests) != 3 {
		t.Fatalf("WebSocket visits/requests = %d/%d, want 2/3", visits, len(requests))
	}
	if len(lifecycle.ids) != 2 || lifecycle.ids[0] == lifecycle.ids[1] {
		t.Fatalf("reconnect request IDs = %#v, want two distinct IDs", lifecycle.ids)
	}
	if requests[2]["previous_response_id"] != nil {
		t.Fatalf("reconnect previous_response_id = %#v, want absent", requests[2]["previous_response_id"])
	}
	recovered, _ := json.Marshal(requests[2]["input"])
	if !strings.Contains(string(recovered), "first") || !strings.Contains(string(recovered), "second") {
		t.Fatalf("reconnect did not receive complete context: %s", recovered)
	}
}

func TestResponsesWebSocketReusesSessionAndSendsIncrementalInput(t *testing.T) {
	var (
		mu       sync.Mutex
		headers  []map[string]string
		requests []map[string]any
	)
	server := httptest.NewServer(websocket.Handler(func(conn *websocket.Conn) {
		defer conn.Close()
		for i := 0; i < 2; i++ {
			var payload map[string]any
			if err := websocket.JSON.Receive(conn, &payload); err != nil {
				t.Errorf("receive websocket request: %v", err)
				return
			}
			mu.Lock()
			requests = append(requests, payload)
			headers = append(headers, map[string]string{
				"authorization":      conn.Request().Header.Get("authorization"),
				"chatgpt-account-id": conn.Request().Header.Get("chatgpt-account-id"),
				"openai-beta":        conn.Request().Header.Get("openai-beta"),
			})
			mu.Unlock()
			_ = websocket.JSON.Send(conn, map[string]any{
				"type": "response.created",
				"response": map[string]any{
					"id": "resp_" + string(rune('1'+i)),
				},
			})
			_ = websocket.JSON.Send(conn, map[string]any{
				"type":         "response.output_item.added",
				"output_index": 0,
				"item":         map[string]any{"type": "message"},
			})
			_ = websocket.JSON.Send(conn, map[string]any{
				"type":         "response.output_text.delta",
				"output_index": 0,
				"delta":        "ok",
			})
			_ = websocket.JSON.Send(conn, map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id":    "resp_" + string(rune('1'+i)),
					"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
				},
			})
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

	lifecycle := &recordingRequestLifecycle{}
	first := Request{
		Model:     "gpt-5.6-sol",
		FastMode:  true,
		Context:   RequestContext{CacheSessionID: "cache-root-1", ThreadID: "thread-1", TurnID: "turn-1"},
		Lifecycle: lifecycle,
		Messages:  []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "first"}}}},
	}
	assertCompletedResponse(t, client, first)
	second := Request{
		Model:    "gpt-5.6-sol",
		FastMode: true,
		Context:  RequestContext{CacheSessionID: "cache-root-1", ThreadID: "thread-1", TurnID: "turn-2"},
		Messages: append(append([]Message(nil), first.Messages...),
			Message{Role: RoleAssistant, Content: []Content{TextBlock{Text: "ok"}}},
			Message{Role: RoleUser, Content: []Content{TextBlock{Text: "second"}}},
		),
	}
	assertCompletedResponse(t, client, second)
	if len(lifecycle.ids) != 1 || lifecycle.ids[0] == "" {
		t.Fatalf("WebSocket request IDs = %#v", lifecycle.ids)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("websocket requests = %d, want 2", len(requests))
	}
	if got := requests[0]["type"]; got != "response.create" {
		t.Fatalf("first type = %#v", got)
	}
	if got := requests[0]["store"]; got != false {
		t.Fatalf("store = %#v, want false", got)
	}
	if _, ok := requests[0]["stream"]; ok {
		t.Fatalf("WebSocket payload unexpectedly carries stream: %#v", requests[0])
	}
	if got := requests[0]["prompt_cache_key"]; got != "cache-root-1" {
		t.Fatalf("prompt_cache_key = %#v", got)
	}
	if got := requests[0]["service_tier"]; got != fastModeServiceTier {
		t.Fatalf("service_tier = %#v", got)
	}
	if got := requests[1]["previous_response_id"]; got != "resp_1" {
		t.Fatalf("previous_response_id = %#v", got)
	}
	input, ok := requests[1]["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("incremental input = %#v", requests[1]["input"])
	}
	encoded, _ := json.Marshal(input[0])
	if !strings.Contains(string(encoded), "second") || strings.Contains(string(encoded), "ok") {
		t.Fatalf("incremental input = %s", encoded)
	}
	for _, got := range headers {
		if got["authorization"] != "Bearer test-token" {
			t.Fatalf("authorization = %q", got["authorization"])
		}
		if got["chatgpt-account-id"] != "" || got["openai-beta"] != "" {
			t.Fatalf("public WebSocket leaked Codex headers: %#v", got)
		}
	}
}

func TestOpenAICodexUsesWebSocketWithSubscriptionHeaders(t *testing.T) {
	headers := make(chan http.Header, 1)
	requests := make(chan map[string]any, 1)
	server := httptest.NewServer(websocket.Handler(func(conn *websocket.Conn) {
		defer conn.Close()
		headers <- conn.Request().Header.Clone()
		var payload map[string]any
		if err := websocket.JSON.Receive(conn, &payload); err != nil {
			t.Errorf("receive websocket request: %v", err)
			return
		}
		requests <- payload
		for _, event := range []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": "resp_subscription"}},
			{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message"}},
			{"type": "response.output_text.delta", "output_index": 0, "delta": "ok"},
			{"type": "response.completed", "response": map[string]any{"id": "resp_subscription", "usage": map[string]any{}}},
		} {
			_ = websocket.JSON.Send(conn, event)
		}
	}))
	defer server.Close()

	client := NewOpenAICodex("subscription-token", "account-1", server.URL)
	websocketClient, ok := client.(*responsesWebSocketClient)
	if !ok {
		t.Fatalf("NewOpenAICodex returned %T, want WebSocket Responses client", client)
	}
	defer websocketClient.Close()

	request := Request{
		Model:    "gpt-5.6-luna",
		Context:  RequestContext{CacheSessionID: "root-1", ThreadID: "thread-1", TurnID: "turn-1"},
		Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hello"}}}},
	}
	assertCompletedResponse(t, client, request)

	gotHeaders := <-headers
	for name, want := range map[string]string{
		"authorization":       "Bearer subscription-token",
		"chatgpt-account-id":  "account-1",
		"openai-beta":         "responses=experimental",
		"originator":          "codex_cli_rs",
		"user-agent":          "codex_cli_rs/0.0.0",
		"session-id":          "root-1",
		"thread-id":           "thread-1",
		"x-client-request-id": "thread-1",
	} {
		if got := gotHeaders.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if got := gotHeaders.Get("origin"); got != server.URL {
		t.Fatalf("origin = %q, want %q", got, server.URL)
	}
	if got := (<-requests)["prompt_cache_key"]; got != "root-1" {
		t.Fatalf("prompt_cache_key = %#v, want root cache ID", got)
	}
}

func TestWithHTTPClientPreservesSubscriptionHeaders(t *testing.T) {
	client := NewOpenAICodex("subscription-token", "account-1", "https://example.test/backend-api/codex/responses")
	customHTTP := &http.Client{Transport: &http.Transport{}}
	WithHTTPClient(client, customHTTP)

	websocketClient := client.(*responsesWebSocketClient)
	if websocketClient.http.http != customHTTP {
		t.Fatal("subscription client was wrapped in the public OpenAI header-stripping transport")
	}
}

func TestResponsesWebSocketWireBaselineChangeForcesFullRequest(t *testing.T) {
	mappedModel := "gpt-5.6-sol-a"
	client := newResponsesWebSocketClient(&codexClient{
		providerName: "openai",
		capabilities: responsesCapabilities{StablePromptCacheKey: true},
		modelName:    func(string) string { return mappedModel },
	}).(*responsesWebSocketClient)
	first := Request{
		Model:    "gpt-5.6-sol",
		Context:  RequestContext{CacheSessionID: "cache-root-1", ThreadID: "thread-1", TurnID: "turn-1"},
		Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "first"}}}},
	}
	baseline, err := client.canonicalResponsesBaseline(first, first.Context.CacheSessionID)
	if err != nil {
		t.Fatal(err)
	}
	session := &responsesWebSocketSession{}
	session.recordCompleted(first, baseline, Message{Role: RoleAssistant, Content: []Content{TextBlock{Text: "ok"}}}, "resp_1")

	mappedModel = "gpt-5.6-sol-b"
	second := Request{
		Model:   first.Model,
		Context: RequestContext{CacheSessionID: "cache-root-1", ThreadID: "thread-1", TurnID: "turn-2"},
		Messages: append(append([]Message(nil), first.Messages...),
			Message{Role: RoleAssistant, Content: []Content{TextBlock{Text: "ok"}}},
			Message{Role: RoleUser, Content: []Content{TextBlock{Text: "second"}}},
		),
	}
	streamReq, previousResponseID := session.incrementalRequest(second, second.Context.CacheSessionID, client.canonicalResponsesBaseline)
	if previousResponseID != "" {
		t.Fatalf("previous response ID = %q, want full request after wire baseline change", previousResponseID)
	}
	if !reflect.DeepEqual(streamReq.Messages, second.Messages) {
		t.Fatalf("stream messages = %#v, want complete transcript %#v", streamReq.Messages, second.Messages)
	}
}

func TestResponsesWebSocketAssistantMismatchForcesCompleteTranscript(t *testing.T) {
	client := newResponsesWebSocketClient(&codexClient{
		providerName: "openai", capabilities: responsesCapabilities{StablePromptCacheKey: true},
	}).(*responsesWebSocketClient)
	first := Request{
		Model: "gpt-5.6-sol", Context: RequestContext{CacheSessionID: "cache-root-1", ThreadID: "thread-1", TurnID: "turn-1"},
		Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "first"}}}},
	}
	baseline, err := client.canonicalResponsesBaseline(first, first.Context.CacheSessionID)
	if err != nil {
		t.Fatal(err)
	}
	session := &responsesWebSocketSession{}
	session.recordCompleted(first, baseline, Message{Role: RoleAssistant, Content: []Content{TextBlock{Text: "actual"}}}, "resp_1")
	second := Request{
		Model: first.Model, Context: RequestContext{CacheSessionID: "cache-root-1", ThreadID: "thread-1", TurnID: "turn-2"},
		Messages: append(append([]Message(nil), first.Messages...),
			Message{Role: RoleAssistant, Content: []Content{TextBlock{Text: "modified"}}},
			Message{Role: RoleUser, Content: []Content{TextBlock{Text: "second"}}},
		),
	}
	streamReq, previousResponseID := session.incrementalRequest(second, second.Context.CacheSessionID, client.canonicalResponsesBaseline)
	if previousResponseID != "" {
		t.Fatalf("previous response ID = %q, want complete request", previousResponseID)
	}
	if !reflect.DeepEqual(streamReq.Messages, second.Messages) {
		t.Fatalf("stream messages = %#v, want complete transcript %#v", streamReq.Messages, second.Messages)
	}
}

func TestResponsesWebSocketCancellationClosesOnlyActiveConnection(t *testing.T) {
	closed := make(chan struct{})
	var closeOnce sync.Once
	server := httptest.NewServer(websocket.Handler(func(conn *websocket.Conn) {
		defer closeOnce.Do(func() { close(closed) })
		defer conn.Close()
		var payload map[string]any
		if err := websocket.JSON.Receive(conn, &payload); err != nil {
			return
		}
		var ignored any
		_ = websocket.JSON.Receive(conn, &ignored)
	}))
	defer server.Close()

	client := newResponsesWebSocketClient(&codexClient{
		token: "test-token", baseURL: server.URL + "/v1/responses", providerName: "openai",
		capabilities: responsesCapabilities{StablePromptCacheKey: true},
	})
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.Stream(ctx, Request{Model: "gpt-5.6-sol", Context: RequestContext{CacheSessionID: "cache-root-1", ThreadID: "thread-1"}})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	for range stream {
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not close active WebSocket")
	}
}

func TestResponsesWebSocketIgnoresStaleTerminalFrameBeforeNextResponse(t *testing.T) {
	server := httptest.NewServer(websocket.Handler(func(conn *websocket.Conn) {
		defer conn.Close()
		for turn := 1; turn <= 2; turn++ {
			var payload map[string]any
			if err := websocket.JSON.Receive(conn, &payload); err != nil {
				return
			}
			if turn == 2 {
				// A buffered terminal from the prior response must not complete
				// this response before its response.created frame arrives.
				_ = websocket.JSON.Send(conn, map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1", "usage": map[string]any{}}})
			}
			id := "resp_" + string(rune('0'+turn))
			text := "first"
			if turn == 2 {
				text = "second"
			}
			for _, event := range []map[string]any{
				{"type": "response.created", "response": map[string]any{"id": id}},
				{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message"}},
				{"type": "response.output_text.delta", "output_index": 0, "delta": text},
				{"type": "response.completed", "response": map[string]any{"id": id, "usage": map[string]any{}}},
			} {
				_ = websocket.JSON.Send(conn, event)
			}
		}
	}))
	defer server.Close()

	client := newResponsesWebSocketClient(&codexClient{
		token: "test-token", baseURL: server.URL + "/v1/responses", providerName: "openai",
		capabilities: responsesCapabilities{StablePromptCacheKey: true}, http: &http.Client{},
	})
	first := Request{Model: "gpt-5.6-sol", Context: RequestContext{CacheSessionID: "cache-root-1", ThreadID: "thread-1"}, Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "first"}}}}}
	assertCompletedResponseText(t, client, first, "first")
	second := Request{Model: "gpt-5.6-sol", Context: RequestContext{CacheSessionID: "cache-root-1", ThreadID: "thread-1"}, Messages: append(append([]Message(nil), first.Messages...), Message{Role: RoleAssistant, Content: []Content{TextBlock{Text: "first"}}}, Message{Role: RoleUser, Content: []Content{TextBlock{Text: "second"}}})}
	assertCompletedResponseText(t, client, second, "second")
}

func TestResponsesWebSocketCloseReleasesSessionSockets(t *testing.T) {
	client := newResponsesWebSocketClient(&codexClient{}).(*responsesWebSocketClient)
	client.session("resident-child")
	if err := client.Close(); err != nil {
		t.Fatalf("close responses websocket client: %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.sessions) != 0 {
		t.Fatalf("sessions retained after close: %d", len(client.sessions))
	}
}

type cacheDiagnosticsLifecycle struct {
	diagnostics []CacheDiagnostics
}

func (l *cacheDiagnosticsLifecycle) RequestAttempt(int, int)                {}
func (l *cacheDiagnosticsLifecycle) RetryScheduled(int, int, time.Duration) {}

func (l *cacheDiagnosticsLifecycle) CacheDiagnostics(diagnostics CacheDiagnostics) {
	l.diagnostics = append(l.diagnostics, diagnostics)
}

func TestResponsesWebSocketUsesHTTPDiagnosticsForIncompatibleTransport(t *testing.T) {
	lifecycle := &cacheDiagnosticsLifecycle{}
	client := newResponsesWebSocketClient(&codexClient{
		token: "test-token", baseURL: "https://example.test/v1/responses", providerName: "openai",
		capabilities: responsesCapabilities{StablePromptCacheKey: true},
		http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(`data: {"type":"response.completed","response":{"status":"completed","usage":{}}}

`)),
			}, nil
		})},
	})
	req := Request{
		Model: "gpt-5.6-sol", Context: RequestContext{CacheSessionID: "cache-root", ThreadID: "thread"}, Lifecycle: lifecycle,
		Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "request"}}}},
	}
	stream, err := client.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if len(lifecycle.diagnostics) != 1 || lifecycle.diagnostics[0].Transport != "http_sse" {
		t.Fatalf("diagnostics = %#v, want only HTTP/SSE diagnostics", lifecycle.diagnostics)
	}
}

func assertCompletedResponse(t *testing.T, client Client, req Request) {
	assertCompletedResponseText(t, client, req, "ok")
}

func assertCompletedResponseText(t *testing.T, client Client, req Request, want string) {
	t.Helper()
	stream, err := client.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var done EventDone
	var got []Event
	for event := range stream {
		got = append(got, event)
		if got, ok := event.(EventDone); ok {
			done = got
		}
	}
	if done.Err != nil || len(done.Message.Content) != 1 || done.Message.Content[0].(TextBlock).Text != want {
		t.Fatalf("events = %#v; done = %#v", got, done)
	}
}
