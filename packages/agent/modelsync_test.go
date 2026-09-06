package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/auth"
)

func TestFilterCacheByProviderScopesRemovesMismatchedProvider(t *testing.T) {
	cache := provider.ModelCache{
		Models: []provider.Model{
			{Provider: "openai-codex", ID: "account-a-only"},
			{Provider: "openai", ID: "shared"},
		},
		AuthoritativeProviders: []string{"openai-codex"},
		ProviderScopes:         map[string]string{"openai-codex": "account-a"},
	}

	filtered := filterCacheByProviderScopes(cache, map[string]string{"openai-codex": "account-b"})
	if len(filtered.Models) != 1 || filtered.Models[0].Provider != "openai" {
		t.Fatalf("models = %+v, want only non-scoped model", filtered.Models)
	}
	if len(filtered.AuthoritativeProviders) != 0 {
		t.Fatalf("authoritative providers = %v, want none", filtered.AuthoritativeProviders)
	}
	if len(filtered.ProviderScopes) != 0 {
		t.Fatalf("provider scopes = %v, want none", filtered.ProviderScopes)
	}
}

func TestFilterCacheByProviderScopesRetainsMatchingScopedProvider(t *testing.T) {
	cache := provider.ModelCache{
		Models:                 []provider.Model{{Provider: "openai-codex", ID: "account-a-only"}},
		AuthoritativeProviders: []string{"openai-codex"},
		ProviderScopes:         map[string]string{"openai-codex": "account-a"},
	}

	filtered := filterCacheByProviderScopes(cache, map[string]string{"openai-codex": "account-a"})
	if len(filtered.Models) != 1 || len(filtered.AuthoritativeProviders) != 1 || filtered.ProviderScopes["openai-codex"] != "account-a" {
		t.Fatalf("matching scoped cache was not preserved: %+v", filtered)
	}
}

func TestFilterCacheByProviderScopesRemovesRotatedOpenCodeGoCredential(t *testing.T) {
	cache := provider.ModelCache{
		Models:                 []provider.Model{{Provider: "opencode-go", ID: "old-key-model"}},
		AuthoritativeProviders: []string{"opencode-go"},
		ProviderScopes:         map[string]string{"opencode-go": credentialScopeForEndpoint("old-key", "")},
	}

	filtered := filterCacheByProviderScopes(cache, map[string]string{"opencode-go": credentialScopeForEndpoint("new-key", "")})
	if len(filtered.Models) != 0 || len(filtered.AuthoritativeProviders) != 0 || len(filtered.ProviderScopes) != 0 {
		t.Fatalf("rotated OpenCode Go cache was retained: %+v", filtered)
	}
}

func TestLoadCachedModelsFiltersExplicitOpenCodeGoCredentialScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	t.Setenv("OPENCODE_API_KEY", "")

	if err := provider.SaveCache(filepath.Join(home, "models-cache.json"), provider.ModelCache{
		Version:                provider.ModelCacheVersion,
		FetchedAt:              time.Now(),
		Models:                 []provider.Model{{Provider: "opencode-go", ID: "old-key-model"}},
		AuthoritativeProviders: []string{"opencode-go"},
		ProviderScopes:         map[string]string{"opencode-go": credentialScopeForEndpoint("old-key", "")},
	}); err != nil {
		t.Fatal(err)
	}
	provider.SetLiveModels(nil)
	t.Cleanup(func() { provider.SetLiveModels(nil) })

	loadCachedModels(map[string]string{"opencode-go": credentialScopeForEndpoint("new-key", "")})
	if _, err := provider.FindModel("opencode-go", "old-key-model"); err == nil {
		t.Fatal("cached model from another OpenCode Go credential remained active")
	}
}

func TestLoadCachedModelsClearsMismatchedActiveOpenCodeGoOverlay(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	provider.SetLiveModels([]provider.Model{{Provider: provider.ProviderOpenCodeGo, ID: "stale-model", BaseURL: "https://old.example/v1"}})
	t.Cleanup(func() { provider.SetLiveModels(nil) })

	loadCachedModels(map[string]string{provider.ProviderOpenCodeGo: credentialScopeForEndpoint("new-key", "https://new.example/v1")})
	if _, err := provider.FindModel(provider.ProviderOpenCodeGo, "stale-model"); err == nil {
		t.Fatal("mismatched OpenCode Go metadata remained active after cache filtering")
	}
}

func TestFilterCacheByProviderScopesDropsLegacyAuthoritativeCodex(t *testing.T) {
	cache := provider.ModelCache{
		Models:                 []provider.Model{{Provider: "openai-codex", ID: "legacy"}},
		AuthoritativeProviders: []string{"openai-codex"},
	}

	filtered := filterCacheByProviderScopes(cache, map[string]string{"openai-codex": "account-a"})
	if len(filtered.Models) != 0 || len(filtered.AuthoritativeProviders) != 0 {
		t.Fatalf("legacy Codex cache was retained: %+v", filtered)
	}
}

