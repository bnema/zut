package provider

import (
	"crypto/tls"
	"net/http"
)

// NewHTTPClient returns a provider HTTP client. When insecureTLS is true,
// only this client skips TLS certificate verification. The process-wide
// default transport is left untouched so auth, discovery, and other providers
// keep normal certificate validation.
func NewHTTPClient(insecureTLS bool) *http.Client {
	if !insecureTLS {
		return &http.Client{Timeout: 0}
	}
	tr, ok := http.DefaultTransport.(*http.Transport)
	if ok {
		tr = tr.Clone()
	} else {
		tr = &http.Transport{}
	}
	if tr.TLSClientConfig != nil {
		tr.TLSClientConfig = tr.TLSClientConfig.Clone()
	} else {
		tr.TLSClientConfig = &tls.Config{}
	}
	tr.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec
	return &http.Client{Timeout: 0, Transport: tr}
}

// WithHTTPClient scopes an HTTP client to a concrete provider client.
// Unsupported clients are returned unchanged.
func WithHTTPClient(c Client, httpClient *http.Client) Client {
	if httpClient == nil {
		return c
	}
	switch v := c.(type) {
	case *openaiClient:
		v.http = httpClient
	case *anthropicClient:
		v.http = httpClient
	case *geminiClient:
		v.http = httpClient
	case *bedrockClient:
		v.http = httpClient
	case *codexClient:
		v.http = httpClient
	case *responsesWebSocketClient:
		if v.http.accountID == "" {
			v.http.http = openaiResponsesHTTPClient(httpClient)
		} else {
			v.http.http = httpClient
		}
	case *renamedClient:
		if codex, ok := v.inner.(*codexClient); ok {
			if _, isPublicResponses := codex.http.Transport.(*openaiResponsesTransport); isPublicResponses {
				codex.http = openaiResponsesHTTPClient(httpClient)
				return c
			}
		}
		v.inner = WithHTTPClient(v.inner, httpClient)
	case *modelRouter:
		v.fallback = WithHTTPClient(v.fallback, httpClient)
		for api, inner := range v.byAPI {
			v.byAPI[api] = WithHTTPClient(inner, httpClient)
		}
	}
	return c
}
