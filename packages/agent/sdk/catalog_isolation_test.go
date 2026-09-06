package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
)

func TestSDKRuntimeKeepsOpenCodeGoMetadataPerEndpoint(t *testing.T) {
	const modelID = "scoped-opencode-model"
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	provider.SetLiveModels(nil)
	provider.SetUserModels(nil)
	t.Cleanup(func() {
		provider.SetLiveModels(nil)
		provider.SetUserModels(nil)
	})

	newServer := func(apiKey, wantEffort string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/chat/completions" {
				http.NotFound(w, r)
				return
			}
			if got := r.Header.Get("authorization"); got != "Bearer "+apiKey {
				t.Errorf("authorization = %q, want bearer key %q", got, apiKey)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read request: %v", err)
			}
			var request struct {
				Model           string `json:"model"`
				ReasoningEffort string `json:"reasoning_effort"`
			}
			if err := json.Unmarshal(body, &request); err != nil {
				t.Errorf("decode request: %v", err)
			}
			if request.Model != modelID || request.ReasoningEffort != wantEffort {
				t.Errorf("request = model %q effort %q, want model %q effort %q", request.Model, request.ReasoningEffort, modelID, wantEffort)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			_, _ = fmt.Fprintln(w)
			_, _ = fmt.Fprintln(w, "data: [DONE]")
		}))
	}
	serverA := newServer("key-a", "minimal")
	defer serverA.Close()
	serverB := newServer("key-b", "")
	defer serverB.Close()

	saveCache := func(server *httptest.Server, apiKey string, reasoning bool, effort string) {
		t.Helper()
		model := provider.Model{
			Provider:      provider.ProviderOpenCodeGo,
			ID:            modelID,
			Reasoning:     reasoning,
			ContextWindow: 128000,
			MaxOutput:     8192,
			PriceInput:    100,
			PriceOutput:   200,
			BaseURL:       server.URL + "/v1",
		}
		if reasoning {
			model.PriceInput = 10
			model.PriceOutput = 20
		}
		if reasoning {
			model.ReasoningLevelMap = map[string]string{"minimum": "minimum", "low": "", "medium": "", "high": "", "xhigh": "", "max": ""}
			model.ReasoningEffortMap = map[string]string{"minimum": effort}
		}
		if err := provider.SaveCache(filepath.Join(home, "models-cache.json"), provider.ModelCache{
			Version:                provider.ModelCacheVersion,
			FetchedAt:              time.Now().UTC(),
			Models:                 []provider.Model{model},
			AuthoritativeProviders: []string{provider.ProviderOpenCodeGo},
			ProviderScopes:         map[string]string{provider.ProviderOpenCodeGo: testCredentialEndpointScope(apiKey, server.URL+"/v1")},
		}); err != nil {
			t.Fatal(err)
		}
	}

	saveCache(serverA, "key-a", true, "minimal")
	runtimeA, err := New(Config{
		Provider:  provider.ProviderOpenCodeGo,
		Model:     modelID,
		APIKey:    "key-a",
		BaseURL:   serverA.URL + "/v1",
		Reasoning: "minimum",
		CWD:       t.TempDir(),
		NoTools:   true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Replace the disk cache and process-global catalog with another scope.
	// Runtime A must continue using its own endpoint metadata afterward.
	saveCache(serverB, "key-b", false, "")
	runtimeB, err := New(Config{
		Provider:  provider.ProviderOpenCodeGo,
		Model:     modelID,
		APIKey:    "key-b",
		BaseURL:   serverB.URL + "/v1",
		Reasoning: "minimum",
		CWD:       t.TempDir(),
		NoTools:   true,
	})
	if err != nil {
		runtimeA.Close()
		t.Fatal(err)
	}

	prompt := func(runtime *Runtime) {
		t.Helper()
		events, err := runtime.Prompt(context.Background(), "hello", nil)
		if err != nil {
			t.Fatal(err)
		}
		for event := range events {
			if event.Type == "error" {
				t.Fatalf("prompt error: %s", event.Error)
			}
		}
	}
	prompt(runtimeB)
	prompt(runtimeA)

	if got, want := runtimeB.Cost().CostUSD, 0.0003; math.Abs(got-want) > 1e-12 {
		t.Fatalf("runtime B cost = %v, want %v", got, want)
	}
	if got, want := runtimeA.Cost().CostUSD, 0.00003; math.Abs(got-want) > 1e-12 {
		t.Fatalf("runtime A cost = %v, want %v", got, want)
	}
	if err := runtimeB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runtimeA.Close(); err != nil {
		t.Fatal(err)
	}
}
