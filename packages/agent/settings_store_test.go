package agent

import (
	"strings"
	"testing"
)

func TestAutoSubagentsSystemAddendumForListsOnlyEnabledLifecycleTools(t *testing.T) {
	for _, tc := range []struct {
		name            string
		spawn           bool
		stop            bool
		resume          bool
		want            []string
		unwanted        []string
		wantUnavailable bool
	}{
		{name: "none", spawn: true, unwanted: []string{"subagent_stop", "subagent_resume"}},
		{name: "fully unavailable", unwanted: []string{"subagent_stop", "subagent_resume"}, wantUnavailable: true},
		{name: "stop", spawn: true, stop: true, want: []string{"subagent_stop"}, unwanted: []string{"subagent_resume"}},
		{name: "resume", spawn: true, resume: true, want: []string{"subagent_resume"}, unwanted: []string{"subagent_stop"}},
		{name: "both", spawn: true, stop: true, resume: true, want: []string{"subagent_stop", "subagent_resume"}},
		{name: "stop without spawn", stop: true, want: []string{"subagent_stop", "Spawning new workers is unavailable"}, unwanted: []string{"subagent_resume"}},
		{name: "resume without spawn", resume: true, want: []string{"subagent_resume", "Spawning new workers is unavailable"}, unwanted: []string{"subagent_stop"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := AutoSubagentsSystemAddendumFor(tc.spawn, tc.stop, tc.resume)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("addendum missing enabled lifecycle tool %q:\n%s", want, got)
				}
			}
			for _, unwanted := range tc.unwanted {
				if strings.Contains(got, unwanted) {
					t.Fatalf("addendum mentions unavailable lifecycle tool %q:\n%s", unwanted, got)
				}
			}
			if gotUnavailable := strings.Contains(got, AutoSubagentsDelegationUnavailableAddendum); gotUnavailable != tc.wantUnavailable {
				t.Fatalf("delegation-unavailable guidance present = %t, want %t:\n%s", gotUnavailable, tc.wantUnavailable, got)
			}
		})
	}
}

func TestConfigSettingsStorePersistsShowInstructionsAtStartup(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	if err := SaveConfig(Config{Theme: "dark"}); err != nil {
		t.Fatal(err)
	}

	if err := (configSettingsStore{}).SetShowInstructionsAtStartup(true); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ShowInstructionsAtStartup == nil || !*cfg.ShowInstructionsAtStartup {
		t.Fatal("show_instructions_at_startup was not persisted as enabled")
	}
	if cfg.Theme != "dark" {
		t.Fatalf("unrelated config changed: theme = %q, want dark", cfg.Theme)
	}
}

func TestConfigSettingsStorePersistsTerminalAlerts(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	if err := SaveConfig(Config{Theme: "dark"}); err != nil {
		t.Fatal(err)
	}

	if err := (configSettingsStore{}).SetTerminalAlertsEnabled(false); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TerminalAlertsEnabled == nil || *cfg.TerminalAlertsEnabled {
		t.Fatal("terminal_alerts_enabled was not persisted as disabled")
	}
	if cfg.Theme != "dark" {
		t.Fatalf("unrelated config changed: theme = %q, want dark", cfg.Theme)
	}
}

func TestConfigSettingsStorePersistsTerminalTitle(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	if err := SaveConfig(Config{Theme: "dark"}); err != nil {
		t.Fatal(err)
	}

	if err := (configSettingsStore{}).SetTerminalTitleEnabled(false); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TerminalTitleEnabled == nil || *cfg.TerminalTitleEnabled {
		t.Fatal("terminal_title_enabled was not persisted as disabled")
	}
	if cfg.Theme != "dark" {
		t.Fatalf("unrelated config changed: theme = %q, want dark", cfg.Theme)
	}
}

func TestConfigSettingsStorePersistsJailByDefault(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	if err := SaveConfig(Config{Theme: "dark"}); err != nil {
		t.Fatal(err)
	}

	if err := (configSettingsStore{}).SetJailByDefault(true); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JailByDefault == nil || !*cfg.JailByDefault {
		t.Fatal("jail_by_default was not persisted as enabled")
	}
	if cfg.Theme != "dark" {
		t.Fatalf("unrelated config changed: theme = %q, want dark", cfg.Theme)
	}
}

