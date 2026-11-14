package modes

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// Like the existing LSP setting tests, embed the unused base contract.
type codemodeTestSettingsStore struct {
	SettingsStore
	enabled  bool
	readErr  error
	writeErr error
}

func (s *codemodeTestSettingsStore) CodemodeEnabled() (bool, error) { return s.enabled, s.readErr }
func (s *codemodeTestSettingsStore) SetCodemodeEnabled(enabled bool) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	s.enabled = enabled
	return nil
}

func TestSettingsCodemodeToggle(t *testing.T) {
	store := &codemodeTestSettingsStore{}
	i, _ := newPortSettingsInteractive(t, InteractiveConfig{
		SettingsStore: store,
		RefreshTools:  func() error { t.Fatal("codemode must not refresh live tools"); return nil },
	})
	row := settingsRow(t, i, "experimental_codemode")
	require.Equal(t, "codemode (experimental)", row.label)
	require.Contains(t, row.desc, "host access")
	require.Contains(t, row.desc, "new sessions")
	require.False(t, row.disabled)
	require.False(t, row.value)
	for _, enabled := range []bool{true, false} {
		i.applySettingToggle("experimental_codemode", enabled)
		require.Equal(t, enabled, store.enabled)
		require.Equal(t, enabled, settingsRow(t, i, "experimental_codemode").value)
		require.Contains(t, i.statusOK, "applies to new sessions")
		require.Empty(t, i.statusErr)
		i.openSettingsDialog()
		require.Equal(t, enabled, settingsRow(t, i, "experimental_codemode").value)
	}
	store.writeErr = errors.New("disk full")
	i.resetSettingsToggle("experimental_codemode", true)
	i.applySettingToggle("experimental_codemode", true)
	require.False(t, store.enabled)
	require.False(t, settingsRow(t, i, "experimental_codemode").value)
	require.Contains(t, i.statusErr, "disk full")
	require.Empty(t, i.statusOK)
}

func TestSettingsCodemodeUnavailable(t *testing.T) {
	i, _ := newPortSettingsInteractive(t, InteractiveConfig{})
	require.True(t, settingsRow(t, i, "experimental_codemode").disabled)
	i.applySettingToggle("experimental_codemode", true)
	require.Contains(t, i.statusErr, "requires persistent settings")
}

func TestSettingsCodemodeReadFailure(t *testing.T) {
	store := &codemodeTestSettingsStore{readErr: errors.New("invalid config")}
	i, _ := newPortSettingsInteractive(t, InteractiveConfig{SettingsStore: store})
	require.True(t, settingsRow(t, i, "experimental_codemode").disabled)
	require.Contains(t, settingsRow(t, i, "experimental_codemode").hint, "invalid config")
	i.applySettingToggle("experimental_codemode", true)
	require.False(t, store.enabled)
	require.Contains(t, i.statusErr, "invalid config")
}
