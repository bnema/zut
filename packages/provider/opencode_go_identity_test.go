package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestOpenCodeGoRetriesKeepRequestIdentityStable(t *testing.T) {
	const apiKey = "synthetic-key"
	models := []Model{
		{Provider: ProviderOpenCodeGo, ID: "retry-responses", API: APIResponses, MaxOutput: 1024},
		{Provider: ProviderOpenCodeGo, ID: "retry-messages", API: APIAnthropicMessages, MaxOutput: 1024},
		{Provider: ProviderOpenCodeGo, ID: "retry-chat", API: APICompletions, MaxOutput: 1024},
	}
	var (
		mu    sync.Mutex
		calls = make(map[string]int)
		seen  = make(map[string][]http.Header)
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/responses", "/v1/messages", "/v1/chat/completions":
		default:
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		calls[r.URL.Path]++
		attempt := calls[r.URL.Path]
		seen[r.URL.Path] = append(seen[r.URL.Path], r.Header.Clone())
		mu.Unlock()
		if attempt == 1 {
			http.Error(w, "temporary gateway failure", http.StatusInternalServerError)
			return
		}
		writeTextResponse(w, r.URL.Path)
	}))
	defer server.Close()

	client := NewOpenCodeGoWithModels(apiKey, server.URL+"/v1", models)
	for _, model := range models {
		req := Request{
			Model: model.ID,
			Context: RequestContext{
				CacheSessionID: "cache-root",
				ThreadID:       "thread-stable",
				TurnID:         "turn-stable",
			},
			Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hello"}}}},
		}
		if _, err := consumeTextStream(client, req); err != nil {
			t.Fatalf("%s: %v", model.ID, err)
		}
	}

	for _, model := range models {
		path := map[string]string{
			APIResponses:         "/v1/responses",
			APIAnthropicMessages: "/v1/messages",
			APICompletions:       "/v1/chat/completions",
		}[model.API]
		mu.Lock()
		got := append([]http.Header(nil), seen[path]...)
		mu.Unlock()
		if len(got) != 2 {
			t.Fatalf("%s attempts = %d, want initial failure and retry", path, len(got))
		}
		for i, headers := range got {
			if headers.Get("x-opencode-client") != "zut" || headers.Get("user-agent") != openCodeGoUserAgent {
				t.Fatalf("%s attempt %d identity = client %q user-agent %q", path, i+1, headers.Get("x-opencode-client"), headers.Get("user-agent"))
			}
			if headers.Get("x-opencode-session") != "thread-stable" || headers.Get("x-opencode-request") != "turn-stable" {
				t.Fatalf("%s attempt %d correlation = session %q request %q", path, i+1, headers.Get("x-opencode-session"), headers.Get("x-opencode-request"))
			}
		}
	}
}

func TestOpenCodeGoHeaderHelperScopesIdentity(t *testing.T) {
	openCodeHeaders := make(http.Header)
	setOpenCodeGoHeaders(openCodeHeaders, ProviderOpenCodeGo, RequestContext{CacheSessionID: "cache", TurnID: "turn"})
	if openCodeHeaders.Get("x-opencode-session") != "cache" || openCodeHeaders.Get("x-opencode-request") != "turn" {
		t.Fatalf("fallback identity = session %q request %q", openCodeHeaders.Get("x-opencode-session"), openCodeHeaders.Get("x-opencode-request"))
	}
	if openCodeHeaders.Get("x-opencode-client") != "zut" || openCodeHeaders.Get("user-agent") != openCodeGoUserAgent {
		t.Fatalf("OpenCode Go identity = %v", openCodeHeaders)
	}

	ordinaryHeaders := make(http.Header)
	setOpenCodeGoHeaders(ordinaryHeaders, ProviderOpenAI, RequestContext{ThreadID: "thread", TurnID: "turn"})
	if ordinaryHeaders.Get("x-opencode-session") != "" || ordinaryHeaders.Get("x-opencode-request") != "" || ordinaryHeaders.Get("x-opencode-client") != "" || ordinaryHeaders.Get("user-agent") != "" {
		t.Fatalf("ordinary provider received OpenCode headers: %v", ordinaryHeaders)
	}

	emptyHeaders := make(http.Header)
	setOpenCodeGoHeaders(emptyHeaders, ProviderOpenCodeGo, RequestContext{})
	if emptyHeaders.Get("x-opencode-session") != "" || emptyHeaders.Get("x-opencode-request") != "" {
		t.Fatalf("empty context got correlation headers: %v", emptyHeaders)
	}
	if emptyHeaders.Get("x-opencode-client") != "zut" || emptyHeaders.Get("user-agent") != openCodeGoUserAgent {
		t.Fatalf("empty context lost product identity: %v", emptyHeaders)
	}

	firstSession := make(http.Header)
	secondSession := make(http.Header)
	setOpenCodeGoHeaders(firstSession, ProviderOpenCodeGo, RequestContext{ThreadID: "thread-a"})
	setOpenCodeGoHeaders(secondSession, ProviderOpenCodeGo, RequestContext{ThreadID: "thread-b"})
	if firstSession.Get("x-opencode-session") == secondSession.Get("x-opencode-session") {
		t.Fatalf("distinct conversations shared session identity %q", firstSession.Get("x-opencode-session"))
	}
}

func TestOpenCodeGoResponsesRetainsIdentityThroughPublicTransport(t *testing.T) {
	named := NewOpenAIResponsesNamed("synthetic-key", "https://example.test/v1", ProviderOpenCodeGo).(*renamedClient)
	client := named.inner.(*codexClient)
	client.SetModelMetadata(Model{Provider: ProviderOpenCodeGo, ID: "responses-model", MaxOutput: 1024})

	var got *http.Request
	client.http = openaiResponsesHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})})

	stream, err := client.Stream(context.Background(), Request{
		Model:   "responses-model",
		Context: RequestContext{ThreadID: "thread", TurnID: "turn"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if got == nil {
		t.Fatal("request was not sent")
	}
	if got.Header.Get("x-opencode-client") != "zut" || got.Header.Get("user-agent") != openCodeGoUserAgent {
		t.Fatalf("identity stripped by public Responses transport: %v", got.Header)
	}
	if got.Header.Get("x-opencode-session") != "thread" || got.Header.Get("x-opencode-request") != "turn" {
		t.Fatalf("correlation stripped by public Responses transport: %v", got.Header)
	}
	if got.Header.Get("chatgpt-account-id") != "" || got.Header.Get("originator") != "" || got.Header.Get("openai-beta") != "" {
		t.Fatalf("Codex identity leaked through public Responses transport: %v", got.Header)
	}
}
