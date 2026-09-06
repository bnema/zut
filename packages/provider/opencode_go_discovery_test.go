package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenCodeGoCatalogIsDiscoveredAtRuntime(t *testing.T) {
	for _, model := range Catalog {
		if model.Provider == "opencode-go" {
			t.Fatalf("OpenCode Go model %q is still manually cataloged", model.ID)
		}
	}
}

func TestDiscoverOpenCodeGoCombinesLiveIDsWithModelsDevMetadata(t *testing.T) {
	const apiKey = "synthetic-opencode-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api.json":
			if got := r.Header.Get("authorization"); got != "" {
				t.Errorf("models.dev authorization = %q, want empty", got)
			}
			_, _ = w.Write([]byte(`{
				"opencode-go": {
					"id": "opencode-go",
					"api": "https://opencode.ai/zen/go/v1",
					"models": {
						"muse-spark-1.2-contributor": {
							"id": "muse-spark-1.2-contributor",
							"name": "Muse Spark 1.2 Contributor",
							"reasoning": true,
							"reasoning_options": [{"type":"effort","values":["minimal","low","medium","high","xhigh"]}],
							"limit": {"context": 1048576, "output": 131072},
							"cost": {"input": 0.1, "output": 0.2, "cache_read": 0.002, "cache_write": 0.003,
								"tiers": [{"input": 0.4, "output": 0.8, "cache_read": 0.004, "cache_write": 0.005,
									"tier": {"type": "context", "size": 272000}}]}
						},
						"not-served": {
							"id": "not-served",
							"name": "Not Served",
							"limit": {"context": 999, "output": 99}
						}
					}
				}
			}`))
		case "/v1/models":
			if got := r.Header.Get("authorization"); got != "Bearer "+apiKey {
				t.Errorf("OpenCode Go authorization = %q, want bearer key", got)
			}
			_, _ = w.Write([]byte(`{"object":"list","data":[
				{"id":"muse-spark-1.2-contributor","object":"model"},
				{"id":"future-model","object":"model"}
			]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	models, err := discoverOpenCodeGo(context.Background(), apiKey, server.URL+"/v1", server.URL+"/api.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v, want two served models", models)
	}

	if got := models[0]; got.Provider != "opencode-go" || got.ID != "muse-spark-1.2-contributor" || got.DisplayName != "Muse Spark 1.2 Contributor" || got.ContextWindow != 1048576 || got.MaxOutput != 131072 || !got.Reasoning || got.PriceInput != 0.1 || got.PriceOutput != 0.2 || got.PriceCacheRead != 0.002 || got.PriceCacheWrite != 0.003 || got.PriceTierInputTokens != 272000 || got.PriceInputAbove != 0.4 || got.PriceOutputAbove != 0.8 || got.PriceCacheReadAbove != 0.004 || got.PriceCacheWriteAbove != 0.005 || got.BaseURL != server.URL+"/v1" || got.Source != "live" {
		t.Fatalf("metadata model = %+v", got)
	}
	if got := models[0].ReasoningLevelMap; got["minimum"] != "minimum" || got["xhigh"] != "xhigh" {
		t.Fatalf("reasoning level map = %#v, want minimum/xhigh identity mappings", got)
	}
	if got := models[0].ReasoningEffortMap; got["minimum"] != "minimal" || got["xhigh"] != "xhigh" {
		t.Fatalf("reasoning effort map = %#v, want minimal/xhigh wire values", got)
	}

	if got := models[1]; got.Provider != "opencode-go" || got.ID != "future-model" || got.DisplayName != "future-model" || got.BaseURL != server.URL+"/v1" || got.Source != "live" || got.ContextWindow != 0 || got.MaxOutput != 0 || got.Reasoning {
		t.Fatalf("unmapped live model = %+v", got)
	}
}

func TestDiscoverOpenCodeGoRequiresProviderMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api.json" {
			_, _ = w.Write([]byte(`{"other-provider":{"models":{}}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	if _, err := discoverOpenCodeGo(context.Background(), "key", server.URL, server.URL+"/api.json"); err == nil {
		t.Fatal("missing models.dev provider returned nil error")
	}
}
