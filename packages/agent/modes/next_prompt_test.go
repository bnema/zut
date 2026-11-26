package modes

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Like the codemode setting tests, embed the unused base contract.
type nextPromptStoreRecorder struct {
	SettingsStore
	calls []bool
}

func (r *nextPromptStoreRecorder) SetNextPromptSuggestionsEnabled(enabled bool) error {
	r.calls = append(r.calls, enabled)
	return nil
}

func TestNextPromptSuggestionsSettingDefaultsEnabledAndToggles(t *testing.T) {
	store := &nextPromptStoreRecorder{}
	i := NewInteractive(InteractiveConfig{Terminal: &alertTestTerminal{}, SettingsStore: store})
	i.openSettingsDialog()
	var item *settingsItem
	for idx := range i.settingsDialog.items {
		if i.settingsDialog.items[idx].key == "next_prompt_suggestions_enabled" {
			item = &i.settingsDialog.items[idx]
			break
		}
	}
	require.NotNil(t, item, "next prompt suggestions setting should be listed")
	require.True(t, item.value, "next prompt suggestions should be enabled by default")

	i.ed.SetGhost("run the tests")
	require.Equal(t, "run the tests", i.ed.Ghost())
	i.applySettingToggle("next_prompt_suggestions_enabled", false)
	require.NotNil(t, i.cfg.NextPromptSuggestionsEnabled)
	require.False(t, *i.cfg.NextPromptSuggestionsEnabled)
	require.Empty(t, i.ed.Ghost(), "disabling must clear the shown suggestion")

	i.applySettingToggle("next_prompt_suggestions_enabled", true)
	require.True(t, *i.cfg.NextPromptSuggestionsEnabled)
	require.Equal(t, []bool{false, true}, store.calls)
}