func TestConfigSettingsStorePersistsAutoCompactThreshold(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	if err := SaveConfig(Config{Theme: "dark"}); err != nil {
		t.Fatal(err)
	}

	if err := (configSettingsStore{}).SetAutoCompactThreshold(70); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AutoCompactThreshold == nil || *cfg.AutoCompactThreshold != 70 {
		t.Fatalf("auto_compact_threshold = %v, want 70", cfg.AutoCompactThreshold)
	}
	if cfg.Theme != "dark" {
		t.Fatalf("unrelated config changed: theme = %q, want dark", cfg.Theme)
	}
}

func TestConfigSettingsStorePersistsLSPDefaults(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	if err := SaveConfig(Config{Theme: "dark"}); err != nil {
		t.Fatal(err)
	}

	if err := (configSettingsStore{}).SetLSPEnabled(false); err != nil {
		t.Fatal(err)
	}
	if err := (configSettingsStore{}).SetSubagentLSPEnabled(false); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LSPEnabled == nil || *cfg.LSPEnabled {
		t.Fatal("lsp_enabled was not persisted as disabled")
	}
	if cfg.SubagentLSPEnabled == nil || *cfg.SubagentLSPEnabled {
		t.Fatal("subagent_lsp_enabled was not persisted as disabled")
	}
	if cfg.Theme != "dark" {
		t.Fatalf("unrelated config changed: theme = %q, want dark", cfg.Theme)
	}
}

func TestConfigLSPEnabledForDefaultsToTrue(t *testing.T) {
	cfg := Config{}
	if !cfg.LSPEnabledFor(false) {
		t.Fatal("main-session LSP should default to enabled")
	}
	if !cfg.LSPEnabledFor(true) {
		t.Fatal("sub-agent LSP should default to enabled")
	}

	noMain, noSub := false, false
	cfg.LSPEnabled = &noMain
	cfg.SubagentLSPEnabled = &noSub
	if cfg.LSPEnabledFor(false) || cfg.LSPEnabledFor(true) {
		t.Fatal("explicitly disabled LSP settings were ignored")
	}
	yesSub := true
	cfg.SubagentLSPEnabled = &yesSub
	write, edit := true, true
	cfg.LSPDiagnosticsOnWrite = &write
	cfg.LSPDiagnosticsOnEdit = &edit
	if cfg.LSPDiagnosticsOnWriteEnabled(false) || cfg.LSPDiagnosticsOnEditEnabled(false) {
		t.Fatal("diagnostics remained enabled with main-session LSP disabled")
	}
	if !cfg.LSPDiagnosticsOnWriteEnabled(true) || !cfg.LSPDiagnosticsOnEditEnabled(true) {
		t.Fatal("sub-agent diagnostics did not follow sub-agent LSP settings")
	}
}

func TestConfigSettingsStorePersistsInheritedTheme(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	if err := SaveConfig(Config{Theme: "dark"}); err != nil {
		t.Fatal(err)
	}

	if err := (configSettingsStore{}).SetTheme("inherited"); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Theme != "inherited" {
		t.Fatalf("theme = %q, want inherited", cfg.Theme)
	}
}

func TestConfigWebSearchEnabledForCLIDefaultsToTrue(t *testing.T) {
	cfg := Config{}
	if !cfg.WebSearchEnabledForCLI() {
		t.Fatal("web search should default to enabled when the config field is absent")
	}
	disabled := false
	cfg.WebSearchEnabled = &disabled
	if cfg.WebSearchEnabledForCLI() {
		t.Fatal("explicitly disabled web search setting was ignored")
	}
}

func TestConfigSettingsStorePersistsWebSearch(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	if err := SaveConfig(Config{Theme: "dark"}); err != nil {
		t.Fatal(err)
	}

	if err := (configSettingsStore{}).SetWebSearchEnabled(false); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebSearchEnabled == nil || *cfg.WebSearchEnabled {
		t.Fatal("web_search_enabled was not persisted as disabled")
	}
	if cfg.Theme != "dark" {
		t.Fatalf("unrelated config changed: theme = %q, want dark", cfg.Theme)
	}
}
