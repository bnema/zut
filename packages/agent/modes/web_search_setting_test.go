package modes

import (
	"errors"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

type recordingWebSearchSettingsStore struct {
	SettingsStore
	values   []bool
	calls    int
	err      error
	failures map[int]error
}

func (s *recordingWebSearchSettingsStore) SetWebSearchEnabled(enabled bool) error {
	s.calls++
	if s.err != nil {
		return s.err
	}
	if err := s.failures[s.calls]; err != nil {
		return err
	}
	s.values = append(s.values, enabled)
	return nil
}

func TestSettingsDialogOffersWebSearchEnabledByDefault(t *testing.T) {
	store := &recordingWebSearchSettingsStore{}
	interactive := NewInteractive(InteractiveConfig{
		SettingsStore: store,
		RefreshTools:  func() error { return nil },
	})
	interactive.openSettingsDialog()

	item := findSettingsItem(interactive.settingsDialog.items, "web_search_enabled")
	if item == nil {
		t.Fatal("settings dialog is missing web search")
	}
	if !item.value {
		t.Fatal("web search should be enabled when the config value is absent")
	}
	if item.disabled {
		t.Fatal("web search should be available with persistence and refresh capabilities")
	}
}

func TestSettingsDialogDisablesWebSearchAtInvocationCapabilityCeiling(t *testing.T) {
	allowed := false
	store := &recordingWebSearchSettingsStore{}
	refreshCalls := 0
	interactive := NewInteractive(InteractiveConfig{
		WebSearchToolAllowed: &allowed,
		SettingsStore:        store,
		RefreshTools: func() error {
			refreshCalls++
			return nil
		},
	})
	interactive.rend = nil
	interactive.openSettingsDialog()

	item := findSettingsItem(interactive.settingsDialog.items, "web_search_enabled")
	if item == nil {
		t.Fatal("settings dialog is missing web search")
	}
	if item.value {
		t.Fatal("web search was reported enabled despite the invocation ceiling")
	}
	if !item.disabled || !strings.Contains(item.hint, "launch-time tool capability ceiling") {
		t.Fatalf("ceiling item = disabled %v hint %q", item.disabled, item.hint)
	}

	interactive.applySettingToggle("web_search_enabled", true)
	if store.calls != 0 || refreshCalls != 0 {
		t.Fatalf("ceiling toggle persisted %d times and refreshed %d times", store.calls, refreshCalls)
	}
}

func TestSettingsDialogExplainsExplicitWebSearchInvocationOverride(t *testing.T) {
	persisted := false
	store := &recordingWebSearchSettingsStore{}
	refreshCalls := 0
	interactive := NewInteractive(InteractiveConfig{
		WebSearchEnabled:            &persisted,
		WebSearchInvocationOverride: true,
		SettingsStore:               store,
		RefreshTools: func() error {
			refreshCalls++
			return nil
		},
	})
	interactive.rend = nil
	interactive.openSettingsDialog()

	item := findSettingsItem(interactive.settingsDialog.items, "web_search_enabled")
	if item == nil {
		t.Fatal("settings dialog is missing web search")
	}
	if !item.value || !item.disabled {
		t.Fatalf("explicit override item = value %v disabled %v, want true and true", item.value, item.disabled)
	}
	if !strings.Contains(item.hint, "explicit --tools web_search") || !strings.Contains(item.hint, "without --tools") {
		t.Fatalf("explicit override hint = %q", item.hint)
	}

	interactive.applySettingToggle("web_search_enabled", false)
	if store.calls != 0 || refreshCalls != 0 {
		t.Fatalf("explicit override toggle persisted %d times and refreshed %d times", store.calls, refreshCalls)
	}
	if interactive.statusErr == "" {
		t.Fatal("explicit override toggle should explain why the row is read-only")
	}
}

func TestSettingsDialogDisablesWebSearchWithoutCapabilities(t *testing.T) {
	cases := []struct {
		name string
		cfg  InteractiveConfig
	}{
		{name: "neither capability", cfg: InteractiveConfig{}},
		{name: "persistence only", cfg: InteractiveConfig{SettingsStore: &recordingWebSearchSettingsStore{}}},
		{name: "refresh only", cfg: InteractiveConfig{RefreshTools: func() error { return nil }}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			interactive := NewInteractive(tc.cfg)
			interactive.openSettingsDialog()

			item := findSettingsItem(interactive.settingsDialog.items, "web_search_enabled")
			if item == nil {
				t.Fatal("settings dialog is missing web search")
			}
			if !item.disabled {
				t.Fatal("web search should be disabled without both capabilities")
			}
			if item.hint == "" {
				t.Fatal("disabled web search should explain why it is unavailable")
			}
		})
	}
}

