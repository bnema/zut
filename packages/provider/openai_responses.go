package provider

// OpenAI Responses API client (api.openai.com/v1/responses).
//
// Strategy: reuse the existing codexClient (which already speaks the
// Responses wire format) via a header-rewriting RoundTripper. The codex
// client always sends `chatgpt-account-id` and `openai-beta`; the public
// Responses API on api.openai.com accepts but doesn't require those —
// we strip the account-id header on its way out so the request looks
// like a normal authenticated OpenAI call.
//
// This is a separate provider from `openai` (which is Chat Completions);
// users opt in by passing `--provider openai-responses` or by picking a
// model whose catalog entry tags it under that provider id.
//
// Auth: API key via Authorization: Bearer (set by codexClient itself).
// Different from openai-codex, which uses OAuth subscription tokens.

import (
	"net/http"
	"net/url"
	"strings"
)

const openaiResponsesDefaultBaseURL = "https://api.openai.com/v1/responses"

// openaiResponsesTransport rewrites headers so the codex client (which
// is designed for chatgpt.com OAuth tokens) can target the public
// OpenAI Responses API with a normal API key.
type openaiResponsesTransport struct {
	inner http.RoundTripper
}

// openaiResponsesHTTPClient preserves the public Responses header boundary
// when a caller injects a transport, for example to scope insecure TLS.
func openaiResponsesHTTPClient(client *http.Client) *http.Client {
	clone := *client
	inner := clone.Transport
	if inner == nil {
		inner = http.DefaultTransport
	}
	clone.Transport = &openaiResponsesTransport{inner: inner}
	return &clone
}

func (t *openaiResponsesTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	// The codex client always sets chatgpt-account-id; api.openai.com
	// doesn't need it and may reject the OAuth identity headers, so
	// strip them.
	clone.Header.Del("chatgpt-account-id")
	clone.Header.Del("openai-beta")
	clone.Header.Del("originator")
	clone.Header.Del("user-agent")
	// Keep Authorization: Bearer <key> as set by the codex client.
	return t.inner.RoundTrip(clone)
}

// NewOpenAIResponses returns an OpenAI Responses-API client (API-key
// flow). Uses the same wire format as the ChatGPT Codex backend but
// with the public api.openai.com endpoint and standard Bearer auth.
//
// baseURL may be empty; defaults to https://api.openai.com/v1/responses.
func NewOpenAIResponses(apiKey, baseURL string) Client {
	return NewOpenAIResponsesNamed(apiKey, baseURL, "openai-responses")
}

// NewOpenAIResponsesNamed returns a public OpenAI Responses-API client
// reporting the supplied provider name. This lets the `openai` provider
// route Responses-only preview models without changing the visible provider.
func NewOpenAIResponsesNamed(apiKey, baseURL, name string) Client {
	baseURL = responsesURL(baseURL)
	httpClient := openaiResponsesHTTPClient(&http.Client{Timeout: 0})
	inner := &codexClient{
		token:        apiKey,
		accountID:    "", // unused; transport strips the header
		baseURL:      strings.TrimRight(baseURL, "/"),
		errorLabel:   "openai",
		providerName: name,
		capabilities: responsesCapabilities{
			// Only the official OpenAI provider declares this extension.
			// A Responses-shaped custom endpoint must opt in through its
			// own client rather than inheriting OpenAI's cache contract.
			StablePromptCacheKey: officialOpenAIResponsesEndpoint(name, baseURL),
		},
		http: httpClient,
	}
	var client Client = inner
	if officialOpenAIResponsesEndpoint(name, baseURL) {
		client = newResponsesWebSocketClient(inner)
	}
	return &renamedClient{inner: client, name: name}
}

func officialOpenAIResponsesEndpoint(name, endpoint string) bool {
	if name != "openai" && name != "openai-responses" {
		return false
	}
	u, err := url.Parse(endpoint)
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), "api.openai.com") && u.Path == "/v1/responses"
}

// responsesURL accepts either an API root or a complete Responses endpoint.
// Catalog model entries generally carry API roots, while explicit overrides
// may already include /responses.
func responsesURL(baseURL string) string {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return openaiResponsesDefaultBaseURL
	}
	if strings.HasSuffix(baseURL, "/responses") {
		return baseURL
	}
	if versionSegmentSuffix.MatchString(baseURL) {
		return baseURL + "/responses"
	}
	return baseURL + "/v1/responses"
}
