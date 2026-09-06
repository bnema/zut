package sdk

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
)

func testCredentialEndpointScope(credential, baseURL string) string {
	endpoint := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	digest := sha256.Sum256([]byte(credential + "\x00" + endpoint))
	return fmt.Sprintf("sha256:%x", digest)
}

func TestNewLoadsCachedOpenCodeGoMetadataBeforeResolving(t *testing.T) {
	const (
		apiKey  = "synthetic-opencode-key"
		modelID = "cached-opencode-model"
	)
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	provider.SetLiveModels(nil)
	provider.SetUserModels(nil)
	t.Cleanup(func() {
		provider.SetLiveModels(nil)
		provider.SetUserModels(nil)
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("authorization"); got != "Bearer "+apiKey {
			t.Errorf("authorization = %q, want bearer API key", got)
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
		if request.Model != modelID {
			t.Errorf("request model = %q, want %q", request.Model, modelID)
		}
		if request.ReasoningEffort != "minimal" {
			t.Errorf("request reasoning_effort = %q, want minimal", request.ReasoningEffort)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, "data: [DONE]")
	}))
	defer server.Close()

	cached := provider.Model{
		Provider:           provider.ProviderOpenCodeGo,
		ID:                 modelID,
		DisplayName:        "Cached OpenCode model",
		Reasoning:          true,
		ReasoningLevelMap:  map[string]string{"minimum": "minimum", "low": "", "medium": "", "high": "", "xhigh": "", "max": ""},
		ReasoningEffortMap: map[string]string{"minimum": "minimal"},
		ContextWindow:      128000,
		MaxOutput:          8192,
		BaseURL:            server.URL + "/v1",
		Source:             "live",
	}
	if err := provider.SaveCache(filepath.Join(home, "models-cache.json"), provider.ModelCache{
		Version:                provider.ModelCacheVersion,
		FetchedAt:              time.Now().UTC(),
		Models:                 []provider.Model{cached},
		AuthoritativeProviders: []string{provider.ProviderOpenCodeGo},
		ProviderScopes:         map[string]string{provider.ProviderOpenCodeGo: testCredentialEndpointScope(apiKey, server.URL+"/v1")},
	}); err != nil {
		t.Fatal(err)
	}

	runtime, err := New(Config{
		Provider:  provider.ProviderOpenCodeGo,
		Model:     "  " + modelID + "  ",
		APIKey:    apiKey,
		BaseURL:   server.URL + "/v1",
		Reasoning: "minimum",
		CWD:       t.TempDir(),
		NoTools:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	models := runtime.ListModels()
	var found bool
	for _, model := range models {
		if model.ID == modelID {
			found = model.ContextWindow == 128000 && model.MaxOutput == 8192 && model.Reasoning
		}
	}
	if !found {
		t.Fatalf("SDK did not load cached OpenCode Go metadata: %+v", models)
	}
	if err := runtime.SetModel("  " + modelID + "  "); err != nil {
		t.Fatal(err)
	}
	if runtime.Model() != modelID {
		t.Fatalf("SDK model = %q, want trimmed id", runtime.Model())
	}

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
