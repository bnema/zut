package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/auth"
)

// loadTestModelsJSON installs a models.json under ZUT_HOME and restores the
// provider catalog and custom-provider registry when the test ends.
func loadTestModelsJSON(t *testing.T, body string) {
	t.Helper()
	preserveProviderCatalog(t)
	if err := os.WriteFile(UserModelsPath(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	provider.SetManagedModelsForProvider("m4", nil)
	provider.SetManagedModelsForProvider("m5", nil)
	LoadUserModels()
	t.Cleanup(func() {
		_ = os.WriteFile(UserModelsPath(), []byte(`{"providers":{}}`), 0o644)
		LoadUserModels()
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

func TestCustomDiscoveryKeepsTLSVerificationWithInsecureInference(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	if err := SaveConfig(Config{Insecure: true}); err != nil {
		t.Fatal(err)
	}
	client := customDiscoveryHTTPClient()
	if transport, ok := client.Transport.(*http.Transport); ok && transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("discovery inherited insecure inference TLS")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
	}))
	defer server.Close()
	if _, err := provider.DiscoverCustomProviderWithClient(context.Background(), client, "test-provider", server.URL, ""); err == nil {
		t.Fatal("discovery trusted a self-signed server")
	}
}

func TestRefreshCustomProviderModelsIsOptIn(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("M4_API_KEY", "")
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

func TestDiscoveryDefaultsOffNoRequestsAtStartup(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	var requests atomic.Int32
	server := newModelsServer(t, &requests, "")
	loadTestModelsJSON(t, fmt.Sprintf(`{"providers":{"m5":{"baseUrl":%q,"models":[{"id":"pinned"}]}}}`, server.URL+"/v1"))
	refreshCustomProvidersBackground()
	if requests.Load() != 0 || CustomDiscoveryConfigured() {
		t.Fatalf("discovery ran without opt-in (requests=%d)", requests.Load())
	}
}

func TestRefreshCustomProviderModelsUsesStoredKeyAndKeepsUserOverrides(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
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
}

func TestRefreshCustomProviderModelsExplicitKeylessIgnoresStoredKey(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
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
	t.Setenv("ZUT_HOME", home)
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
	err = RefreshCustomProviderModels(context.Background())
	if err == nil || !strings.Contains(err.Error(), "m4") {
		t.Fatalf("command failure not reported: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("refresh probed after key command failed")
	}
}

func TestRefreshCustomProviderModelsKeepsSnapshotOnFailure(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
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
	t.Setenv("ZUT_HOME", home)
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
	write(Config{Provider: "m4", Model: "qwen3-coder"})
	ValidateAndRepairConfig()
	out, _ := LoadConfig()
	if out.Provider != "m4" || out.Model != "qwen3-coder" {
		t.Fatalf("discoverable custom model repaired away: %+v", out)
	}
	write(Config{Provider: "m5", Model: "gone"})
	ValidateAndRepairConfig()
	out, _ = LoadConfig()
	if out.Provider != "m5" || out.Model != "pinned" {
		t.Fatalf("non-discoverable custom model not repaired: %+v", out)
	}
}

func TestResolveRestoresDiscoveredCustomModelAfterRestart(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
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
	if r.Provider != "m4" || r.Model != "qwen3-coder" || r.BaseURL != server.URL+"/v1" || r.MaxOutput <= 0 {
		t.Fatalf("resolved = %+v", r)
	}
	if !r.HasCredential() || r.Credential != "" {
		t.Fatalf("keyless discovered provider should be usable without a placeholder: %q", r.Credential)
	}
	if _, err := Resolve(Args{Provider: "m4", Model: "qwen3-coder", NoSkill: true}, true); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want no extra probe", requests.Load())
	}
}

func streamOnce(t *testing.T, r Resolved, model string) {
	t.Helper()
	events, err := r.NewClient().Stream(context.Background(), provider.Request{
		Model: model, Messages: []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "hello"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
}

func TestCustomProviderKeylessOmitsAuthorization(t *testing.T) {
	for _, api := range []string{"openai", "openai-responses", "anthropic"} {
		t.Run(api, func(t *testing.T) {
			t.Setenv("ZUT_HOME", t.TempDir())
			t.Setenv("UNSLOTH_API_KEY", "")
			got := make(chan http.Header, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got <- r.Header.Clone()
				w.Header().Set("Content-Type", "text/event-stream")
			}))
			t.Cleanup(srv.Close)
			loadTestModelsJSON(t, `{"providers":{"unsloth":{"baseUrl":"`+srv.URL+`/v1","api":"`+api+`","models":[{"id":"qista"}]}}}`)
			r, err := Resolve(Args{Provider: "unsloth", Model: "qista", NoSkill: true}, true)
			if err != nil {
				t.Fatal(err)
			}
			if r.Credential != "" || !r.HasCredential() {
				t.Fatalf("credential = %q usable=%v", r.Credential, r.HasCredential())
			}
			streamOnce(t, r, "qista")
			h := <-got
			if _, ok := h["Authorization"]; ok {
				t.Fatal("Authorization sent")
			}
			if _, ok := h["X-Api-Key"]; ok {
				t.Fatal("X-Api-Key sent")
			}
		})
	}
}

func TestCustomProviderExplicitKeylessIgnoresInheritedKeys(t *testing.T) {
	for _, api := range []string{"openai", "openai-responses", "anthropic"} {
		t.Run(api, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("ZUT_HOME", home)
			t.Setenv("UNSLOTH_API_KEY", "env-key")
			got := make(chan http.Header, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got <- r.Header.Clone()
				w.Header().Set("Content-Type", "text/event-stream")
			}))
			t.Cleanup(srv.Close)
			loadTestModelsJSON(t, `{"providers":{"unsloth":{"baseUrl":"`+srv.URL+`/v1","api":"`+api+`","auth":"none","models":[{"id":"qista"}]}}}`)
			// A stored command-backed key must never run for a keyless provider.
			marker := filepath.Join(home, "ran")
			creds := auth.Credentials{AdditionalAPIKeyCreds: map[string]auth.ProviderCreds{
				"unsloth": {APIKeyCommand: &auth.APIKeyCommand{Program: "/bin/sh", Args: []string{"-c", "touch " + marker + "; echo secret"}}},
			}}
			encoded, _ := json.Marshal(creds)
			if err := os.WriteFile(AuthPath(), encoded, 0o600); err != nil {
				t.Fatal(err)
			}
			r, err := Resolve(Args{Provider: "unsloth", Model: "qista", APIKey: "cli-key", NoSkill: true}, true)
			if err != nil {
				t.Fatal(err)
			}
			if r.Credential != "" || !r.HasCredential() {
				t.Fatalf("keyless resolved credential = %q, usable = %v", r.Credential, r.HasCredential())
			}
			streamOnce(t, r, "qista")
			h := <-got
			if _, ok := h["Authorization"]; ok {
				t.Fatal("Authorization sent")
			}
			if _, ok := h["X-Api-Key"]; ok {
				t.Fatal("X-Api-Key sent")
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("stored key command executed for explicit keyless provider")
			}
			stored, err := AuthStoreFor().Load()
			if err != nil || stored.AdditionalAPIKeyCreds["unsloth"].APIKeyCommand == nil {
				t.Fatalf("stored credential changed: %+v, %v", stored.AdditionalAPIKeyCreds["unsloth"], err)
			}
		})
	}
}