func TestApplyWebSearchToggleRefreshesAndPersists(t *testing.T) {
	store := &recordingWebSearchSettingsStore{}
	refreshCalls := 0
	interactive := NewInteractive(InteractiveConfig{
		SettingsStore: store,
		RefreshTools: func() error {
			refreshCalls++
			return nil
		},
	})
	interactive.rend = nil

	interactive.applySettingToggle("web_search_enabled", false)
	if interactive.webSearchEnabled() {
		t.Fatal("web search remained enabled after disabling it")
	}
	interactive.applySettingToggle("web_search_enabled", true)
	if !interactive.webSearchEnabled() {
		t.Fatal("web search remained disabled after enabling it")
	}
	if refreshCalls != 2 {
		t.Fatalf("refresh calls = %d, want 2", refreshCalls)
	}
	if len(store.values) != 2 || store.values[0] || !store.values[1] {
		t.Fatalf("persisted values = %v, want [false true]", store.values)
	}
	if interactive.statusErr != "" {
		t.Fatalf("unexpected status error: %q", interactive.statusErr)
	}
}

func TestApplyWebSearchToggleRollsBackWhenRefreshFails(t *testing.T) {
	store := &recordingWebSearchSettingsStore{}
	refreshCalls := 0
	interactive := NewInteractive(InteractiveConfig{
		SettingsStore: store,
		RefreshTools: func() error {
			refreshCalls++
			if refreshCalls == 1 {
				return errors.New("refresh failed")
			}
			return nil
		},
	})
	interactive.rend = nil

	interactive.applySettingToggle("web_search_enabled", false)
	if !interactive.webSearchEnabled() {
		t.Fatal("web search stayed disabled after refresh rollback")
	}
	if refreshCalls != 2 {
		t.Fatalf("refresh calls = %d, want initial refresh and rollback refresh", refreshCalls)
	}
	if len(store.values) != 2 || store.values[0] || !store.values[1] {
		t.Fatalf("persisted rollback values = %v, want [false true]", store.values)
	}
	if !strings.Contains(interactive.statusErr, "refresh failed") {
		t.Fatalf("status error = %q, want refresh failure", interactive.statusErr)
	}
}

func TestApplyWebSearchToggleFailsClosedWhenRollbackRefreshFails(t *testing.T) {
	store := &recordingWebSearchSettingsStore{}
	agent := core.NewAgent(nil, "model", "", core.Registry{
		"web_search": tools.NewWebSearchTool(),
	})
	var availability []bool
	interactive := NewInteractive(InteractiveConfig{
		Agent:         agent,
		SettingsStore: store,
		RefreshTools:  func() error { return errors.New("refresh failed") },
		SetWebSearchAvailable: func(available bool) {
			availability = append(availability, available)
		},
	})
	interactive.rend = nil

	interactive.applySettingToggle("web_search_enabled", false)
	if !interactive.webSearchEnabled() {
		t.Fatal("persisted setting did not roll back to its prior value")
	}
	if _, ok := agent.ToolsSnapshot()["web_search"]; ok {
		t.Fatal("rollback refresh failure left web_search callable")
	}
	if len(availability) == 0 || availability[len(availability)-1] {
		t.Fatalf("generic-child/live availability = %v, want final deny", availability)
	}
	if !strings.Contains(interactive.statusErr, "rollback refresh") {
		t.Fatalf("status error = %q, want rollback refresh failure", interactive.statusErr)
	}
}

