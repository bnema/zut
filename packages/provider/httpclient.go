package provider

import (
	"net/http"
)

// NewHTTPClient returns a provider HTTP client. When insecureTLS is true,
// only this client skips TLS certificate verification. The process-wide
// default transport is left untouched so auth, discovery, and other providers
// keep normal certificate validation.
func NewHTTPClient(insecureTLS bool) *http.Client {
	if !insecureTLS {
		return newDefaultHTTPClient()
	}
	return &http.Client{Transport: NewTransport(TransportOptions{InsecureTLS: true})}
}

// WithHTTPClient scopes an HTTP client to a concrete provider client.
// Unsupported clients are returned unchanged.
func WithHTTPClient(c Client, httpClient *http.Client) Client {
	if httpClient == nil {
		return c
	}
	switch v := c.(type) {
	case *openaiClient:
		v.http = providerHTTPClient(v.http, httpClient)
	case *anthropicClient:
		v.http = providerHTTPClient(v.http, httpClient)
	case *geminiClient:
		v.http = providerHTTPClient(v.http, httpClient)
	case *bedrockClient:
		v.http = providerHTTPClient(v.http, httpClient)
	case *codexClient:
		v.http = providerHTTPClient(v.http, httpClient)
	case *responsesWebSocketClient:
		v.http.http = providerHTTPClient(v.http.http, httpClient)
	case *renamedClient:
		v.inner = WithHTTPClient(v.inner, httpClient)
	case *copilotClient:
		WithHTTPClient(v.router, httpClient)
	case *modelRouter:
		v.fallback = WithHTTPClient(v.fallback, httpClient)
		for api, inner := range v.byAPI {
			v.byAPI[api] = WithHTTPClient(inner, httpClient)
		}
	}
	return c
}

// providerHTTPClient retains provider wire behavior while replacing the network
// transport. Clone wrappers rather than mutating transports shared by routes.
func providerHTTPClient(previous, replacement *http.Client) *http.Client {
	if previous == nil {
		return replacement
	}
	inner := replacement.Transport
	if inner == nil {
		inner = sharedTransport()
	}
	transport, wrapped := replaceProviderTransport(previous.Transport, inner)
	if !wrapped {
		return replacement
	}
	clone := *replacement
	clone.Transport = transport
	return &clone
}

func replaceProviderTransport(existing, replacement http.RoundTripper) (http.RoundTripper, bool) {
	switch tr := existing.(type) {
	case *azureRewriteTransport:
		clone := *tr
		clone.inner, _ = replaceProviderTransport(tr.inner, replacement)
		return &clone, true
	case *azureResponsesTransport:
		clone := *tr
		clone.inner, _ = replaceProviderTransport(tr.inner, replacement)
		return &clone, true
	case *vertexTransport:
		clone := *tr
		clone.inner, _ = replaceProviderTransport(tr.inner, replacement)
		return &clone, true
	case *copilotRefreshTransport:
		clone := *tr
		clone.inner, _ = replaceProviderTransport(tr.inner, replacement)
		return &clone, true
	case *copilotResponsesStripTransport:
		clone := *tr
		clone.inner, _ = replaceProviderTransport(tr.inner, replacement)
		return &clone, true
	case *openaiResponsesTransport:
		clone := *tr
		clone.inner, _ = replaceProviderTransport(tr.inner, replacement)
		return &clone, true
	default:
		return replacement, false
	}
}
