package modes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// newPortSettingsInteractive is a daemon-path TUI with the settings dialog
// open. The daemon persists daemon-owned toggles and refreshes its tools
// (session TestApplySettingsPersistsAndRefreshes); the TUI only applies the
// launch-time limits and mirrors the applied value.
func newPortSettingsInteractive(t *testing.T, cfg InteractiveConfig) (*Interactive, *sessionmocks.MockPort) {
	t.Helper()
	port := sessionmocks.NewMockPort(t)
	cfg.Port = port
	cfg.Terminal = &alertTestTerminal{}
	i := NewInteractive(cfg)
	i.rend = nil
	i.openSettingsDialog()
	return i, port
}

// expectApplySettings records each patch and returns the scripted errors.
func expectApplySettings(port *sessionmocks.MockPort, errs ...error) <-chan session.SettingsPatch {
	got := make(chan session.SettingsPatch, len(errs))
	for _, err := range errs {
		port.EXPECT().ApplySettings(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, p session.SettingsPatch) error {
			got <- p
			return err
		}).Once()
	}
	return got
}

func settingsRow(t *testing.T, i *Interactive, key string) settingsItem {
	t.Helper()
	item := findSettingsItem(i.settingsDialog.items, key)
	require.NotNil(t, item, "settings dialog is missing %s", key)
	return *item
}

func waitStatus(t *testing.T, i *Interactive, ok func(statusOK, statusErr string) bool) {
	t.Helper()
	require.Eventually(t, func() bool {
		i.mu.Lock()
		defer i.mu.Unlock()
		return ok(i.statusOK, i.statusErr)
	}, 5*time.Second, 5*time.Millisecond)
}

func TestSettingsDialogOffersWebSearchEnabledByDefault(t *testing.T) {
	i, _ := newPortSettingsInteractive(t, InteractiveConfig{})
	item := settingsRow(t, i, "web_search_enabled")
	require.True(t, item.value, "web search should be enabled when the config value is absent")
	require.False(t, item.disabled, "the daemon persists and refreshes web search")
}

func TestSettingsDialogDisablesWebSearchAtInvocationCapabilityCeiling(t *testing.T) {
	allowed := false
	i, _ := newPortSettingsInteractive(t, InteractiveConfig{WebSearchToolAllowed: &allowed})
	item := settingsRow(t, i, "web_search_enabled")
	require.False(t, item.value, "web search was reported enabled despite the invocation ceiling")
	require.True(t, item.disabled)
	require.Contains(t, item.hint, "launch-time tool capability ceiling")

	// The strict mock fails on any ApplySettings call.
	i.applySettingToggle("web_search_enabled", true)
	require.Contains(t, i.statusErr, "web search unavailable")
}

func TestSettingsDialogExplainsExplicitWebSearchInvocationOverride(t *testing.T) {
	persisted := false
	i, _ := newPortSettingsInteractive(t, InteractiveConfig{WebSearchEnabled: &persisted, WebSearchInvocationOverride: true})
	item := settingsRow(t, i, "web_search_enabled")
	require.True(t, item.value)
	require.True(t, item.disabled)
	require.Contains(t, item.hint, "explicit --tools web_search")
	require.Contains(t, item.hint, "without --tools")

	i.applySettingToggle("web_search_enabled", false)
	require.NotEmpty(t, i.statusErr, "explicit override toggle should explain why the row is read-only")
}

func TestSettingsDialogEnablesWebSearchWithoutLocalCapabilities(t *testing.T) {
	// Production never gives the daemon client a SettingsStore web-search
	// setter or RefreshTools: availability must not depend on them.
	i, _ := newPortSettingsInteractive(t, InteractiveConfig{})
	item := settingsRow(t, i, "web_search_enabled")
	require.False(t, item.disabled)
	require.Empty(t, item.hint)
}

func TestApplyWebSearchToggleRefreshesAndPersists(t *testing.T) {
	i, port := newPortSettingsInteractive(t, InteractiveConfig{})
	got := expectApplySettings(port, nil, nil)

	i.applySettingChange(settingsAction{Toggle: true, Key: "web_search_enabled", Value: false})
	p := <-got
	require.NotNil(t, p.WebSearchEnabled)
	require.False(t, *p.WebSearchEnabled)
	waitStatus(t, i, func(ok, _ string) bool { return ok == "web search disabled" })
	require.False(t, i.webSearchEnabled())

	i.applySettingChange(settingsAction{Toggle: true, Key: "web_search_enabled", Value: true})
	p = <-got
	require.True(t, *p.WebSearchEnabled)
	waitStatus(t, i, func(ok, _ string) bool { return ok == "web search enabled" })
	require.True(t, i.webSearchEnabled())
}

