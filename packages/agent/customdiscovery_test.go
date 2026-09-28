package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/provider/auth"
)

// loadTestModelsJSON installs a models.json under ZOT_HOME and clears the
// custom provider registry and managed catalog when the test ends.
func loadTestModelsJSON(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(UserModelsPath(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	provider.SetLiveModels(nil)
	provider.SetManagedModels(nil)
	LoadUserModels()
	t.Cleanup(func() {
		_ = os.WriteFile(UserModelsPath(), []byte(`{"providers":{}}`), 0o644)
		LoadUserModels()
		provider.SetLiveModels(nil)
		provider.SetManagedModels(nil)
	})
}

func newModelsServer(t *testing.T, requests *atomic.Int32, wantAuth string, ids ...string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != wantAuth {
			t.Errorf("Authorization = %q, want %q", got, wantAuth)
		}
		var data []map[string]string
		for _, id := range ids {
			data = append(data, map[string]string{"id": id})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
	t.Cleanup(server.Close)
	return server
}

func TestRefreshCustomProviderModelsIsOptIn(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("M4_API_KEY", "")
	t.Setenv("M5_API_KEY", "")
	var requests atomic.Int32
	server := newModelsServer(t, &requests, "", "qwen3-coder")
	loadTestModelsJSON(t, fmt.Sprintf(`{"providers": {
		"m4": {"baseUrl": %q, "discover": true},
		"m5": {"baseUrl": %q, "models": [{"id": "pinned"}]}
	}}`, server.URL+"/v1", server.URL+"/v1"))

	if !CustomDiscoveryConfigured() {
		t.Fatal("discovery not reported as configured")
	}
	if got := discoverableCustomProviders(); len(got) != 1 || got[0] != "m4" {
		t.Fatalf("discoverable = %v", got)
	}
	if err := RefreshCustomProviderModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1 (only m4)", requests.Load())
	}
	m, err := provider.FindModel("m4", "qwen3-coder")
	if err != nil {
		t.Fatal(err)
	}
	if m.Source != "live" || m.BaseURL != server.URL+"/v1" || m.ContextWindow <= 0 || m.MaxOutput <= 0 {
		t.Fatalf("model = %+v", m)
	}
	if _, err := provider.FindModel("m5", "qwen3-coder"); err == nil {
		t.Fatal("m5 gained discovered models without opting in")
	}
	if _, err := provider.FindModel("m5", "pinned"); err != nil {
		t.Fatal("m5 user model lost")
	}
}

func TestRefreshCustomProviderModelsUsesStoredKeyAndKeepsUserOverrides(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("M4_API_KEY", "")
	var requests atomic.Int32
	server := newModelsServer(t, &requests, "Bearer dummy", "qwen3-coder", "big-model")
	loadTestModelsJSON(t, fmt.Sprintf(`{"providers": {
		"m4": {"baseUrl": %q, "discover": true, "models": [{"id": "big-model", "contextWindow": 200000, "maxTokens": 16000}]}
	}}`, server.URL+"/v1"))
	if err := AuthStoreFor().SetAPIKey("m4", "dummy"); err != nil {
		t.Fatal(err)
	}

	if err := RefreshCustomProviderModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d", requests.Load())
	}
	big, err := provider.FindModel("m4", "big-model")
	if err != nil {
		t.Fatal(err)
	}
	if big.Source != "user" || big.ContextWindow != 200000 || big.MaxOutput != 16000 {
		t.Fatalf("models.json override lost: %+v", big)
	}
	if _, err := provider.FindModel("m4", "qwen3-coder"); err != nil {
		t.Fatal("discovered model missing")
	}

	// Env var beats auth.json, matching credential resolution order.
	t.Setenv("M4_API_KEY", "from-env")
	server2 := newModelsServer(t, &requests, "Bearer from-env", "other")
	loadTestModelsJSON(t, fmt.Sprintf(`{"providers": {"m4": {"baseUrl": %q, "discover": true}}}`, server2.URL))
	if err := RefreshCustomProviderModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.FindModel("m4", "other"); err != nil {
		t.Fatal("refresh did not replace snapshot")
	}
	if _, err := provider.FindModel("m4", "qwen3-coder"); err == nil {
		t.Fatal("stale discovered model retained after successful refresh")
	}
}

func TestRefreshCustomProviderModelsExplicitKeylessIgnoresStoredKey(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("M4_API_KEY", "env-key")
	var requests atomic.Int32
	server := newModelsServer(t, &requests, "", "qwen3-coder")
	loadTestModelsJSON(t, fmt.Sprintf(`{"providers":{"m4":{"baseUrl":%q,"discover":true,"auth":"none"}}}`, server.URL+"/v1"))
	if err := AuthStoreFor().SetAPIKey("m4", "stored-key"); err != nil {
		t.Fatal(err)
	}
	if err := RefreshCustomProviderModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", requests.Load())
	}
}

