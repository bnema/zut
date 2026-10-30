package modes

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
	"github.com/stretchr/testify/mock"
)

func TestReasoningSelectorUsesCurrentModelLevels(t *testing.T) {
	i := &Interactive{cfg: InteractiveConfig{
		Provider:  "openai",
		Model:     "gpt-5.6-sol",
		Reasoning: "minimum",
	}}

	item := i.reasoningSettingItem()
	var levels []string
	for _, option := range item.options {
		levels = append(levels, option.value)
	}
	want := []string{"", "low", "medium", "high", "xhigh", "max"}
	if !slices.Equal(levels, want) {
		t.Fatalf("reasoning levels = %q, want %q", levels, want)
	}
	if item.options[item.choice].value != "low" {
		t.Fatalf("clamped current level = %q, want low", item.options[item.choice].value)
	}
}

func TestReasoningSelectorOnlyOffersOffForUnsupportedModel(t *testing.T) {
	i := &Interactive{cfg: InteractiveConfig{
		Provider:  "google",
		Model:     "gemini-2.0-flash",
		Reasoning: "high",
	}}

	item := i.reasoningSettingItem()
	if len(item.options) != 1 || item.options[0].value != "" {
		t.Fatalf("reasoning levels = %#v, want off only", item.options)
	}
	if item.hint != "current model does not support reasoning" {
		t.Fatalf("hint = %q", item.hint)
	}
}

// Ctrl+R asks the daemon for the next level; the level shown is the one the
// daemon reports back (session TestSetReasoningPersistsAndUpdatesAgent).
func TestCtrlRCyclesCurrentModelReasoningLevels(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	info := session.Info{Run: session.RunStateIdle, Provider: "openai", Model: "gpt-5.6-sol", Reasoning: "high"}
	i := newQueuePortInteractive(t, port, session.Snapshot{Info: info}, InteractiveConfig{Theme: tui.Dark})
	i.rend = nil

	for _, want := range []string{"xhigh", "max", ""} {
		called := make(chan struct{})
		port.EXPECT().SetReasoning(mock.Anything, want).RunAndReturn(func(context.Context, string) error {
			close(called)
			return nil
		}).Once()
		if done := i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlR}); done {
			t.Fatal("ctrl+r exited the interactive session")
		}
		waitSignal(t, called, "SetReasoning "+want)
		info.Reasoning = want
		i.mu.Lock()
		i.applyInfoLocked(info)
		got := i.cfg.Reasoning
		i.mu.Unlock()
		if got != want {
			t.Fatalf("reasoning = %q, want %q", got, want)
		}
	}
}

func TestCtrlRReportsUnsupportedCurrentModel(t *testing.T) {
	i := NewInteractive(InteractiveConfig{
		Theme:     tui.Dark,
		Provider:  "google",
		Model:     "gemini-2.0-flash",
		Reasoning: "high",
	})
	i.rend = nil

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlR})

	if i.cfg.Reasoning != "high" {
		t.Fatalf("reasoning changed to %q for unsupported model", i.cfg.Reasoning)
	}
	if i.statusOK != "current model does not support reasoning" {
		t.Fatalf("reasoning status = %q", i.statusOK)
	}
}

func TestReasoningCommandOpensDirectSelector(t *testing.T) {
	i := &Interactive{
		cfg: InteractiveConfig{
			Reasoning: "high",
		},
		settingsDialog: newSettingsDialog(),
	}

	i.runSlash(context.Background(), "/reasoning")

	if !i.settingsDialog.Active() || !i.settingsDialog.selecting || !i.settingsDialog.direct {
		t.Fatal("reasoning command did not open the direct option selector")
	}
	lines := i.settingsDialog.Render(tui.Dark, 100)
	text := strings.Join(lines, "\n")
	if !strings.Contains(text, "reasoning level") || !strings.Contains(text, "high") {
		t.Fatalf("reasoning selector missing current level: %q", text)
	}
	i.settingsDialog.HandleKey(tui.Key{Kind: tui.KeyDown})
	act := i.settingsDialog.HandleKey(tui.Key{Kind: tui.KeyEnter})
	i.applySettingChange(act)
	if i.cfg.Reasoning != "xhigh" || i.settingsDialog.Active() {
		t.Fatalf("reasoning selection was not applied and closed: level=%q active=%v", i.cfg.Reasoning, i.settingsDialog.Active())
	}
	if i.statusOK != "reasoning level xhigh" {
		t.Fatalf("reasoning status = %q", i.statusOK)
	}

	i.runSlash(context.Background(), "/reasoning")
	act = i.settingsDialog.HandleKey(tui.Key{Kind: tui.KeyEsc})
	if !act.Close || i.settingsDialog.Active() {
		t.Fatal("escape did not close the direct reasoning selector")
	}
}

func TestModelDialogAdvertisesReasoningNavigation(t *testing.T) {
	d := newModelDialog()
	d.Open("", nil, "high")
	d.view = []provider.Model{{Provider: "openai-codex", ID: "gpt-5.6-luna", Reasoning: true}}

	text := strings.Join(d.Render(tui.Dark, 100), "\n")
	plain := stripANSIBytes(text)
	if !strings.Contains(plain, "reasoning: high") || !strings.Contains(plain, "←/→ or H/L to change") {
		t.Fatalf("model dialog missing reasoning guidance: %q", plain)
	}
}

func TestModelPickerSkipsLlamaRefreshWhenRouterIsNotConfigured(t *testing.T) {
	i := &Interactive{
		cfg: InteractiveConfig{
			RefreshLlamaCPPModels: func(context.Context) error { return nil },
		},
		modelRefresh: make(chan modelRefreshResult, 1),
		modelDialog:  newModelDialog(),
	}

	i.runSlash(context.Background(), "/model")

	if !i.modelDialog.Active() {
		t.Fatal("model picker did not open immediately")
	}
	if i.statusOK == "Refreshing models" {
		t.Fatal("refresh status shown without llama.cpp router configuration")
	}
}
