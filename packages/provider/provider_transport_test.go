package provider

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"
)

// providerHTTPClients visits every wire client, including all router branches.
func providerHTTPClients(t *testing.T, client Client) []*http.Client {
	t.Helper()
	switch c := client.(type) {
	case *openaiClient:
		return []*http.Client{c.http}
	case *anthropicClient:
		return []*http.Client{c.http}
	case *geminiClient:
		return []*http.Client{c.http}
	case *bedrockClient:
		return []*http.Client{c.http}
	case *codexClient:
		return []*http.Client{c.http}
	case *responsesWebSocketClient:
		return []*http.Client{c.http.http}
	case *renamedClient:
		return providerHTTPClients(t, c.inner)
	case *copilotClient:
		return providerHTTPClients(t, c.router)
	case *modelRouter:
		clients := providerHTTPClients(t, c.fallback)
		for _, branch := range c.byAPI {
			clients = append(clients, providerHTTPClients(t, branch)...)
		}
		return clients
	default:
		t.Fatalf("unhandled client %T", client)
		return nil
	}
}

func transportChain(tr http.RoundTripper) ([]string, http.RoundTripper) {
	var wrappers []string
	for {
		wrappers = append(wrappers, fmt.Sprintf("%T", tr))
		switch v := tr.(type) {
		case *azureRewriteTransport:
			tr = v.inner
		case *azureResponsesTransport:
			tr = v.inner
		case *vertexTransport:
			tr = v.inner
		case *copilotRefreshTransport:
			tr = v.inner
		case *copilotResponsesStripTransport:
			tr = v.inner
		case *openaiResponsesTransport:
			tr = v.inner
		default:
			return wrappers, tr
		}
	}
}

func TestProviderTransports(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "test-project")
	t.Setenv("GOOGLE_CLOUD_API_KEY", "test-key")
	const endpoint = "https://example.test"
	tests := []struct {
		name   string
		client Client
	}{
		{"anthropic", NewAnthropic("key", "")},
		{"anthropic-oauth", NewAnthropicOAuth("token", "")},
		{"openai", NewOpenAI("key", "")},
		{"openai-oauth", NewOpenAIOAuth("token", "")},
		{"kimi", NewKimi("key", "")},
		{"deepseek", NewDeepSeek("key", "")},
		{"gemini", NewGemini("key", "")},
		{"bedrock", NewBedrockClient("key", "")},
		{"codex", NewOpenAICodex("token", "account", "")},
		{"responses", NewOpenAIResponses("key", "")},
		{"custom-responses", NewOpenAIResponses("key", endpoint)},
		{"compat", NewOpenAICompat("compat", "key", endpoint, endpoint)},
		{"anthropic-compat", NewAnthropicCompat("compat", "key", endpoint)},
		{"kimi-coding", NewKimiCoding("key", endpoint)},
		{"cloudflare", NewCloudflareAIGateway("key", endpoint)},
		{"azure", NewAzureOpenAI("key", endpoint)},
		{"azure-responses", NewAzureOpenAIResponses("key", endpoint)},
		{"vertex", NewVertex("", "")},
		{"copilot", NewGithubCopilotClient("pat")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := providerHTTPClients(t, tt.client)
			chains := make([][]string, len(before))
			// Router maps have no stable traversal order; compare wrapper chains as a multiset.
			counts := make(map[string]int)
			for i, client := range before {
				chain, inner := transportChain(client.Transport)
				chains[i] = chain
				counts[fmt.Sprint(chain)]++
				if inner != sharedTransport() {
					t.Fatalf("%v ends in %T, not shared pool", chain, inner)
				}
			}
			insecure := NewHTTPClient(true)
			defer insecure.CloseIdleConnections()
			WithHTTPClient(tt.client, insecure)
			after := providerHTTPClients(t, tt.client)
			if len(after) != len(before) {
				t.Fatal("wire client count changed")
			}
			for _, client := range after {
				chain, inner := transportChain(client.Transport)
				counts[fmt.Sprint(chain)]--
				if inner != insecure.Transport {
					t.Fatalf("%v did not receive insecure transport", chain)
				}
			}
			for chain, count := range counts {
				if count != 0 {
					t.Fatalf("wrapper chain changed: %s (%d)", chain, count)
				}
			}
			for i, client := range before {
				chain, inner := transportChain(client.Transport)
				if inner != sharedTransport() || !reflect.DeepEqual(chain, chains[i]) {
					t.Fatal("original wrapper mutated")
				}
			}
		})
	}
}

func TestAuxiliaryClientTransports(t *testing.T) {
	llama, err := NewLlamaCPPClient("http://127.0.0.1:8080", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, client := range []*http.Client{llama.HTTP, NewHuggingFaceClient("").HTTP} {
		if client.Transport != sharedTransport() || client.Timeout != llamaCPPRequestTimeout {
			t.Fatal("auxiliary client transport or timeout changed")
		}
	}
	if vertexCache.http.Transport != sharedTransport() {
		t.Fatal("Vertex token client does not share pool")
	}
}
