package agent

import (
	"os"
	"testing"

	"github.com/bnema/zut/packages/agent/config"
	"github.com/bnema/zut/packages/agent/modes"
)

func TestActiveAliasedModelProfileResolvesCanonicalProvider(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	cfg := config.Config{
		ActiveModelProfile: 2,
		QuickModelShortcuts: []config.QuickModelShortcut{
			{},
			{Provider: "bedrock", Model: "amazon.nova-lite-v1:0"},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	r, err := Resolve(Args{APIKey: "synthetic-test-key"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Provider != "amazon-bedrock" {
		t.Fatalf("resolved provider = %q", r.Provider)
	}
	if r.Model != cfg.QuickModelShortcuts[1].Model || r.Provider != "amazon-bedrock" {
		t.Fatalf("active aliased profile not resolved: %s/%s", r.Provider, r.Model)
	}
}

func TestLegacyQuickModelShortcutPreservesProfileState(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	if err := config.Save(config.Config{
		QuickModelShortcuts: []config.QuickModelShortcut{{
			Provider: "openai", Model: "gpt-5.6-sol", Reasoning: "high", FastMode: true,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := (configSettingsStore{}).SetQuickModelShortcut(1, "openai", "gpt-5.5"); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	profile := cfg.QuickModelShortcuts[0]
	if profile.Model != "gpt-5.5" || profile.Reasoning != "high" || !profile.FastMode {
		t.Fatalf("legacy shortcut update lost profile state: %+v", profile)
	}
}

func TestModelProfileConfigRoundTrip(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	if err := config.Save(config.Config{Theme: "dark"}); err != nil {
		t.Fatal(err)
	}
	store := configSettingsStore{}
	p := modes.QuickModelShortcut{Provider: "openai", Model: "gpt-5.6-sol", Reasoning: "max", FastMode: true}
	if err := store.SetModelProfile(9, p, 9); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ActiveModelProfile != 9 || len(cfg.QuickModelShortcuts) != 9 || cfg.Reasoning != "max" || cfg.Model != p.Model || cfg.FastMode == nil || !*cfg.FastMode || cfg.QuickModelShortcuts[8].FastMode != p.FastMode || cfg.Theme != "dark" {
		t.Fatalf("config=%+v", cfg)
	}
	// The selected profile is authoritative even after hand-editing its model
	// and reasoning, without requiring duplicate changes to the legacy defaults.
	cfg.QuickModelShortcuts[8].Reasoning = "low"
	applyActiveModelProfile(&cfg)
	if cfg.Reasoning != "low" {
		t.Fatal("active profile did not supply defaults")
	}
	if err := store.SetModelProfile(9, modes.QuickModelShortcut{}, 0); err != nil {
		t.Fatal(err)
	}
	cleared, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cleared.ActiveModelProfile != 0 || len(cleared.QuickModelShortcuts) != 0 || cleared.Model != p.Model {
		t.Fatalf("clear=%+v", cleared)
	}
}

func TestModelProfileConfigLegacyAndInvalidActiveSlots(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	if err := os.WriteFile(config.Path(), []byte(`{"provider":"openai","model":"gpt-5.5","reasoning":"high","quick_model_shortcuts":[{"provider":"openai","model":"gpt-5.6-sol"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, slot := range []int{-1, 0, 2, 10} {
		copy := cfg
		copy.ActiveModelProfile = slot
		applyActiveModelProfile(&copy)
		want := "gpt-5.5" // invalid or unassigned active slots preserve plain settings
		if copy.Model != want {
			t.Fatalf("active slot %d: model %s, want %s", slot, copy.Model, want)
		}
	}
	cfg.ActiveModelProfile = 1
	applyActiveModelProfile(&cfg)
	if cfg.Model != "gpt-5.6-sol" || cfg.Reasoning != "" {
		t.Fatal("legacy slots should recall with reasoning off")
	}
}

func TestModelProfileStoreRejectsInvalidSlotsAndCorruptConfig(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	store := configSettingsStore{}
	for _, slot := range []int{-1, 0, 10} {
		if err := store.SetModelProfile(slot, modes.QuickModelShortcut{}, 0); err == nil {
			t.Fatalf("accepted slot %d", slot)
		}
	}
	original := []byte(`{"invalid":`)
	if err := os.WriteFile(config.Path(), original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.SetModelProfile(1, modes.QuickModelShortcut{Provider: "openai", Model: "gpt-5.5"}, 1); err == nil {
		t.Fatal("corrupt config was accepted")
	}
	got, err := os.ReadFile(config.Path())
	if err != nil || string(got) != string(original) {
		t.Fatal("corrupt config was overwritten")
	}
}

func TestModelProfileResolveDefaultsAndExplicitOverrides(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	if err := config.Save(config.Config{Provider: "openai", Model: "gpt-5.5", Reasoning: "high", ActiveModelProfile: 1, QuickModelShortcuts: []config.QuickModelShortcut{{Provider: "openai", Model: "gpt-5.6-sol", Reasoning: "max"}}}); err != nil {
		t.Fatal(err)
	}
	for _, override := range []bool{false, true} {
		args := Args{APIKey: "synthetic-test-key"}
		wantModel, wantReasoning := "gpt-5.6-sol", "max"
		if override {
			args.Model, args.Reasoning = "gpt-5.5", "off"
			wantModel, wantReasoning = "gpt-5.5", ""
		}
		r, err := Resolve(args, false)
		if err != nil {
			t.Fatal(err)
		}
		if r.Model != wantModel || r.Reasoning != wantReasoning {
			t.Fatalf("override=%v model=%s reasoning=%s", override, r.Model, r.Reasoning)
		}
	}
}
