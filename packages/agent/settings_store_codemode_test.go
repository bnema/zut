package agent

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSettingsStoreCodemode(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	store := configSettingsStore{}
	enabled, err := store.CodemodeEnabled()
	require.NoError(t, err)
	require.False(t, enabled)
	// A missing experimental section is created on the first write.
	require.NoError(t, store.SetCodemodeEnabled(true))
	require.NoError(t, updateConfig(func(c *Config) error {
		c.Model = "preserved-model"
		deferred := false
		c.Experimental.CodemodeDeferMCP = &deferred
		return nil
	}))
	for _, want := range []bool{false, true, false} {
		require.NoError(t, store.SetCodemodeEnabled(want))
		data, err := os.ReadFile(ConfigPath())
		require.NoError(t, err)
		var persisted struct {
			Model        string `json:"model"`
			Experimental struct {
				Codemode *bool `json:"codemode"`
				Defer    *bool `json:"codemode_defer_mcp"`
			} `json:"experimental"`
		}
		require.NoError(t, json.Unmarshal(data, &persisted))
		require.NotNil(t, persisted.Experimental.Codemode)
		require.Equal(t, want, *persisted.Experimental.Codemode)
		require.NotNil(t, persisted.Experimental.Defer)
		require.False(t, *persisted.Experimental.Defer)
		require.Equal(t, "preserved-model", persisted.Model)
		enabled, err = store.CodemodeEnabled()
		require.NoError(t, err)
		require.Equal(t, want, enabled)
	}
}
