package agent

import (
	"bytes"
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/provider"
	"github.com/stretchr/testify/require"
)

func TestTUIClientModelCatalogWithStoredCredentials(t *testing.T) {
	preserveProviderCatalog(t)
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("ZUT_THEME", "dark")
	t.Setenv("ANTHROPIC_API_KEY", "")
	store := AuthStoreFor()
	require.NoError(t, store.SetAPIKey("anthropic", "synthetic-test-key"))
	for _, launch := range []*session.CreateSpec{nil, {}} {
		cfg := tuiClientConfig(&bytes.Buffer{}, launch)
		require.NotNil(t, cfg.LoggedInProviders)
		names := cfg.LoggedInProviders()
		require.Contains(t, names, "anthropic")
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
		cfg.AuthManager.Close()
	}
}