func TestRefreshLlamaCPPModelsAddsOnlyLoadedModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"loaded","status":{"value":"loaded"},"meta":{"n_ctx":32768}},{"id":"offline","status":{"value":"unloaded"}}]}`)
	}))
	defer server.Close()

	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("LLAMA_BASE_URL", "")
	if err := AuthStoreFor().SetEndpointCredential(provider.LlamaCPPProviderID, server.URL, ""); err != nil {
		t.Fatal(err)
	}
	provider.SetManagedModels(nil)
	t.Cleanup(func() { provider.SetManagedModels(nil) })

	if err := RefreshLlamaCPPModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	loaded, err := provider.FindModel(provider.LlamaCPPProviderID, "loaded")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ContextWindow != 32768 || loaded.BaseURL != server.URL+"/v1" {
		t.Fatalf("loaded model = %+v", loaded)
	}
	if _, err := provider.FindModel(provider.LlamaCPPProviderID, "offline"); err == nil {
		t.Fatal("unloaded model must not be selectable")
	}
}

// TestValidateAndRepairConfig_MismatchedPair simulates the bug from a
// stale /model switch: provider=anthropic but model=kimi-for-coding
// (which belongs to provider=kimi). The validator should rewrite the
// model to anthropic's default and persist.
func TestValidateAndRepairConfig_MismatchedPair(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)

	must := func(c Config) {
		t.Helper()
		b, _ := json.Marshal(c)
		if err := os.WriteFile(filepath.Join(home, "config.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must(Config{Provider: "anthropic", Model: "kimi-for-coding"})

	ValidateAndRepairConfig()

	out, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if out.Provider != "anthropic" {
		t.Errorf("provider not preserved: %q", out.Provider)
	}
	if out.Model == "kimi-for-coding" {
		t.Errorf("model not repaired; still %q", out.Model)
	}
	if out.Model == "" {
		t.Errorf("model not set; expected anthropic default")
	}
}

// TestValidateAndRepairConfig_UnknownProvider resets to anthropic and
// clears the model when the saved provider id isn't recognised
// (e.g. user removed it from a previous build).
func TestValidateAndRepairConfig_UnknownProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)

	b, _ := json.Marshal(Config{Provider: "made-up-provider", Model: "some-model"})
	_ = os.WriteFile(filepath.Join(home, "config.json"), b, 0o644)

	ValidateAndRepairConfig()

	out, _ := LoadConfig()
	if out.Provider != "anthropic" {
		t.Errorf("provider not reset: %q", out.Provider)
	}
	if out.Model != "" {
		t.Errorf("model not cleared: %q", out.Model)
	}
}

// TestValidateAndRepairConfig_UnknownModel keeps the provider but
// snaps the model to that provider's default when the saved id is no
// longer in the catalog.
func TestValidateAndRepairConfig_UnknownModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)

	b, _ := json.Marshal(Config{Provider: "anthropic", Model: "claude-deleted-model"})
	_ = os.WriteFile(filepath.Join(home, "config.json"), b, 0o644)

	ValidateAndRepairConfig()

	out, _ := LoadConfig()
	if out.Provider != "anthropic" {
		t.Errorf("provider changed: %q", out.Provider)
	}
	if out.Model == "" || out.Model == "claude-deleted-model" {
		t.Errorf("model not repaired: %q", out.Model)
	}
}

func TestValidateAndRepairConfig_OpenCodeGoPreservesDynamicModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)

	const model = "muse-spark-1.2-contributor"
	b, _ := json.Marshal(Config{Provider: "opencode-go", Model: model})
	if err := os.WriteFile(filepath.Join(home, "config.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	ValidateAndRepairConfig()

	out, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if out.Provider != "opencode-go" || out.Model != model {
		t.Fatalf("dynamic OpenCode Go config changed: provider=%q model=%q", out.Provider, out.Model)
	}
}

func TestResolveAllowsUnknownOpenCodeGoModelBeforeDiscovery(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("OPENCODE_API_KEY", "")

	const model = "muse-spark-1.2-contributor"
	resolved, err := Resolve(Args{Provider: "opencode-go", Model: model}, false)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Provider != "opencode-go" || resolved.Model != model {
		t.Fatalf("resolved dynamic model = provider=%q model=%q", resolved.Provider, resolved.Model)
	}
	if resolved.ContextWindow != 128000 || resolved.MaxOutput != 16384 {
		t.Fatalf("bootstrap model limits = context %d output %d", resolved.ContextWindow, resolved.MaxOutput)
	}
	if resolved.ModelCatalogSnapshot() != nil {
		t.Fatal("normal Resolve captured a private OpenCode Go catalog snapshot")
	}
}

func TestResolveRejectsUnknownOpenCodeGoModelAfterDiscovery(t *testing.T) {
	preserveProviderCatalog(t)
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("OPENCODE_API_KEY", "")
	provider.SetLiveModelsForProviders([]provider.Model{{
		Provider:      provider.ProviderOpenCodeGo,
		ID:            "served-model",
		ContextWindow: 128000,
		MaxOutput:     16384,
	}}, []string{provider.ProviderOpenCodeGo})
	provider.SetUserModels(nil)

	resolved, err := Resolve(Args{Provider: provider.ProviderOpenCodeGo, Model: "removed-model"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Model != "served-model" {
		t.Fatalf("unknown model resolved to %q, want served-model", resolved.Model)
	}
}

func TestResolveFallbackUsesCapturedOpenCodeGoCatalog(t *testing.T) {
	preserveProviderCatalog(t)
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("OPENCODE_API_KEY", "")

	const modelID = "shared-scoped-model"
	provider.SetLiveModelsForProviders([]provider.Model{{
		Provider:      provider.ProviderOpenCodeGo,
		ID:            modelID,
		ContextWindow: 111000,
		MaxOutput:     11000,
		BaseURL:       "https://account-a.example/v1",
	}}, []string{provider.ProviderOpenCodeGo})
	args := Args{
		Provider:                  provider.ProviderOpenCodeGo,
		Model:                     "removed-from-account-a",
		APIKey:                    "account-a-key",
		modelCatalog:              provider.ModelsForProvider(provider.ProviderOpenCodeGo),
		modelCatalogAuthoritative: true,
		NoTools:                   true,
	}

	// A later runtime replaces the process-global catalog with another
	// credential's endpoint. The first runtime must resolve its fallback from
	// the snapshot it captured before that replacement.
	provider.SetLiveModelsForProviders([]provider.Model{{
		Provider:      provider.ProviderOpenCodeGo,
		ID:            modelID,
		ContextWindow: 222000,
		MaxOutput:     22000,
		BaseURL:       "https://account-b.example/v1",
	}}, []string{provider.ProviderOpenCodeGo})

	resolved, err := Resolve(args, false)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Model != modelID || resolved.ContextWindow != 111000 || resolved.MaxOutput != 11000 || resolved.BaseURL != "https://account-a.example/v1" {
		t.Fatalf("fallback resolved from the wrong catalog: provider=%q model=%q context=%d output=%d base=%q", resolved.Provider, resolved.Model, resolved.ContextWindow, resolved.MaxOutput, resolved.BaseURL)
	}
}

func TestValidateAndRepairConfigPreservesPaddedDynamicModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	const model = "muse-spark-1.2-contributor"
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"provider":"opencode-go","model":" `+model+` "}`), 0o644); err != nil {
		t.Fatal(err)
	}

	ValidateAndRepairConfig()

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "opencode-go" || cfg.Model != " "+model+" " {
		t.Fatalf("padded dynamic model was repaired: provider=%q model=%q", cfg.Provider, cfg.Model)
	}
	resolved, err := Resolve(Args{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Model != model {
		t.Fatalf("resolved model = %q, want trimmed dynamic id", resolved.Model)
	}
}

func TestResolveTrimsModelID(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())

	resolved, err := Resolve(Args{Provider: "openai", Model: " gpt-5.6-sol "}, false)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Model != "gpt-5.6-sol" {
		t.Fatalf("resolved model = %q, want trimmed id", resolved.Model)
	}
}