func TestApplyWebSearchToggleKeepsAppliedStateWhenRefreshFails(t *testing.T) {
	// The daemon stores the value before refreshing (session.refreshApplied):
	// a refresh failure keeps the persisted setting and reports the error.
	i, port := newPortSettingsInteractive(t, InteractiveConfig{})
	expectApplySettings(port, errors.New("setting applied, but refresh tools: refresh failed"))

	i.applySettingChange(settingsAction{Toggle: true, Key: "web_search_enabled", Value: false})
	waitStatus(t, i, func(_, e string) bool { return strings.Contains(e, "refresh failed") })
	require.False(t, i.webSearchEnabled())
}

func TestApplyWebSearchToggleKeepsStateWhenPersistenceFails(t *testing.T) {
	i, port := newPortSettingsInteractive(t, InteractiveConfig{})
	expectApplySettings(port, errors.New("disk full"))

	i.applySettingChange(settingsAction{Toggle: true, Key: "web_search_enabled", Value: false})
	waitStatus(t, i, func(_, e string) bool { return strings.Contains(e, "disk full") })
	require.True(t, i.webSearchEnabled(), "web search changed despite persistence failure")
	i.mu.Lock()
	defer i.mu.Unlock()
	require.True(t, findSettingsItem(i.settingsDialog.items, "web_search_enabled").value, "dialog row not reverted")
}

func TestApplyAutoSubagentsToggleOnDaemonPath(t *testing.T) {
	i, port := newPortSettingsInteractive(t, InteractiveConfig{})
	item := settingsRow(t, i, "auto_subagents_enabled")
	require.False(t, item.disabled, "the daemon session owns the resident manager")
	got := expectApplySettings(port, nil)

	i.applySettingChange(settingsAction{Toggle: true, Key: "auto_subagents_enabled", Value: true})
	p := <-got
	require.NotNil(t, p.AutoSubagentsEnabled)
	require.True(t, *p.AutoSubagentsEnabled)
	waitStatus(t, i, func(ok, _ string) bool { return ok == "subagent orchestrator enabled" })
	i.mu.Lock()
	defer i.mu.Unlock()
	require.True(t, *i.cfg.AutoSubagentsEnabled)
}

func TestApplyAutoSubagentsToggleKeepsStateWhenPersistenceFails(t *testing.T) {
	i, port := newPortSettingsInteractive(t, InteractiveConfig{})
	expectApplySettings(port, errors.New("disk full"))
	i.applySettingToggle("auto_subagents_enabled", true)
	waitStatus(t, i, func(_, e string) bool { return strings.Contains(e, "disk full") })
	i.mu.Lock()
	defer i.mu.Unlock()
	require.False(t, i.cfg.AutoSubagentsEnabled != nil && *i.cfg.AutoSubagentsEnabled)
	require.False(t, findSettingsItem(i.settingsDialog.items, "auto_subagents_enabled").value)
}

func TestApplyWebSearchToggleExplainsOldDaemon(t *testing.T) {
	i, port := newPortSettingsInteractive(t, InteractiveConfig{})
	expectApplySettings(port, errors.New(`unsupported port method "apply_settings"`))
	i.applySettingToggle("web_search_enabled", false)
	waitStatus(t, i, func(_, e string) bool { return strings.Contains(e, "restart the daemon") })
	i.mu.Lock()
	defer i.mu.Unlock()
	require.True(t, i.webSearchEnabled())
	require.True(t, findSettingsItem(i.settingsDialog.items, "web_search_enabled").value)
}

func TestAutoSubagentsToggleHonorsLaunchToolPolicyOnDaemonPath(t *testing.T) {
	deny := false
	i, _ := newPortSettingsInteractive(t, InteractiveConfig{
		AutoSubagentsToolAllowed: &deny, AutoSubagentsStatusToolAllowed: &deny, AutoSubagentsStopToolAllowed: &deny,
		AutoSubagentsResumeToolAllowed: &deny, AutoSubagentsInterruptToolAllowed: &deny,
	})
	item := settingsRow(t, i, "auto_subagents_enabled")
	require.True(t, item.disabled)
	require.Contains(t, item.hint, "launch-time tool policy")
	i.applySettingToggle("auto_subagents_enabled", true)
	require.Contains(t, i.statusErr, "subagent orchestrator unavailable")
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
