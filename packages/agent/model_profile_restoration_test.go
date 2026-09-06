package agent

import (
	"testing"

	"github.com/bnema/zut/packages/agent/modes"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/tui"
)

func TestResolveRepairsActiveModelProfileFallback(t *testing.T) {
	for _, tc := range []struct {
		name    string
		active  int
		profile QuickModelShortcut
	}{
		{"missing model", 2, QuickModelShortcut{Provider: "openai", Model: "removed-model", Reasoning: "high"}},
		{"missing provider default slot", 0, QuickModelShortcut{Provider: "removed-provider", Model: "removed-model", Reasoning: "high"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ZUT_HOME", t.TempDir())
			slot := tc.active
			if slot == 0 {
				slot = 1
			}
			profiles := make([]QuickModelShortcut, slot)
			profiles[slot-1] = tc.profile
			if err := SaveConfig(Config{ActiveModelProfile: tc.active, QuickModelShortcuts: profiles}); err != nil {
				t.Fatal(err)
			}
			args := Args{APIKey: "synthetic-test-key"}
			resolved, err := Resolve(args, false)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := LoadConfig()
			if err != nil {
				t.Fatal(err)
			}
			p := saved.QuickModelShortcuts[slot-1]
			if p.Provider != resolved.Provider || p.Model != resolved.Model || p.Reasoning != resolved.Reasoning {
				t.Fatalf("fallback left stale profile: %+v; resolved %s/%s (%s)", p, resolved.Provider, resolved.Model, resolved.Reasoning)
			}
			again, err := Resolve(args, false)
			if err != nil {
				t.Fatal(err)
			}
			if again.Provider != resolved.Provider || again.Model != resolved.Model || again.Reasoning != resolved.Reasoning {
				t.Fatal("repaired selection changed on next resolution")
			}
		})
	}
}

func TestResolveExplicitFallbackDoesNotRepairFavorite(t *testing.T) {
	for _, args := range []Args{{Model: "removed-model"}, {Provider: "removed-provider"}} {
		t.Setenv("ZUT_HOME", t.TempDir())
		original := QuickModelShortcut{Provider: "openai", Model: "gpt-5.6-sol", Reasoning: "high"}
		if err := SaveConfig(Config{ActiveModelProfile: 1, QuickModelShortcuts: []QuickModelShortcut{original}}); err != nil {
			t.Fatal(err)
		}
		args.APIKey = "synthetic-test-key"
		if _, err := Resolve(args, false); err != nil {
			t.Fatal(err)
		}
		saved, err := LoadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if saved.QuickModelShortcuts[0] != original {
			t.Fatalf("explicit fallback overwrote favorite: %+v", saved.QuickModelShortcuts[0])
		}
	}
}

func TestModelProfileStartupClampsReasoningAndPreservesAutosave(t *testing.T) {
	for _, tc := range []struct {
		profile QuickModelShortcut
		want    string
	}{
		{QuickModelShortcut{Provider: "openai", Model: "gpt-5.6-sol", Reasoning: "minimum"}, "low"},
		{QuickModelShortcut{Provider: "google", Model: "gemini-2.0-flash", Reasoning: "high"}, ""},
	} {
		t.Run(tc.profile.Model, func(t *testing.T) {
			t.Setenv("ZUT_HOME", t.TempDir())
			cfg := Config{ActiveModelProfile: 1, QuickModelShortcuts: []QuickModelShortcut{tc.profile}}
			if err := SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			r, err := Resolve(Args{APIKey: "synthetic-test-key"}, false)
			if err != nil {
				t.Fatal(err)
			}
			if r.Reasoning != tc.want {
				t.Fatalf("startup reasoning=%q, want %q", r.Reasoning, tc.want)
			}
			i := modes.NewInteractive(modes.InteractiveConfig{
				Theme: tui.Dark, Provider: r.Provider, Model: r.Model, Reasoning: r.Reasoning,
				Agent: core.NewAgent(nil, r.Model, "", nil), SettingsStore: configSettingsStore{},
				QuickModelShortcuts: interactiveQuickModelShortcuts(cfg.QuickModelShortcuts), ActiveModelProfile: 1,
			})
			i.SubmitSlash("/model " + r.Model)
			saved, err := LoadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if saved.QuickModelShortcuts[0].Reasoning != tc.want {
				t.Fatalf("clamped active profile failed to autosave: %+v", saved.QuickModelShortcuts[0])
			}
		})
	}
}