func TestResolveTreatsWhitespaceConfigModelAsMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"provider":"openai","model":" \t "}`), 0o644); err != nil {
		t.Fatal(err)
	}

	resolved, err := Resolve(Args{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Provider != "openai" || resolved.Model != defaultModelForProvider("openai") {
		t.Fatalf("whitespace config model resolved to provider=%q model=%q", resolved.Provider, resolved.Model)
	}
}

func TestCurrentModelProviderScopesHashesOpenCodeGoCredential(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	const key = "synthetic-opencode-key"
	t.Setenv("OPENCODE_API_KEY", key)

	scopes := currentModelProviderScopes()
	if got, want := scopes["opencode-go"], credentialScopeForEndpoint(key, ""); got != want {
		t.Fatalf("OpenCode Go scope = %q, want %q", got, want)
	}
	if got, other := scopes["opencode-go"], credentialScopeForEndpoint(key, "https://proxy.example/v1"); got == other {
		t.Fatal("OpenCode Go cache scope did not change with the endpoint")
	}
	if scopes["opencode-go"] == key {
		t.Fatal("OpenCode Go credential was stored directly in its cache scope")
	}
}

func TestSynchronousOpenCodeGoAPIKeyHonorsCancellation(t *testing.T) {
	preserveProviderCatalog(t)
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	t.Setenv("OPENCODE_API_KEY", "")
	t.Setenv("ZUT_AGENT_API_KEY_COMMAND_HELPER", "1")
	marker := filepath.Join(home, "canceled-command-ran")
	credentials := auth.Credentials{AdditionalAPIKeyCreds: map[string]auth.ProviderCreds{
		provider.ProviderOpenCodeGo: {
			APIKeyCommand: &auth.APIKeyCommand{
				Program: os.Args[0],
				Args:    []string{"-test.run=^TestAgentAPIKeyCommandHelperProcess$", "--", marker},
			},
		},
	}}
	encoded, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(AuthPath(), encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	previous := discoverOpenCodeGoFn
	t.Cleanup(func() { discoverOpenCodeGoFn = previous })
	discoverOpenCodeGoFn = func(context.Context, string, string) ([]provider.Model, error) {
		t.Fatal("canceled catalog preparation reached discovery")
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ResolveSDK(ctx, Args{Provider: provider.ProviderOpenCodeGo}); !errors.Is(err, context.Canceled) {
		t.Fatalf("ResolveSDK error = %v, want context.Canceled", err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	PrepareRuntimeCatalog(ctx, true, provider.ProviderOpenCodeGo, "", "", nil)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("canceled credential command marker = %v", err)
	}
}

func TestSynchronousRefreshExecutesOpenCodeGoAPIKeyCommand(t *testing.T) {
	preserveProviderCatalog(t)
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	t.Setenv("OPENCODE_API_KEY", "")
	t.Setenv("ZUT_AGENT_API_KEY_COMMAND_HELPER", "1")
	marker := filepath.Join(home, "command-ran")
	credentials := auth.Credentials{AdditionalAPIKeyCreds: map[string]auth.ProviderCreds{
		provider.ProviderOpenCodeGo: {
			APIKeyCommand: &auth.APIKeyCommand{
				Program: os.Args[0],
				Args:    []string{"-test.run=^TestAgentAPIKeyCommandHelperProcess$", "--", marker},
			},
		},
	}}
	encoded, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(AuthPath(), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	previous := discoverOpenCodeGoFn
	t.Cleanup(func() { discoverOpenCodeGoFn = previous })
	var gotKey string
	discoverOpenCodeGoFn = func(_ context.Context, apiKey, endpoint string) ([]provider.Model, error) {
		gotKey = apiKey
		if endpoint != "" {
			t.Fatalf("discovery endpoint = %q, want default", endpoint)
		}
		return []provider.Model{{Provider: provider.ProviderOpenCodeGo, ID: "command-model"}}, nil
	}

	refreshModelsWithMode(provider.ProviderOpenCodeGo, "", "", provider.ProviderOpenCodeGo, apiKeyCommandExecute)
	if gotKey != "resolved-secret" {
		t.Fatalf("discovery key = %q, want command result", gotKey)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("credential command marker: %v", err)
	}
	cache, err := provider.LoadCache(ModelCachePath())
	if err != nil {
		t.Fatal(err)
	}
	if cache.ProviderScopes[provider.ProviderOpenCodeGo] != credentialScopeForEndpoint(gotKey, "") {
		t.Fatalf("cache scope = %q, want command credential scope", cache.ProviderScopes[provider.ProviderOpenCodeGo])
	}
}

func TestResolveSDKRefreshReusesResolvedCommandCredential(t *testing.T) {
	preserveProviderCatalog(t)
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	t.Setenv("OPENCODE_API_KEY", "")
	t.Setenv("ZUT_AGENT_API_KEY_COMMAND_HELPER", "1")
	marker := filepath.Join(home, "sdk-command-ran")
	credentials := auth.Credentials{AdditionalAPIKeyCreds: map[string]auth.ProviderCreds{
		provider.ProviderOpenCodeGo: {
			APIKeyCommand: &auth.APIKeyCommand{
				Program: os.Args[0],
				Args:    []string{"-test.run=^TestAgentAPIKeyCommandHelperProcess$", "--", marker},
			},
		},
	}}
	encoded, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(AuthPath(), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	previous := discoverOpenCodeGoFn
	t.Cleanup(func() { discoverOpenCodeGoFn = previous })
	called := make(chan string, 1)
	discoverOpenCodeGoFn = func(_ context.Context, apiKey, endpoint string) ([]provider.Model, error) {
		called <- apiKey + "\x00" + endpoint
		return nil, fmt.Errorf("synthetic discovery stop")
	}

	if _, err := ResolveSDK(context.Background(), Args{Provider: provider.ProviderOpenCodeGo, BaseURL: "https://proxy.example/v1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-called:
		if got != "resolved-secret\x00https://proxy.example/v1" {
			t.Fatalf("discovery inputs = %q, want resolved credential and endpoint", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SDK OpenCode Go discovery was not invoked")
	}
}

func TestRefreshModelsUsesExplicitOpenCodeGoEndpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	provider.SetLiveModels(nil)
	t.Cleanup(func() { provider.SetLiveModels(nil) })

	const (
		key     = "synthetic-proxy-key"
		baseURL = "https://proxy.example/v1"
		modelID = "proxy-model"
	)
	previous := discoverOpenCodeGoFn
	t.Cleanup(func() { discoverOpenCodeGoFn = previous })
	var gotKey, gotBaseURL string
	discoverOpenCodeGoFn = func(_ context.Context, apiKey, endpoint string) ([]provider.Model, error) {
		gotKey, gotBaseURL = apiKey, endpoint
		return []provider.Model{{Provider: provider.ProviderOpenCodeGo, ID: modelID, BaseURL: endpoint, Source: "live"}}, nil
	}

	refreshModels(provider.ProviderOpenCodeGo, key, baseURL, provider.ProviderOpenCodeGo)
	if gotKey != key || gotBaseURL != baseURL {
		t.Fatalf("discovery received key=%q endpoint=%q, want key=%q endpoint=%q", gotKey, gotBaseURL, key, baseURL)
	}
	cache, err := provider.LoadCache(ModelCachePath())
	if err != nil {
		t.Fatal(err)
	}
	if cache.ProviderScopes[provider.ProviderOpenCodeGo] != credentialScopeForEndpoint(key, baseURL) {
		t.Fatalf("cache scope = %q, want endpoint-scoped credential", cache.ProviderScopes[provider.ProviderOpenCodeGo])
	}
}

func TestRefreshModelsReplacesUnchangedOpenCodeGoScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	provider.SetLiveModels(nil)
	t.Cleanup(func() { provider.SetLiveModels(nil) })

	const (
		oldKey  = "synthetic-old-key"
		oldBase = "https://old.example/v1"
		newKey  = "synthetic-new-key"
		newBase = "https://new.example/v1"
	)
	if err := provider.SaveCache(ModelCachePath(), provider.ModelCache{
		Version:                2,
		FetchedAt:              time.Now().Add(-48 * time.Hour),
		Models:                 []provider.Model{{Provider: provider.ProviderOpenCodeGo, ID: "old-model", BaseURL: oldBase}},
		AuthoritativeProviders: []string{provider.ProviderOpenCodeGo},
		ProviderScopes:         map[string]string{provider.ProviderOpenCodeGo: credentialScope(oldKey)},
	}); err != nil {
		t.Fatal(err)
	}
	previous := discoverOpenCodeGoFn
	t.Cleanup(func() { discoverOpenCodeGoFn = previous })
	discoverOpenCodeGoFn = func(_ context.Context, apiKey, endpoint string) ([]provider.Model, error) {
		if apiKey != newKey || endpoint != newBase {
			t.Fatalf("discovery inputs = key %q endpoint %q", apiKey, endpoint)
		}
		return []provider.Model{{Provider: provider.ProviderOpenCodeGo, ID: "new-model", BaseURL: endpoint, Source: "live"}}, nil
	}

	refreshModels(provider.ProviderOpenCodeGo, newKey, newBase, provider.ProviderOpenCodeGo)
	cache, err := provider.LoadCache(ModelCachePath())
	if err != nil {
		t.Fatal(err)
	}
	if cache.ProviderScopes[provider.ProviderOpenCodeGo] != credentialScopeForEndpoint(newKey, newBase) {
		t.Fatalf("scope = %q, want new endpoint-scoped scope", cache.ProviderScopes[provider.ProviderOpenCodeGo])
	}
	if _, ok := findCachedModel(cache.Models, provider.ProviderOpenCodeGo, "new-model"); !ok {
		t.Fatalf("new OpenCode Go model missing: %+v", cache.Models)
	}
	if cache.IsFresh() {
		t.Fatal("partial upgrade of a legacy cache must still require a full refresh")
	}
}

func TestEffectiveCatalogBaseURLUsesProviderLevelOpenCodeGoEndpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	path := filepath.Join(home, "models.json")
	const baseURL = "https://provider-proxy.example/v1"
	if err := os.WriteFile(path, []byte(`{"providers":{"opencode-go":{"baseUrl":"`+baseURL+`","models":[]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	models := LoadUserModels()
	t.Cleanup(func() {
		_ = os.WriteFile(path, []byte(`{"providers":{}}`), 0o600)
		LoadUserModels()
		provider.SetUserModels(nil)
	})

	if got := effectiveCatalogBaseURL(provider.ProviderOpenCodeGo, "new-live-model", "", models); got != baseURL {
		t.Fatalf("provider-level endpoint = %q, want %q", got, baseURL)
	}
	manifest := ZutfileManifest{}
	manifest.Model.Preferred = []string{"new-live-model"}
	gotProvider, gotModel := zutfileCatalogSelection(Args{}, manifest, models)
	if gotProvider != provider.ProviderOpenCodeGo || gotModel != "new-live-model" {
		t.Fatalf("catalog selection = provider=%q model=%q, want %q/new-live-model", gotProvider, gotModel, provider.ProviderOpenCodeGo)
	}
}

func TestZutfilePreferredDynamicModelRefreshesBeforeRequirements(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	provider.SetLiveModels(nil)
	provider.SetUserModels(nil)
	t.Cleanup(func() {
		provider.SetLiveModels(nil)
		provider.SetUserModels(nil)
	})
	previous := discoverOpenCodeGoFn
	t.Cleanup(func() { discoverOpenCodeGoFn = previous })
	const (
		key     = "synthetic-zutfile-key"
		modelID = "new-preferred-model"
	)
	discoverOpenCodeGoFn = func(_ context.Context, apiKey, endpoint string) ([]provider.Model, error) {
		if apiKey != key || endpoint != "" {
			t.Fatalf("discovery inputs = key %q endpoint %q", apiKey, endpoint)
		}
		return []provider.Model{{
			Provider:      provider.ProviderOpenCodeGo,
			ID:            modelID,
			ContextWindow: 256000,
			MaxOutput:     32000,
		}}, nil
	}
	manifest := ZutfileManifest{}
	manifest.Model.MinContext = 200000
	manifest.Model.Preferred = []string{modelID}
	catalogProvider, catalogModel := zutfileCatalogSelection(Args{Provider: provider.ProviderOpenCodeGo}, manifest, nil)
	PrepareRuntimeCatalog(context.Background(), true, catalogProvider, key, "", nil, catalogModel)

	args := Args{}
	if err := applyZutfileModelRequirements(&args, manifest); err != nil {
		t.Fatal(err)
	}
	if args.Provider != provider.ProviderOpenCodeGo || args.Model != modelID {
		t.Fatalf("selected model = provider=%q model=%q, want %q/%q", args.Provider, args.Model, provider.ProviderOpenCodeGo, modelID)
	}
}

func TestResolveSDKUsesUserModelEndpointForDiscovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	t.Setenv("OPENCODE_API_KEY", "")
	provider.SetLiveModels(nil)
	provider.SetUserModels(nil)
	t.Cleanup(func() {
		provider.SetLiveModels(nil)
		provider.SetUserModels(nil)
	})

	const (
		key     = "synthetic-proxy-key"
		baseURL = "https://model-proxy.example/v1"
		modelID = "proxy-model"
	)
	if err := os.WriteFile(UserModelsPath(), []byte(`{"providers":{"opencode-go":{"models":[{"id":"`+modelID+`","baseUrl":"`+baseURL+`"}]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := discoverOpenCodeGoFn
	t.Cleanup(func() { discoverOpenCodeGoFn = previous })
	called := make(chan struct{}, 1)
	var gotKey, gotBaseURL string
	discoverOpenCodeGoFn = func(_ context.Context, apiKey, endpoint string) ([]provider.Model, error) {
		gotKey, gotBaseURL = apiKey, endpoint
		called <- struct{}{}
		return nil, fmt.Errorf("synthetic discovery stop")
	}

	resolved, err := ResolveSDK(context.Background(), Args{Provider: provider.ProviderOpenCodeGo, Model: modelID, APIKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.BaseURL != baseURL {
		t.Fatalf("resolved base URL = %q, want user model endpoint %q", resolved.BaseURL, baseURL)
	}
	select {
	case <-called:
		if gotKey != key || gotBaseURL != baseURL {
			t.Fatalf("discovery received key=%q endpoint=%q, want key=%q endpoint=%q", gotKey, gotBaseURL, key, baseURL)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OpenCode Go discovery was not invoked")
	}
}

func TestResolveSDKDoesNotInheritLiveEndpointForMetadataOnlyUserModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	t.Setenv("OPENCODE_API_KEY", "")
	provider.SetLiveModels([]provider.Model{{Provider: provider.ProviderOpenCodeGo, ID: "metadata-only", BaseURL: "https://old.example/v1", Source: "live"}})
	provider.SetUserModels(nil)
	t.Cleanup(func() {
		provider.SetLiveModels(nil)
		provider.SetUserModels(nil)
	})
	if err := os.WriteFile(UserModelsPath(), []byte(`{"providers":{"opencode-go":{"models":[{"id":"metadata-only","contextWindow":90000}]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	const key = "synthetic-metadata-key"
	previous := discoverOpenCodeGoFn
	t.Cleanup(func() { discoverOpenCodeGoFn = previous })
	called := make(chan string, 1)
	discoverOpenCodeGoFn = func(_ context.Context, apiKey, endpoint string) ([]provider.Model, error) {
		if apiKey != key {
			t.Errorf("discovery key = %q, want %q", apiKey, key)
		}
		called <- endpoint
		return nil, fmt.Errorf("synthetic discovery stop")
	}

	resolved, err := ResolveSDK(context.Background(), Args{Provider: provider.ProviderOpenCodeGo, Model: "metadata-only", APIKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.BaseURL != "" {
		t.Fatalf("metadata-only user model inherited base URL %q", resolved.BaseURL)
	}
	select {
	case endpoint := <-called:
		if endpoint != "" {
			t.Fatalf("discovery endpoint = %q, want default endpoint", endpoint)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OpenCode Go discovery was not invoked")
	}
}

func TestResolveSDKFollowsActiveModelProfile(t *testing.T) {
	const (
		key     = "synthetic-profile-key"
		baseURL = "https://profile-proxy.example/v1"
	)
	for _, tc := range []struct {
		name            string
		topProvider     string
		topModel        string
		profileProvider string
		profileModel    string
		wantContext     int
		wantCachedModel bool
	}{
		{
			name:            "profile switches to OpenCode Go",
			topProvider:     provider.ProviderAnthropic,
			topModel:        "claude-sonnet-4-5",
			profileProvider: provider.ProviderOpenCodeGo,
			profileModel:    "profile-opencode-model",
			wantContext:     654321,
			wantCachedModel: true,
		},
		{
			name:            "profile switches away from OpenCode Go",
			topProvider:     provider.ProviderOpenCodeGo,
			topModel:        "profile-opencode-model",
			profileProvider: provider.ProviderAnthropic,
			profileModel:    "claude-sonnet-4-5",
			wantCachedModel: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("ZUT_HOME", home)
			provider.SetLiveModels(nil)
			provider.SetUserModels(nil)
			t.Cleanup(func() {
				provider.SetLiveModels(nil)
				provider.SetUserModels(nil)
			})

			if err := SaveConfig(Config{
				Provider:           tc.topProvider,
				Model:              tc.topModel,
				ActiveModelProfile: 1,
				QuickModelShortcuts: []QuickModelShortcut{{
					Provider: tc.profileProvider,
					Model:    tc.profileModel,
				}},
			}); err != nil {
				t.Fatal(err)
			}
			if tc.wantCachedModel {
				if err := provider.SaveCache(filepath.Join(home, "models-cache.json"), provider.ModelCache{
					Version:                provider.ModelCacheVersion,
					FetchedAt:              time.Now().UTC(),
					Models:                 []provider.Model{{Provider: provider.ProviderOpenCodeGo, ID: tc.profileModel, ContextWindow: tc.wantContext, MaxOutput: 8192, BaseURL: baseURL}},
					AuthoritativeProviders: []string{provider.ProviderOpenCodeGo},
					ProviderScopes:         map[string]string{provider.ProviderOpenCodeGo: credentialScopeForEndpoint(key, baseURL)},
				}); err != nil {
					t.Fatal(err)
				}
			}

			modelCatalogMu.Lock()
			_, _ = prepareRuntimeCatalog("", key, baseURL, nil, "")
			resolved, err := Resolve(Args{APIKey: key, BaseURL: baseURL}, false)
			modelCatalogMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Provider != tc.profileProvider || resolved.Model != tc.profileModel {
				t.Fatalf("resolved profile = provider=%q model=%q, want provider=%q model=%q", resolved.Provider, resolved.Model, tc.profileProvider, tc.profileModel)
			}
			if tc.wantCachedModel && resolved.ContextWindow != tc.wantContext {
				t.Fatalf("cached OpenCode Go context = %d, want %d", resolved.ContextWindow, tc.wantContext)
			}
		})
	}
}

func TestEligibleProviderDiscoveryIncompleteIncludesUnscopedProviders(t *testing.T) {
	eligible := map[string]struct{}{
		provider.ProviderOpenAI:     {},
		provider.ProviderOpenCodeGo: {},
	}
	discovered := map[string][]provider.Model{
		provider.ProviderOpenCodeGo: {{Provider: provider.ProviderOpenCodeGo, ID: "served"}},
	}
	if !eligibleProviderDiscoveryIncomplete(eligible, discovered) {
		t.Fatal("missing unscoped provider discovery was not detected")
	}
	discovered[provider.ProviderOpenAI] = nil
	if eligibleProviderDiscoveryIncomplete(eligible, discovered) {
		t.Fatal("providers present in discovery were treated as missing")
	}
}

func TestPartialOpenCodeGoRefreshPreservesFullCatalogFreshness(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	provider.SetLiveModels(nil)
	t.Cleanup(func() { provider.SetLiveModels(nil) })

	const (
		key     = "synthetic-partial-key"
		baseURL = "https://partial-proxy.example/v1"
	)
	staleAt := time.Now().Add(-48 * time.Hour)
	if err := provider.SaveCache(ModelCachePath(), provider.ModelCache{
		Version:   provider.ModelCacheVersion,
		FetchedAt: staleAt,
		Models: []provider.Model{
			{Provider: provider.ProviderOpenAI, ID: "cached-openai"},
			{Provider: provider.ProviderOpenCodeGo, ID: "old-opencode"},
		},
		AuthoritativeProviders: []string{provider.ProviderOpenCodeGo},
		ProviderScopes:         map[string]string{provider.ProviderOpenCodeGo: credentialScopeForEndpoint(key, baseURL)},
	}); err != nil {
		t.Fatal(err)
	}
	previous := discoverOpenCodeGoFn
	t.Cleanup(func() { discoverOpenCodeGoFn = previous })
	calls := 0
	discoverOpenCodeGoFn = func(_ context.Context, apiKey, endpoint string) ([]provider.Model, error) {
		calls++
		if apiKey != key || endpoint != baseURL {
			t.Fatalf("discovery inputs = key %q endpoint %q", apiKey, endpoint)
		}
		return []provider.Model{{Provider: provider.ProviderOpenCodeGo, ID: "new-opencode", Source: "live"}}, nil
	}

	refreshModels(provider.ProviderOpenCodeGo, key, baseURL, provider.ProviderOpenCodeGo)
	partial, err := provider.LoadCache(ModelCachePath())
	if err != nil {
		t.Fatal(err)
	}
	if partial.IsFresh() {
		t.Fatal("partial OpenCode Go refresh made a stale full catalog fresh")
	}
	if _, ok := findCachedModel(partial.Models, provider.ProviderOpenAI, "cached-openai"); !ok {
		t.Fatalf("partial refresh dropped unrelated cached model: %+v", partial.Models)
	}

	refreshModels(provider.ProviderOpenCodeGo, key, baseURL, "")
	if calls != 2 {
		t.Fatalf("full refresh calls = %d, want a refresh after partial stale cache", calls)
	}
}

func findCachedModel(models []provider.Model, providerName, id string) (provider.Model, bool) {
	for _, model := range models {
		if model.Provider == providerName && model.ID == id {
			return model, true
		}
	}
	return provider.Model{}, false
}

func TestNeedsOpenCodeGoRefreshWhenCredentialAppearsAfterCache(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("OPENCODE_API_KEY", "synthetic-key")

	freshWithoutOpenCode := provider.ModelCache{
		Version:   provider.ModelCacheVersion,
		FetchedAt: time.Now(),
		Models:    []provider.Model{{Provider: "openai", ID: "gpt-5"}},
	}
	if !needsOpenCodeGoRefresh(freshWithoutOpenCode) {
		t.Fatal("fresh cache without OpenCode Go should refresh after credential appears")
	}

	freshWithOpenCode := freshWithoutOpenCode
	freshWithOpenCode.Models = append(freshWithOpenCode.Models, provider.Model{Provider: "opencode-go", ID: "served"})
	if needsOpenCodeGoRefresh(freshWithOpenCode) {
		t.Fatal("cache with OpenCode Go models should remain fresh")
	}

	t.Setenv("OPENCODE_API_KEY", "")
	if needsOpenCodeGoRefresh(freshWithoutOpenCode) {
		t.Fatal("cache without an OpenCode Go credential should not refresh")
	}
}

func TestValidateAndRepairConfig_DuplicateModelIDValidForConfiguredProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)

	b, _ := json.Marshal(Config{Provider: "openai-codex", Model: "gpt-5.5"})
	_ = os.WriteFile(filepath.Join(home, "config.json"), b, 0o644)

	ValidateAndRepairConfig()

	out, _ := LoadConfig()
	if out.Provider != "openai-codex" {
		t.Errorf("provider mutated: %q", out.Provider)
	}
	if out.Model != "gpt-5.5" {
		t.Errorf("model mutated: %q", out.Model)
	}
}

func TestValidateAndRepairConfig_OpenRouterPreservesRoutedModelID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)

	want := "deepseek/deepseek-v4-flash"
	b, _ := json.Marshal(Config{Provider: "openrouter", Model: want})
	_ = os.WriteFile(filepath.Join(home, "config.json"), b, 0o644)

	ValidateAndRepairConfig()

	out, _ := LoadConfig()
	if out.Provider != "openrouter" {
		t.Errorf("provider mutated: %q", out.Provider)
	}
	if out.Model != want {
		t.Errorf("routed model id mutated: got %q, want %q", out.Model, want)
	}
}

func TestValidateAndRepairConfig_GatewayPlainUnknownModelStillRepairs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)

	b, _ := json.Marshal(Config{Provider: "openrouter", Model: "not-a-routed-model"})
	_ = os.WriteFile(filepath.Join(home, "config.json"), b, 0o644)

	ValidateAndRepairConfig()

	out, _ := LoadConfig()
	if out.Provider != "openrouter" {
		t.Errorf("provider mutated: %q", out.Provider)
	}
	if out.Model == "not-a-routed-model" || out.Model == "" {
		t.Errorf("plain unknown gateway model was not repaired: %q", out.Model)
	}
}

// TestValidateAndRepairConfig_HappyPath leaves a valid config alone.
func TestValidateAndRepairConfig_HappyPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)

	b, _ := json.Marshal(Config{Provider: "anthropic", Model: "claude-sonnet-4-5"})
	_ = os.WriteFile(filepath.Join(home, "config.json"), b, 0o644)

	ValidateAndRepairConfig()

	out, _ := LoadConfig()
	if out.Provider != "anthropic" {
		t.Errorf("provider mutated: %q", out.Provider)
	}
	if out.Model != "claude-sonnet-4-5" {
		t.Errorf("model mutated: %q", out.Model)
	}
}
