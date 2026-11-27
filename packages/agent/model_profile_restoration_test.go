package agent

import (
	"fmt"
	"testing"

	"github.com/bnema/zut/packages/agent/config"
	"github.com/stretchr/testify/require"
)

func TestResolveRepairsActiveModelProfileFallback(t *testing.T) {
	for _, tc := range []struct {
		name    string
		active  int
		profile config.QuickModelShortcut
	}{
		{"missing model", 2, config.QuickModelShortcut{Provider: "openai", Model: "removed-model", Reasoning: "high"}},
		{"missing provider active slot", 1, config.QuickModelShortcut{Provider: "removed-provider", Model: "removed-model", Reasoning: "high"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ZUT_HOME", t.TempDir())
			slot := tc.active
			if slot == 0 {
				slot = 1
			}
			profiles := make([]config.QuickModelShortcut, slot)
			profiles[slot-1] = tc.profile
			if err := config.Save(config.Config{ActiveModelProfile: tc.active, QuickModelShortcuts: profiles}); err != nil {
				t.Fatal(err)
			}
			args := Args{APIKey: "synthetic-test-key"}
			resolved, err := Resolve(args, false)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := config.Load()
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
		original := config.QuickModelShortcut{Provider: "openai", Model: "gpt-5.6-sol", Reasoning: "high"}
		if err := config.Save(config.Config{ActiveModelProfile: 1, QuickModelShortcuts: []config.QuickModelShortcut{original}}); err != nil {
			t.Fatal(err)
		}
		args.APIKey = "synthetic-test-key"
		if _, err := Resolve(args, false); err != nil {
			t.Fatal(err)
		}
		saved, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		if saved.QuickModelShortcuts[0] != original {
			t.Fatalf("explicit fallback overwrote favorite: %+v", saved.QuickModelShortcuts[0])
		}
	}
}

func TestModelProfileStartupClampsReasoning(t *testing.T) {
	for _, tc := range []struct {
		profile config.QuickModelShortcut
		want    string
	}{
		{config.QuickModelShortcut{Provider: "openai", Model: "gpt-5.6-sol", Reasoning: "minimum"}, "low"},
		{config.QuickModelShortcut{Provider: "google", Model: "gemini-2.0-flash", Reasoning: "high"}, ""},
	} {
		t.Run(tc.profile.Model, func(t *testing.T) {
			t.Setenv("ZUT_HOME", t.TempDir())
			cfg := config.Config{ActiveModelProfile: 1, QuickModelShortcuts: []config.QuickModelShortcut{tc.profile}}
			if err := config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			r, err := Resolve(Args{APIKey: "synthetic-test-key"}, false)
			if err != nil {
				t.Fatal(err)
			}
			if r.Reasoning != tc.want {
				t.Fatalf("startup reasoning=%q, want %q", r.Reasoning, tc.want)
			}
		})
	}
}

func TestResolveWithoutActiveProfilePreservesPlainSettings(t *testing.T) {
	for _, active := range []int{0, -1, 10} {
		for _, model := range []string{"gpt-5", "removed-model"} {
			t.Run(fmt.Sprintf("active=%d/model=%s", active, model), func(t *testing.T) {
				t.Setenv("ZUT_HOME", t.TempDir())
				favorite := config.QuickModelShortcut{Provider: "removed-provider", Model: "removed-favorite", Reasoning: "max", FastMode: true}
				fast := false
				cfg := config.Config{Provider: "openai", Model: model, Reasoning: "low", FastMode: &fast, ActiveModelProfile: active, QuickModelShortcuts: []config.QuickModelShortcut{favorite}}
				require.Equal(t, 0, applyActiveModelProfile(&cfg))
				require.Equal(t, model, cfg.Model)
				require.NoError(t, config.Save(cfg))
				resolved, err := Resolve(Args{APIKey: "synthetic-test-key"}, false)
				require.NoError(t, err)
				require.Equal(t, "openai", resolved.Provider)
				require.Equal(t, "low", resolved.Reasoning)
				if model == "gpt-5" {
					require.Equal(t, model, resolved.Model)
				}
				saved, err := config.Load()
				require.NoError(t, err)
				require.Equal(t, active, saved.ActiveModelProfile)
				require.Equal(t, []config.QuickModelShortcut{favorite}, saved.QuickModelShortcuts)
				require.Equal(t, "low", saved.Reasoning)
				require.Equal(t, &fast, saved.FastMode)
				require.Equal(t, resolved.Model, saved.Model)
			})
		}
	}
}
