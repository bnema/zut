package agent

import (
	"bytes"
	"os"
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/provider"
	"github.com/stretchr/testify/require"
)

func TestTUIClientModelCatalogWithStoredCredentials(t *testing.T) {
	preserveProviderCatalog(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_OAUTH_TOKEN", "")
	t.Setenv("ZUT_THEME", "dark")
	t.Setenv("ANTHROPIC_API_KEY", "")
	require.NoError(t, os.WriteFile(UserModelsPath(), []byte(`{"providers":{"keyless-test":{"baseUrl":"http://127.0.0.1:9/v1","models":[{"id":"local-model"}]}}}`), 0600))
	store := AuthStoreFor()
	require.NoError(t, store.SetAPIKey("anthropic", "synthetic-test-key"))
	for _, launch := range []*session.CreateSpec{{}, nil} {
		if launch != nil {
			// Normal and zutfile entry points load user metadata before
			// PrepareRuntimeCatalog and TUI construction.
			LoadUserModels()
		} else {
			// Attach must load user metadata itself, not inherit the
			// preceding launch's in-process catalog.
			provider.SetUserModels(nil)
		}
		cfg := tuiClientConfig(&bytes.Buffer{}, launch)
		t.Cleanup(cfg.AuthManager.Close)
		require.NotNil(t, cfg.LoggedInProviders)
		names := cfg.LoggedInProviders()
		require.Contains(t, names, "anthropic")
		require.Contains(t, names, "ollama")
		require.Contains(t, names, "keyless-test")
		// The dialog filters Active by these names: verify its input includes
		// reachable models, not merely a non-empty credential list.
		var models []provider.Model
		for _, m := range provider.Active() {
			for _, name := range names {
				if m.Provider == name {
					models = append(models, m)
					break
				}
			}
		}
		require.NotEmpty(t, models)
	}
}

func TestTUIClientConfigPreservesPreparedCatalog(t *testing.T) {
	preserveProviderCatalog(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("ZUT_THEME", "dark")
	// Represent the endpoint-scoped overlay prepared by normal/zutfile
	// launch before TUI setup. Loading a default-scope cache would erase it.
	model := provider.Model{Provider: provider.ProviderOpenCodeGo, ID: "prepared-endpoint-model", DisplayName: "prepared-endpoint-model"}
	provider.SetLiveModelsForProviders([]provider.Model{model}, []string{provider.ProviderOpenCodeGo})
	cfg := tuiClientConfig(&bytes.Buffer{}, &session.CreateSpec{})
	t.Cleanup(cfg.AuthManager.Close)
	require.Contains(t, provider.ModelsForProvider(provider.ProviderOpenCodeGo), model)
}
