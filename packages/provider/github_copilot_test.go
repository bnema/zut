package provider

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"
)

type mockRoundTripper struct {
	lastReq *http.Request
}

func (m *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	m.lastReq = req
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       http.NoBody,
	}, nil
}

func TestCopilotHostRewrite(t *testing.T) {
	// Seed the cache with a mock token that carries a non-default proxy endpoint
	// Ensure the expiresAt time is far in the future so that it bypasses the exchange check
	const pat = "ghp_mock_token_for_test"
	copilotCache.mu.Lock()
	copilotCache.tokens[pat] = copilotToken{
		value:     "mock-jwt-with;proxy-ep=enterprise.copilot.proxy",
		expiresAt: time.Now().Add(1 * time.Hour),
		baseURL:   "https://api.enterprise.copilot.proxy",
	}
	copilotCache.mu.Unlock()
	t.Cleanup(func() {
		copilotCache.mu.Lock()
		delete(copilotCache.tokens, pat)
		copilotCache.mu.Unlock()
	})

	mockRT := &mockRoundTripper{}
	transport := &copilotRefreshTransport{
		inner: mockRT,
		pat:   pat,
	}

	reqUrl, err := url.Parse("https://api.individual.githubcopilot.com/chat/completions")
	if err != nil {
		t.Fatalf("failed to parse test URL: %v", err)
	}

	req := &http.Request{
		Method: "POST",
		URL:    reqUrl,
		Header: make(http.Header),
	}
	req = req.WithContext(context.Background())

	_, err = transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}

	if mockRT.lastReq == nil {
		t.Fatal("RoundTrip did not forward the request")
	}

	// Verify both URL host and Host header are rewritten correctly to prevent 421 Misdirected Request
	wantHost := "api.enterprise.copilot.proxy"
	if mockRT.lastReq.URL.Host != wantHost {
		t.Errorf("expected URL.Host to be %q, got %q", wantHost, mockRT.lastReq.URL.Host)
	}
	if mockRT.lastReq.Host != wantHost {
		t.Errorf("expected Host header (Request.Host) to be %q, got %q", wantHost, mockRT.lastReq.Host)
	}
}

func TestCopilotRoutesModernGPTToResponses(t *testing.T) {
	client, ok := NewGithubCopilotClient("pat").(*copilotClient)
	if !ok {
		t.Fatal("NewGithubCopilotClient did not return a copilotClient")
	}
	codex, ok := client.router.byAPI[APIResponses].(*codexClient)
	if !ok {
		t.Fatal("github-copilot Responses client is not a codexClient")
	}
	const want = "https://api.individual.githubcopilot.com/responses"
	if codex.baseURL != want {
		t.Errorf("github-copilot Responses baseURL = %q, want %q", codex.baseURL, want)
	}
	for _, id := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-astra"} {
		m, err := FindModel("github-copilot", id)
		if err != nil {
			t.Fatalf("FindModel(%q): %v", id, err)
		}
		if m.API != APIResponses {
			t.Errorf("model %q API = %q, want %q", id, m.API, APIResponses)
		}
	}
}