func TestCustomProviderWithKeyStillSendsAuthorization(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("UNSLOTH_API_KEY", "")
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
	}))
	t.Cleanup(srv.Close)
	loadTestModelsJSON(t, `{"providers":{"unsloth":{"baseUrl":"`+srv.URL+`/v1","api":"openai","models":[{"id":"qista"}]}}}`)
	if err := AuthStoreFor().SetAPIKey("unsloth", "stored-key"); err != nil {
		t.Fatal(err)
	}
	r, err := Resolve(Args{Provider: "unsloth", Model: "qista", NoSkill: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	streamOnce(t, r, "qista")
	if a := <-got; a != "Bearer stored-key" {
		t.Fatalf("Authorization = %q", a)
	}
}

func TestBuiltinProviderStillRequiresCredentialAndKeepsPlaceholderRules(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_OAUTH_TOKEN", "")
	// A custom provider named like a built-in must not become keyless.
	loadTestModelsJSON(t, `{"providers":{"anthropic":{"baseUrl":"http://127.0.0.1:9/v1","auth":"none","models":[{"id":"claude-sonnet-5"}]}}}`)
	r, err := Resolve(Args{Provider: "anthropic", Model: "claude-sonnet-5", NoSkill: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.credentialOptional {
		t.Fatal("built-in provider treated as keyless custom endpoint")
	}
}

func TestModelPickerProvidersIncludeKeylessCustomEndpoint(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("UNSLOTH_API_KEY", "")
	loadTestModelsJSON(t, `{"providers":{"unsloth":{"baseUrl":"http://127.0.0.1:8888/v1","api":"openai","models":[{"id":"qista"}]}}}`)
	if CredentialAvailable("unsloth") {
		t.Fatal("test requires no credential")
	}
	if !slices.Contains(modelPickerProviders(), "unsloth") {
		t.Fatal("keyless custom endpoint missing from model picker")
	}
	loadTestModelsJSON(t, `{"providers":{"unsloth":{"models":[{"id":"qista","baseUrl":"http://127.0.0.1:8888/v1"}]}}}`)
	if !slices.Contains(modelPickerProviders(), "unsloth") {
		t.Fatal("model-level keyless endpoint missing from model picker")
	}
}

func TestResolveUnknownModelRefreshesOnlySelectedCustomProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	t.Setenv("M4_API_KEY", "")
	t.Setenv("M5_API_KEY", "")
	var m4Requests, m5Requests atomic.Int32
	m4 := newModelsServer(t, &m4Requests, "", "qwen3-coder")
	m5 := newModelsServer(t, &m5Requests, "", "other")
	loadTestModelsJSON(t, fmt.Sprintf(`{"providers": {
		"m4": {"baseUrl": %q, "discover": true},
		"m5": {"baseUrl": %q, "discover": true}
	}}`, m4.URL+"/v1", m5.URL+"/v1"))
	// m5 has a command-backed key; resolving m4 must never run it.
	marker := filepath.Join(home, "m5-command-ran")
	creds := auth.Credentials{AdditionalAPIKeyCreds: map[string]auth.ProviderCreds{
		"m5": {APIKeyCommand: &auth.APIKeyCommand{Program: "/bin/sh", Args: []string{"-c", "touch " + marker + "; echo secret"}}},
	}}
	encoded, _ := json.Marshal(creds)
	if err := os.WriteFile(AuthPath(), encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Resolve(Args{Provider: "m4", Model: "qwen3-coder", NoSkill: true}, true); err != nil {
		t.Fatal(err)
	}
	if m4Requests.Load() != 1 {
		t.Fatalf("selected provider requests = %d, want 1", m4Requests.Load())
	}
	if m5Requests.Load() != 0 {
		t.Fatal("unrelated provider endpoint was contacted")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("unrelated provider api-key command executed")
	}
}

func TestRefreshSingleCustomProviderKeepsSnapshotOnFailure(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("M4_API_KEY", "")
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	loadTestModelsJSON(t, fmt.Sprintf(`{"providers":{"m4":{"baseUrl":%q,"discover":true},"m6":{"baseUrl":%q}}}`, failing.URL, failing.URL))
	provider.SetManagedModelsForProvider("m4", []provider.Model{{Provider: "m4", ID: "previous", Source: "live"}})

	if err := refreshCustomProviderModel(context.Background(), "m4", apiKeyCommandExecute); err == nil {
		t.Fatal("expected refresh failure")
	}
	if _, err := provider.FindModel("m4", "previous"); err != nil {
		t.Fatal("failed scoped refresh dropped the previous snapshot")
	}
	// A provider that did not opt in is never contacted or changed.
	if err := refreshCustomProviderModel(context.Background(), "m6", apiKeyCommandExecute); err != nil {
		t.Fatalf("non-discoverable provider refreshed: %v", err)
	}
}
