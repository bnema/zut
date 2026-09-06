package modes

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

type profileSettingsStore struct {
	SettingsStore
	profiles map[int]QuickModelShortcut
	active   int
	err      error
}

func (s *profileSettingsStore) SetModelProfile(slot int, p QuickModelShortcut, active int) error {
	if s.err != nil {
		return s.err
	}
	s.profiles[slot], s.active = p, active
	return nil
}

func newProfileInteractive(t *testing.T) (*Interactive, *profileSettingsStore) {
	t.Helper()
	s := &profileSettingsStore{profiles: make(map[int]QuickModelShortcut)}
	i := NewInteractive(InteractiveConfig{Theme: tui.Dark, Provider: "openai", Model: "gpt-5.6-sol", Reasoning: "high", SettingsStore: s})
	i.rend = nil
	i.agent = core.NewAgent(nil, i.cfg.Model, "", nil)
	i.agent.Reasoning = i.cfg.Reasoning
	return i, s
}

func TestModelProfileCaptureRecallAndAutosave(t *testing.T) {
	i, s := newProfileInteractive(t)
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: '&', Ctrl: true})
	if i.cfg.ActiveModelProfile != 1 || s.active != 1 || s.profiles[1].Reasoning != "high" {
		t.Fatalf("capture: cfg=%+v store=%+v", i.cfg.QuickModelShortcuts, s)
	}
	if !strings.Contains(i.statusOK, "created and active") {
		t.Fatal(i.statusOK)
	}
	i.applyReasoningSetting("max")
	if s.profiles[1].Reasoning != "max" {
		t.Fatal("reasoning not saved to active slot")
	}
	i.applyQuickModelShortcut(2)
	i.applyReasoningSetting("")
	i.applyModelSelection("openai", "gpt-5.5")
	if s.profiles[2].Model != "gpt-5.5" || s.profiles[2].Reasoning != "" {
		t.Fatal(s.profiles)
	}
	i.runSlash(context.Background(), "/profile 1")
	if i.cfg.Model != "gpt-5.6-sol" || i.agent.Model != i.cfg.Model || i.cfg.Reasoning != "max" || i.agent.Reasoning != "max" || s.active != 1 {
		t.Fatalf("recall failed: model=%s reasoning=%s active=%d", i.cfg.Model, i.cfg.Reasoning, s.active)
	}
	if s.profiles[2].Model != "gpt-5.5" {
		t.Fatal("recall overwrote previous slot")
	}
}

func TestModelProfileCrossProviderCarriesTranscriptAndUsage(t *testing.T) {
	i, s := newProfileInteractive(t)
	msgs := []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "preserve me"}}}}
	i.agent.SetMessages(msgs)
	i.agent.SeedCost(provider.Usage{InputTokens: 123})
	i.cfg.QuickModelShortcuts = []QuickModelShortcut{{Provider: "google", Model: "gemini-2.0-flash", Reasoning: "high"}}
	var changed string
	i.cfg.OnReasoningChanged = func(level string) { changed = level }
	i.cfg.BuildAgentFor = func(p, m string) (*core.Agent, string, string, error) {
		return core.NewAgent(nil, m, "", nil), p, m, nil
	}
	transitions := 0
	i.cfg.SessionTransition = func(fn func()) { transitions++; fn() }
	i.applyQuickModelShortcut(1)
	if transitions != 1 || i.cfg.Provider != "google" || i.cfg.Reasoning != "" || changed != "" || s.profiles[1].Reasoning != "" {
		t.Fatalf("cross-provider state: provider=%s reasoning=%s store=%+v", i.cfg.Provider, i.cfg.Reasoning, s)
	}
	if !reflect.DeepEqual(i.agent.Messages(), msgs) || i.agent.Cost().InputTokens != 123 {
		t.Fatal("lost transcript or usage")
	}
}