func TestRefreshCustomProviderModelsBackgroundSkipsKeyCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZOT_HOME", home)
	t.Setenv("M4_API_KEY", "")
	var requests atomic.Int32
	server := newModelsServer(t, &requests, "", "m")
	loadTestModelsJSON(t, fmt.Sprintf(`{"providers": {"m4": {"baseUrl": %q, "discover": true}}}`, server.URL))
	creds := auth.Credentials{AdditionalAPIKeyCreds: map[string]auth.ProviderCreds{
		"m4": {APIKeyCommand: &auth.APIKeyCommand{Program: filepath.Join(home, "does-not-exist")}},
	}}
	encoded, err := json.Marshal(creds)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(AuthPath(), encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := refreshCustomProviderModels(context.Background(), apiKeyCommandSkip); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatal("background refresh probed a provider whose key needs a command")
	}
	// Interactive refresh runs the command and reports its failure
	// instead of silently probing without a key.
	err = RefreshCustomProviderModels(context.Background())
	if err == nil || !strings.Contains(err.Error(), "m4") {
		t.Fatalf("command failure not reported: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("refresh probed after key command failed")
	}
}

func TestRefreshCustomProviderModelsKeepsSnapshotOnFailure(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("M4_API_KEY", "")
	t.Setenv("M5_API_KEY", "")
	var requests atomic.Int32
	good := newModelsServer(t, &requests, "", "alive")
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	loadTestModelsJSON(t, fmt.Sprintf(`{"providers": {
		"m4": {"baseUrl": %q, "discover": true},
		"m5": {"baseUrl": %q, "discover": true}
	}}`, good.URL, failing.URL))
	provider.SetManagedModelsForProvider("m5", []provider.Model{{Provider: "m5", ID: "previous", Source: "live"}})

	err := RefreshCustomProviderModels(context.Background())
	if err == nil || !strings.Contains(err.Error(), "m5 discovery HTTP 503") {
		t.Fatalf("err = %v", err)
	}
	if _, err := provider.FindModel("m4", "alive"); err != nil {
		t.Fatal("healthy provider not refreshed when a sibling failed")
	}
	if _, err := provider.FindModel("m5", "previous"); err != nil {
		t.Fatal("failed refresh dropped the previous snapshot")
	}
}

func TestValidateAndRepairConfig_PreservesDiscoverableCustomModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZOT_HOME", home)
	loadTestModelsJSON(t, `{"providers": {
		"m4": {"baseUrl": "http://127.0.0.1:1/v1", "discover": true},
		"m5": {"baseUrl": "http://127.0.0.1:1/v1", "models": [{"id": "pinned"}]}
	}}`)

	write := func(c Config) {
		t.Helper()
		b, _ := json.Marshal(c)
		if err := os.WriteFile(filepath.Join(home, "config.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Discovery has not run yet at repair time, so the model is unknown.
	write(Config{Provider: "m4", Model: "qwen3-coder"})
	ValidateAndRepairConfig()
	out, _ := LoadConfig()
	if out.Provider != "m4" || out.Model != "qwen3-coder" {
		t.Fatalf("discoverable custom model repaired away: %+v", out)
	}

	// A provider without discovery keeps the existing repair behavior.
	write(Config{Provider: "m5", Model: "gone"})
	ValidateAndRepairConfig()
	out, _ = LoadConfig()
	if out.Provider != "m5" || out.Model != "pinned" {
		t.Fatalf("non-discoverable custom model not repaired: %+v", out)
	}
}

func TestResolveRestoresDiscoveredCustomModelAfterRestart(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("M4_API_KEY", "")
	var requests atomic.Int32
	server := newModelsServer(t, &requests, "", "qwen3-coder")
	loadTestModelsJSON(t, fmt.Sprintf(`{"providers": {"m4": {"baseUrl": %q, "discover": true}}}`, server.URL+"/v1"))

	r, err := Resolve(Args{Provider: "m4", Model: "qwen3-coder", NoSkill: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want a restore probe", requests.Load())
	}
	if r.Provider != "m4" || r.Model != "qwen3-coder" || r.BaseURL != server.URL+"/v1" {
		t.Fatalf("resolved = provider=%s model=%s base=%s", r.Provider, r.Model, r.BaseURL)
	}
	if r.MaxOutput <= 0 {
		t.Fatalf("discovered model lost output budget: %+v", r.MaxOutput)
	}

	// Once the model is known, launching again does not probe.
	if _, err := Resolve(Args{Provider: "m4", Model: "qwen3-coder", NoSkill: true}, true); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want no extra probe", requests.Load())
	}
}
