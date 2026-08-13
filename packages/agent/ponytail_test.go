package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigPonytailModeDefaultsToEnabled(t *testing.T) {
	cfg := Config{}
	if !cfg.PonytailModeEnabled() {
		t.Fatal("Ponytail mode should default to enabled")
	}

	disabled := false
	cfg.PonytailEnabled = &disabled
	if cfg.PonytailModeEnabled() {
		t.Fatal("Ponytail mode should honor an explicit false value")
	}

	enabled := true
	cfg.PonytailEnabled = &enabled
	if !cfg.PonytailModeEnabled() {
		t.Fatal("Ponytail mode should honor an explicit true value")
	}
}

func TestPonytailSystemAddendumIsCompact(t *testing.T) {
	addendum := PonytailSystemAddendum()
	if addendum == "" {
		t.Fatal("Ponytail system addendum is empty")
	}
	if len(addendum) > 2000 {
		t.Fatalf("Ponytail system addendum is %d bytes, want at most 2000", len(addendum))
	}
	for _, want := range []string{"YAGNI", "standard library", "validation", "ordinary conversation"} {
		if !strings.Contains(addendum, want) {
			t.Fatalf("Ponytail system addendum missing %q:\n%s", want, addendum)
		}
	}
}

func TestResolvePonytailModeDefaultAndExplicitDisable(t *testing.T) {
	cases := []struct {
		name     string
		disabled bool
		want     bool
	}{
		{name: "default enabled", want: true},
		{name: "explicitly disabled", disabled: true, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("ZUT_HOME", filepath.Join(root, "zut-home"))
			t.Setenv("HOME", filepath.Join(root, "home"))
			if tc.disabled {
				disabled := false
				if err := SaveConfig(Config{PonytailEnabled: &disabled}); err != nil {
					t.Fatal(err)
				}
			}

			resolved, err := Resolve(Args{
				Provider: "ollama",
				Model:    "any-local-model",
				CWD:      filepath.Join(root, "project"),
				NoLSP:    true,
				NoSkill:  true,
				NoTools:  true,
			}, false)
			if err != nil {
				t.Fatalf("Resolve failed: %v", err)
			}
			got := strings.Contains(resolved.SystemPrompt, PonytailSystemAddendum())
			if got != tc.want {
				t.Fatalf("Ponytail prompt inclusion = %v, want %v:\n%s", got, tc.want, resolved.SystemPrompt)
			}
			if count := strings.Count(resolved.SystemPrompt, PonytailSystemAddendum()); count != btoi(tc.want) {
				t.Fatalf("Ponytail prompt occurrence count = %d, want %d", count, btoi(tc.want))
			}
		})
	}
}

func btoi(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestResolvePonytailModeRetainsAddendumWithSystemFile(t *testing.T) {
	root := t.TempDir()
	zutHome := filepath.Join(root, "zut-home")
	t.Setenv("ZUT_HOME", zutHome)
	t.Setenv("HOME", filepath.Join(root, "home"))
	if err := os.MkdirAll(zutHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(zutHome, "SYSTEM.md"), []byte("custom system identity"), 0o600); err != nil {
		t.Fatal(err)
	}

	resolved, err := Resolve(Args{
		Provider: "ollama",
		Model:    "any-local-model",
		CWD:      filepath.Join(root, "project"),
		NoLSP:    true,
		NoSkill:  true,
		NoTools:  true,
	}, false)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if !strings.Contains(resolved.SystemPrompt, "custom system identity") {
		t.Fatalf("SYSTEM.md content missing:\n%s", resolved.SystemPrompt)
	}
	if !strings.Contains(resolved.SystemPrompt, PonytailSystemAddendum()) {
		t.Fatalf("SYSTEM.md replacement lost Ponytail addendum:\n%s", resolved.SystemPrompt)
	}
}

func TestResolvePonytailModePrecedesExplicitAppendPrompt(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ZUT_HOME", filepath.Join(root, "zut-home"))
	t.Setenv("HOME", filepath.Join(root, "home"))

	const explicit = "explicit invocation guidance"
	resolved, err := Resolve(Args{
		Provider:           "ollama",
		Model:              "any-local-model",
		CWD:                filepath.Join(root, "project"),
		NoLSP:              true,
		NoSkill:            true,
		NoTools:            true,
		AppendSystemPrompt: []string{explicit},
	}, false)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	ponytailIndex := strings.Index(resolved.SystemPrompt, PonytailSystemAddendum())
	explicitIndex := strings.Index(resolved.SystemPrompt, explicit)
	if ponytailIndex < 0 || explicitIndex < 0 || ponytailIndex > explicitIndex {
		t.Fatalf("prompt ordering = Ponytail %d, explicit %d:\n%s", ponytailIndex, explicitIndex, resolved.SystemPrompt)
	}
}

func TestResolvePonytailModeAppliesAcrossHeadlessModesAndCustomIdentity(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ZUT_HOME", filepath.Join(root, "zut-home"))
	t.Setenv("HOME", filepath.Join(root, "home"))

	modes := []Mode{"", ModePrint, ModeStream, ModeJSON, ModeRPC}
	for _, mode := range modes {
		resolved, err := Resolve(Args{
			Mode:         mode,
			Provider:     "ollama",
			Model:        "any-local-model",
			CWD:          filepath.Join(root, "project"),
			NoLSP:        true,
			NoSkill:      true,
			NoTools:      true,
			SystemPrompt: "custom identity",
		}, false)
		if err != nil {
			t.Fatalf("Resolve(%q) failed: %v", mode, err)
		}
		if !strings.Contains(resolved.SystemPrompt, "custom identity") {
			t.Fatalf("Resolve(%q) lost custom identity:\n%s", mode, resolved.SystemPrompt)
		}
		if !strings.Contains(resolved.SystemPrompt, PonytailSystemAddendum()) {
			t.Fatalf("Resolve(%q) omitted Ponytail guidance:\n%s", mode, resolved.SystemPrompt)
		}
	}
}

func TestConfigSettingsStorePersistsPonytailWithoutChangingKnownFields(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	if err := SaveConfig(Config{Theme: "dark", Provider: "ollama", Model: "local"}); err != nil {
		t.Fatal(err)
	}

	if err := (configSettingsStore{}).SetPonytailEnabled(false); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PonytailEnabled == nil || *cfg.PonytailEnabled {
		t.Fatal("Ponytail mode was not persisted as disabled")
	}
	if cfg.Theme != "dark" || cfg.Provider != "ollama" || cfg.Model != "local" {
		t.Fatalf("known config fields changed: %#v", cfg)
	}
}