func TestModelProfileFailureDoesNotSelectOrOverwrite(t *testing.T) {
	for _, failure := range []string{"busy", "unknown", "build", "save"} {
		t.Run(failure, func(t *testing.T) {
			i, s := newProfileInteractive(t)
			i.applyQuickModelShortcut(1)
			before := s.profiles[1]
			i.cfg.QuickModelShortcuts = append(i.cfg.QuickModelShortcuts, QuickModelShortcut{Provider: "google", Model: "gemini-2.0-flash"})
			switch failure {
			case "busy":
				i.busy = true
			case "unknown":
				i.cfg.QuickModelShortcuts[1].Model = "does-not-exist"
			case "build":
				i.cfg.BuildAgentFor = func(string, string) (*core.Agent, string, string, error) {
					return nil, "", "", errors.New("build failed")
				}
			case "save":
				i.cfg.QuickModelShortcuts[1] = QuickModelShortcut{Provider: i.cfg.Provider, Model: i.cfg.Model, Reasoning: "low"}
				s.err = errors.New("disk full")
			}
			i.applyQuickModelShortcut(2)
			if s.active != 1 || s.profiles[1] != before || i.statusErr == "" {
				t.Fatalf("failed switch changed persistence: %+v / %s", s, i.statusErr)
			}
			if failure != "save" && (i.cfg.ActiveModelProfile != 1 || i.cfg.Reasoning != "high") {
				t.Fatal("failed switch changed live profile")
			}
			if failure == "save" && (i.cfg.ActiveModelProfile != 0 || !strings.Contains(i.statusErr, "this run")) {
				t.Fatal("save failure misrepresents persistence")
			}
		})
	}
}

func TestModelProfileAssignmentAndReasoningSaveFailure(t *testing.T) {
	i, s := newProfileInteractive(t)
	i.applyQuickModelShortcut(1)
	before := i.cfg.QuickModelShortcuts[0]
	s.err = errors.New("read-only config")
	i.setQuickModelProfile(1, QuickModelShortcut{})
	i.applyReasoningSetting("low")
	if i.cfg.QuickModelShortcuts[0] != before || i.cfg.ActiveModelProfile != 1 || i.agent.Reasoning != "high" || i.cfg.Reasoning != "high" {
		t.Fatal("save failure changed selection")
	}
	s.err = nil
	i.setQuickModelProfile(1, QuickModelShortcut{})
	if i.cfg.ActiveModelProfile != 0 || s.active != 0 || s.profiles[1].Model != "" || i.agent.Model != before.Model {
		t.Fatal("clear should detach without changing live model")
	}
}

func TestModelProfilePickerReasoningIsLocalUntilAssigned(t *testing.T) {
	i, s := newProfileInteractive(t)
	i.applyQuickModelShortcut(1)
	i.openQuickModelPicker(2)
	i.modelDialog.view = []provider.Model{{Provider: "openai", ID: "gpt-5.6-sol", Reasoning: true}}
	i.modelDialog.cursor = 0
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyLeft})
	if i.cfg.Reasoning != "high" || s.profiles[1].Reasoning != "high" {
		t.Fatal("picker changed active reasoning")
	}
	want := i.modelDialog.reasoning
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
	if s.profiles[2].Reasoning != want || i.cfg.ActiveModelProfile != 1 || i.cfg.Reasoning != "high" {
		t.Fatalf("assignment=%+v reasoning=%s", s, want)
	}
}

func TestModelProfileRestoresActiveIndicatorOnlyForMatchingSelection(t *testing.T) {
	p := QuickModelShortcut{Provider: "openai", Model: "gpt-5.6-sol", Reasoning: "high"}
	for _, override := range []bool{false, true} {
		cfg := InteractiveConfig{Theme: tui.Dark, Provider: p.Provider, Model: p.Model, Reasoning: p.Reasoning, QuickModelShortcuts: []QuickModelShortcut{p}, ActiveModelProfile: 1}
		want := 1
		if override {
			cfg.Model = "gpt-5.5"
			want = 0
		}
		i := NewInteractive(cfg)
		if i.cfg.ActiveModelProfile != want {
			t.Fatalf("override=%v active=%d", override, i.cfg.ActiveModelProfile)
		}
	}
}

func TestModelProfileShortcutLayouts(t *testing.T) {
	for _, row := range []string{"123456789", "&é\"'(-è_ç"} {
		for idx, r := range []rune(row) {
			if got := quickModelShortcutSlot(tui.Key{Kind: tui.KeyRune, Rune: r, Ctrl: true}); got != idx+1 {
				t.Fatalf("Ctrl+%c = %d", r, got)
			}
			for _, key := range []tui.Key{{Kind: tui.KeyRune, Rune: r}, {Kind: tui.KeyRune, Rune: r, Ctrl: true, Alt: true}, {Kind: tui.KeyPaste, Paste: string(r)}} {
				if quickModelShortcutSlot(key) != 0 {
					t.Fatalf("unexpected shortcut: %+v", key)
				}
			}
		}
	}
}

func TestModelProfileCommandRejectsInvalidSlots(t *testing.T) {
	for _, command := range []string{"/profile", "/profile 0", "/profile 10", "/profile x", "/profile 1 extra"} {
		i, s := newProfileInteractive(t)
		i.runSlash(context.Background(), command)
		if !strings.Contains(i.statusErr, "usage:") || s.active != 0 {
			t.Fatalf("%s: %s", command, i.statusErr)
		}
	}
}
