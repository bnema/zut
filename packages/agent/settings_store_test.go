package agent

import (
	"strings"
	"testing"
)

func TestSubagentsSystemAddendaListOnlyEnabledLifecycleTools(t *testing.T) {
	builders := []struct {
		name            string
		build           func(bool, bool, bool) string
		noSpawnFallback string
	}{
		{name: "proactive", build: ProactiveSubagentsSystemAddendumFor, noSpawnFallback: "Continue non-delegated work locally."},
		{name: "strict", build: StrictOrchestratorSystemAddendumFor, noSpawnFallback: "Do not implement, debug, test, or review directly."},
	}
	for _, builder := range builders {
		t.Run(builder.name, func(t *testing.T) {
			for _, tc := range []struct {
				name      string
				spawn     bool
				stop      bool
				resume    bool
				want      []string
				unwanted  []string
				available bool
			}{
				{name: "none", spawn: true, unwanted: []string{"subagent_stop", "subagent_resume"}, available: true},
				{name: "fully unavailable", unwanted: []string{"subagent_stop", "subagent_resume"}},
				{name: "stop", spawn: true, stop: true, want: []string{"subagent_stop"}, unwanted: []string{"subagent_resume"}, available: true},
				{name: "resume", spawn: true, resume: true, want: []string{"subagent_resume"}, unwanted: []string{"subagent_stop"}, available: true},
				{name: "both", spawn: true, stop: true, resume: true, want: []string{"subagent_stop", "subagent_resume"}, available: true},
				{name: "stop without spawn", stop: true, want: []string{"subagent_stop", "Spawning new workers is unavailable"}, unwanted: []string{"subagent_resume"}},
				{name: "resume without spawn", resume: true, want: []string{"subagent_resume", "Spawning new workers is unavailable"}, unwanted: []string{"subagent_stop"}},
				{name: "both without spawn", stop: true, resume: true, want: []string{"subagent_stop", "subagent_resume", "Spawning new workers is unavailable"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					got := builder.build(tc.spawn, tc.stop, tc.resume)
					if !tc.spawn && (tc.stop || tc.resume) && !strings.Contains(got, builder.noSpawnFallback) {
						t.Fatalf("addendum missing mode-specific no-spawn fallback %q:\n%s", builder.noSpawnFallback, got)
					}
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
					if gotAvailable := !strings.Contains(got, "unavailable in this session") && !strings.Contains(got, "Spawning new workers is unavailable"); gotAvailable != tc.available {
						t.Fatalf("delegation available = %t, want %t:\n%s", gotAvailable, tc.available, got)
					}
				})
			}
		})
	}
}

func TestSubagentPoliciesSeparateProactiveAndStrictOwnership(t *testing.T) {
	proactive := ProactiveSubagentsSystemAddendumFor(true, false, false)
	for _, want := range []string{"primary owner and implementer", "immediate next task you will perform locally", "If you cannot name useful non-overlapping local work, do not delegate", "Do not search, read, test, review, edit"} {
		if !strings.Contains(proactive, want) {
			t.Fatalf("proactive addendum missing %q:\n%s", want, proactive)
		}
	}
	if strings.Contains(proactive, "not an implementer") {
		t.Fatalf("proactive addendum retained strict orchestrator contract:\n%s", proactive)
	}

	strict := StrictOrchestratorSystemAddendumFor(true, false, false)
	for _, want := range []string{"not an implementer", "non-overlapping worker scopes", "Once a worker is active", "Only coordinate workers"} {
		if !strings.Contains(strict, want) {
			t.Fatalf("strict addendum missing %q:\n%s", want, strict)
		}
	}
}

func TestSubagentPoliciesHandleUnavailableDelegationByMode(t *testing.T) {
	proactive := ProactiveSubagentsSystemAddendumFor(false, false, false)
	if !strings.Contains(proactive, "Continue the user's task locally") || strings.Contains(proactive, "report this limitation") {
		t.Fatalf("proactive unavailable guidance blocks local work:\n%s", proactive)
	}
	strict := StrictOrchestratorSystemAddendumFor(false, false, false)
	if !strings.Contains(strict, "report this limitation") || !strings.Contains(strict, "rather than implementing") {
		t.Fatalf("strict unavailable guidance permits local implementation:\n%s", strict)
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

func TestConfigSettingsStoreResetsRemovedInheritedThemeToAuto(t *testing.T) {
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
	if cfg.Theme != "" {
		t.Fatalf("theme = %q, want auto", cfg.Theme)
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
