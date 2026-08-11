package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/bnema/zut/packages/provider"
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