func TestApplyWebSearchToggleKeepsDurableStateWhenRollbackPersistenceFails(t *testing.T) {
	store := &recordingWebSearchSettingsStore{
		failures: map[int]error{2: errors.New("rollback disk full")},
	}
	refreshCalls := 0
	agent := core.NewAgent(nil, "model", "", core.Registry{
		"web_search": tools.NewWebSearchTool(),
	})
	var availability []bool
	interactive := NewInteractive(InteractiveConfig{
		Agent:         agent,
		SettingsStore: store,
		RefreshTools: func() error {
			refreshCalls++
			return errors.New("refresh failed")
		},
		SetWebSearchAvailable: func(available bool) {
			availability = append(availability, available)
		},
	})
	interactive.rend = nil

	interactive.applySettingToggle("web_search_enabled", false)
	if interactive.webSearchEnabled() {
		t.Fatal("in-memory setting reverted despite rollback persistence failure")
	}
	if len(store.values) != 1 || store.values[0] {
		t.Fatalf("durable writes = %v, want only the successful disable", store.values)
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want only the failed refresh", refreshCalls)
	}
	if _, ok := agent.ToolsSnapshot()["web_search"]; ok {
		t.Fatal("failed rollback left the old callable web_search registry in place")
	}
	if len(availability) == 0 || availability[len(availability)-1] {
		t.Fatalf("generic-child/live availability = %v, want final deny", availability)
	}
	if !strings.Contains(interactive.statusErr, "rollback disk full") {
		t.Fatalf("status error = %q, want rollback failure", interactive.statusErr)
	}
}

func TestStaleWebSearchResolveCannotCommitAfterPolicyChange(t *testing.T) {
	agent := core.NewAgent(nil, "model", "old system", core.Registry{
		"web_search": tools.NewWebSearchTool(),
	})
	var availability []bool
	interactive := NewInteractive(InteractiveConfig{
		Agent: agent,
		SetWebSearchAvailable: func(available bool) {
			availability = append(availability, available)
		},
	})

	startedAt := interactive.WebSearchPolicyGeneration()
	// Models a settings/Telegram revocation completing while Resolve is still
	// discovering config, extensions, and tools.
	interactive.stripWebSearchTool()
	_, applied := interactive.ApplyAgentPromptConfigAtWebSearchGeneration(agent, "stale system", core.Registry{
		"web_search": tools.NewWebSearchTool(),
	}, startedAt)
	if applied {
		t.Fatal("resolve started before the policy change committed its stale registry")
	}
	if _, ok := agent.ToolsSnapshot()["web_search"]; ok {
		t.Fatal("stale resolve reintroduced web_search")
	}
	system, _ := agent.PromptConfig()
	if system != "old system" {
		t.Fatalf("stale resolve changed system prompt to %q", system)
	}
	if len(availability) == 0 || availability[len(availability)-1] {
		t.Fatalf("stale resolve availability = %v, want final deny", availability)
	}
}

func TestApplyWebSearchToggleKeepsStateWhenPersistenceFails(t *testing.T) {
	store := &recordingWebSearchSettingsStore{err: errors.New("disk full")}
	refreshCalls := 0
	interactive := NewInteractive(InteractiveConfig{
		SettingsStore: store,
		RefreshTools: func() error {
			refreshCalls++
			return nil
		},
	})
	interactive.rend = nil

	interactive.applySettingToggle("web_search_enabled", false)
	if !interactive.webSearchEnabled() {
		t.Fatal("web search changed despite persistence failure")
	}
	if refreshCalls != 0 {
		t.Fatalf("refresh calls = %d, want none", refreshCalls)
	}
	if !strings.Contains(interactive.statusErr, "disk full") {
		t.Fatalf("status error = %q, want persistence failure", interactive.statusErr)
	}
}
