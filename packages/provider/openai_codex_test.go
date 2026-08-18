package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/net/websocket"
)

// An image-only tool result must not serialize to an empty
// function_call_output (the Responses API may reject it) and a
// following user-message image must serialize as input_image so the
// model actually receives the bytes.
func TestCodexImageToolResultMirror(t *testing.T) {
	c := NewOpenAICodex("token", "acct", "").(*codexClient)

	wire, err := c.buildRequest(Request{
		Model: "gpt-5.5",
		Messages: []Message{
			{Role: RoleUser, Content: []Content{TextBlock{Text: "look at this"}}},
			{Role: RoleAssistant, Content: []Content{
				ToolCallBlock{ID: "call_1", Name: "read", Arguments: []byte(`{"path":"x.png"}`)},
			}},
			{Role: RoleTool, Content: []Content{
				ToolResultBlock{CallID: "call_1", Content: []Content{
					ImageBlock{MimeType: "image/png", Data: []byte("png-bytes")},
				}},
			}},
			// The agent loop appends this mirror after an image tool result.
			{Role: RoleUser, Content: []Content{
				TextBlock{Text: "Tool output included the following image content:"},
				ImageBlock{MimeType: "image/png", Data: []byte("png-bytes")},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var sawFnOutput, sawInputImage bool
	for _, item := range wire.Input {
		switch v := item.(type) {
		case codexFunctionCallOutput:
			sawFnOutput = true
			if strings.TrimSpace(v.Output) == "" {
				t.Fatalf("image-only tool result produced empty function_call_output")
			}
			if !strings.Contains(strings.ToLower(v.Output), "image") {
				t.Fatalf("placeholder should mention image, got %q", v.Output)
			}
		case codexInputMessage:
			for _, ct := range v.Content {
				if img, ok := ct.(codexInputImage); ok && img.Type == "input_image" {
					sawInputImage = true
				}
			}
		}
	}
	if !sawFnOutput {
		t.Fatalf("no function_call_output emitted")
	}
	if !sawInputImage {
		t.Fatalf("mirrored user image was not serialized as input_image")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCodexPreviewModelUsesCodexCLIShape(t *testing.T) {
	c := NewOpenAICodex("token", "acct", "https://example.test/backend-api/codex/responses").(*codexClient)
	var gotReq *http.Request
	var gotBody codexRequest
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotReq = r
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})

	events, err := c.Stream(context.Background(), Request{
		Model:    "gpt-5.6-terra",
		Context:  RequestContext{CacheSessionID: "cache-root-1", ThreadID: "thread-1", TurnID: "turn-1"},
		Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}

	if gotReq == nil {
		t.Fatal("request was not sent")
	}
	if gotReq.Header.Get("originator") != "codex_cli_rs" {
		t.Fatalf("originator = %q", gotReq.Header.Get("originator"))
	}
	if gotReq.Header.Get("user-agent") != "codex_cli_rs/0.0.0" {
		t.Fatalf("user-agent = %q", gotReq.Header.Get("user-agent"))
	}
	if gotBody.PromptCacheKey != "cache-root-1" {
		t.Fatalf("prompt_cache_key = %q, want root cache ID", gotBody.PromptCacheKey)
	}
	if gotReq.Header.Get("session-id") != "cache-root-1" || gotReq.Header.Get("thread-id") != "thread-1" || gotReq.Header.Get("x-client-request-id") != "thread-1" {
		t.Fatalf("routing headers = session=%q thread=%q request=%q", gotReq.Header.Get("session-id"), gotReq.Header.Get("thread-id"), gotReq.Header.Get("x-client-request-id"))
	}
}

func TestGPT56UsesNativeMaxReasoningEffort(t *testing.T) {
	c := NewOpenAICodex("token", "acct", "").(*codexClient)
	wire, err := c.buildRequest(Request{Model: "gpt-5.6-sol", Reasoning: "max"})
	if err != nil {
		t.Fatal(err)
	}
	if wire.Reasoning == nil || wire.Reasoning.Effort != "max" {
		t.Fatalf("reasoning = %+v", wire.Reasoning)
	}
}

func TestResponsesRequestUsesExplicitReasoningLevelMap(t *testing.T) {
	SetLiveModels([]Model{{
		Provider:          "custom-responses",
		ID:                "reasoning-model",
		API:               APIResponses,
		Reasoning:         true,
		ReasoningLevelMap: map[string]string{"max": "max"},
	}})
	t.Cleanup(func() { SetLiveModels(nil) })

	named := NewOpenAIResponsesNamed("token", "https://example.test/v1", "custom-responses").(*renamedClient)
	c := named.inner.(*codexClient)
	wire, err := c.buildRequest(Request{Model: "reasoning-model", Reasoning: "max"})
	if err != nil {
		t.Fatal(err)
	}
	if wire.Reasoning == nil || wire.Reasoning.Effort != "max" {
		t.Fatalf("reasoning = %+v", wire.Reasoning)
	}
}

func TestXAIResponsesUsesNamedProviderCatalogAndEndpoint(t *testing.T) {
	named := NewOpenAIResponsesNamed("token", "https://api.x.ai/v1", "xai").(*renamedClient)
	c := named.inner.(*codexClient)

	if c.baseURL != "https://api.x.ai/v1/responses" {
		t.Fatalf("base URL = %q", c.baseURL)
	}
	for _, model := range []string{"grok-4.5", "grok-build-0.1"} {
		if _, err := c.buildRequest(Request{Model: model}); err != nil {
			t.Errorf("build request for %s: %v", model, err)
		}
	}
}

func TestOpenAIGPT56DoesNotUseCodexCLIRouting(t *testing.T) {
	named := NewOpenAIResponsesNamed("token", "https://api.openai.com/v1/responses", "openai").(*renamedClient)
	ws := named.inner.(*responsesWebSocketClient)
	c := ws.http
	var gotReq *http.Request
	var gotBody codexRequest
	ws.dial = func(context.Context) (*websocket.Conn, error) {
		return nil, errors.New("test websocket unavailable")
	}
	c.http = openaiResponsesHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotReq = r
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})})

	events, err := ws.Stream(context.Background(), Request{
		Model:    "gpt-5.6-sol",
		Context:  RequestContext{CacheSessionID: "cache-root-2", ThreadID: "thread-2", TurnID: "turn-2"},
		Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}

	if gotReq == nil {
		t.Fatal("request was not sent")
	}
	if gotReq.Header.Get("session-id") != "" {
		t.Fatalf("session-id = %q", gotReq.Header.Get("session-id"))
	}
	if gotBody.PromptCacheKey != "cache-root-2" {
		t.Fatalf("prompt_cache_key = %q, want root cache ID", gotBody.PromptCacheKey)
	}
	if gotBody.PromptCacheOptions == nil || gotBody.PromptCacheOptions.Mode != "implicit" || len(gotBody.Input) < 2 {
		t.Fatalf("explicit cache boundary = options=%+v input=%#v", gotBody.PromptCacheOptions, gotBody.Input)
	}
	boundary, ok := gotBody.Input[0].(map[string]any)
	if !ok || boundary["role"] != "developer" {
		t.Fatalf("stable cache boundary = %#v", gotBody.Input[0])
	}
	breakpoint, ok := boundary["prompt_cache_breakpoint"].(map[string]any)
	if !ok || breakpoint["type"] != "stable" {
		t.Fatalf("stable cache breakpoint = %#v", boundary)
	}
	if gotReq.Header.Get("originator") != "" || gotReq.Header.Get("user-agent") != "" || gotReq.Header.Get("chatgpt-account-id") != "" || gotReq.Header.Get("openai-beta") != "" {
		t.Fatalf("public fallback leaked Codex headers: %v", gotReq.Header)
	}
}

func TestCustomResponsesEndpointDoesNotInheritOpenAICacheExtensions(t *testing.T) {
	named := NewOpenAIResponsesNamed("token", "https://example.test/v1/responses", "openai").(*renamedClient)
	c := named.inner.(*codexClient)
	var gotBody codexRequest
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	events, err := c.Stream(context.Background(), Request{Model: "gpt-5.6-sol", Context: RequestContext{CacheSessionID: "cache-root", ThreadID: "thread", TurnID: "turn"}})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if gotBody.PromptCacheKey != "" {
		t.Fatalf("prompt_cache_key = %q, want omitted for undeclared endpoint", gotBody.PromptCacheKey)
	}
}

func TestCodexNestedStreamError(t *testing.T) {
	c := NewOpenAICodex("token", "acct", "").(*codexClient)
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader("data: {\"type\":\"error\",\"error\":{\"code\":\"model_not_available\",\"message\":\"limited preview\"}}\n\n")),
	}
	out := make(chan Event, 16)
	go c.runStream(context.Background(), resp, Request{Model: "gpt-5.6-sol"}, out)

	var got error
	for ev := range out {
		if done, ok := ev.(EventDone); ok {
			got = done.Err
		}
	}
	if got == nil || got.Error() != "codex error: limited preview" {
		t.Fatalf("error = %v", got)
	}
}

// TestCodexSubscriptionAlwaysUsesCodexCLIShape pins the ChatGPT
// subscription backend to the Codex CLI request identity for every
// model. The backend load-sheds unrecognized originator/user-agent
// pairs with "Our servers are currently overloaded" stream errors
// even when capacity is fine, so the CLI shape is required for
// reliable service, not just for preview-model admission.
func TestCodexSubscriptionAlwaysUsesCodexCLIShape(t *testing.T) {
	c := NewOpenAICodex("token", "acct", "https://example.test/backend-api/codex/responses").(*codexClient)
	var gotReq *http.Request
	var body bytes.Buffer
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotReq = r
		_, _ = body.ReadFrom(r.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})

	events, err := c.Stream(context.Background(), Request{
		Model:    "gpt-5.6-sol",
		Context:  RequestContext{CacheSessionID: "cache-root-2", ThreadID: "thread-2", TurnID: "turn-2"},
		Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}

	if gotReq == nil {
		t.Fatal("request was not sent")
	}
	if gotReq.Header.Get("originator") != "codex_cli_rs" {
		t.Fatalf("originator = %q", gotReq.Header.Get("originator"))
	}
	if gotReq.Header.Get("user-agent") != "codex_cli_rs/0.0.0" {
		t.Fatalf("user-agent = %q", gotReq.Header.Get("user-agent"))
	}
	if gotReq.Header.Get("session-id") != "cache-root-2" || gotReq.Header.Get("thread-id") != "thread-2" || gotReq.Header.Get("x-client-request-id") != "thread-2" {
		t.Fatalf("routing headers = session=%q thread=%q request=%q", gotReq.Header.Get("session-id"), gotReq.Header.Get("thread-id"), gotReq.Header.Get("x-client-request-id"))
	}
	if !strings.Contains(body.String(), "prompt_cache_key") {
		t.Fatalf("request missing prompt_cache_key: %s", body.String())
	}
}
