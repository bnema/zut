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
	"strings"
)

const openaiResponsesDefaultBaseURL = "https://api.openai.com/v1/responses"

// openaiResponsesTransport rewrites headers so the codex client (which
// is designed for chatgpt.com OAuth tokens) can target the public
// OpenAI Responses API with a normal API key.
type openaiResponsesTransport struct {
	inner http.RoundTripper
}

func (t *openaiResponsesTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	// The codex client always sets chatgpt-account-id; api.openai.com
	// doesn't need it and may reject the OAuth identity headers, so
	// strip them.
	clone.Header.Del("chatgpt-account-id")
	clone.Header.Del("openai-beta")
	clone.Header.Del("originator")
	// The shared Codex client sets a Bearer header even with an empty key.
	// Keyless custom Responses endpoints need no Authorization header at all.
	if strings.TrimSpace(clone.Header.Get("authorization")) == "Bearer" {
		clone.Header.Del("authorization")
	}
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
	httpClient := &http.Client{
		Transport: &openaiResponsesTransport{inner: http.DefaultTransport},
		Timeout:   0,
	}
	inner := &codexClient{
		token:        apiKey,
		accountID:    "", // unused; transport strips the header
		baseURL:      strings.TrimRight(baseURL, "/"),
		errorLabel:   "openai",
		providerName: name,
		http:         httpClient,
	}
	return &renamedClient{inner: inner, name: name}
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
