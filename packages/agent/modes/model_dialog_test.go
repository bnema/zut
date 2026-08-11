package modes

import (
	"context"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

func TestModelDialogReasoningNavigation(t *testing.T) {
	d := &modelDialog{
		active:        true,
		view:          []provider.Model{{Provider: "openai-codex", ID: "gpt-5.6-luna", Reasoning: true}},
		reasoning:     "high",
		showReasoning: true,
	}

	act := d.HandleKey(tui.Key{Kind: tui.KeyRight})
	if !act.ReasoningChanged || act.Reasoning != "xhigh" || d.reasoning != "xhigh" {
		t.Fatalf("right action = %+v, dialog reasoning = %q; want xhigh", act, d.reasoning)
	}
	act = d.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: 'L'})
	if !act.ReasoningChanged || act.Reasoning != "max" || d.reasoning != "max" {
		t.Fatalf("L action = %+v, dialog reasoning = %q; want max", act, d.reasoning)
	}
	act = d.HandleKey(tui.Key{Kind: tui.KeyLeft})
	if !act.ReasoningChanged || act.Reasoning != "xhigh" {
		t.Fatalf("left action = %+v; want xhigh", act)
	}
	act = d.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: 'h'})
	if !act.ReasoningChanged || act.Reasoning != "high" {
		t.Fatalf("h action = %+v; want high", act)
	}
}

func TestModelDialogReasoningNavigationStopsAtBounds(t *testing.T) {
	d := &modelDialog{
		active:    true,
		view:      []provider.Model{{Provider: "openai-codex", ID: "gpt-5.6-luna", Reasoning: true}},
		reasoning: "",
	}

	if act := d.HandleKey(tui.Key{Kind: tui.KeyLeft}); act.ReasoningChanged {
		t.Fatalf("left at off changed reasoning: %+v", act)
	}
	d.reasoning = "max"
	if act := d.HandleKey(tui.Key{Kind: tui.KeyRight}); act.ReasoningChanged {
		t.Fatalf("right at max changed reasoning: %+v", act)
	}
}

func TestModelDialogReasoningNavigationAppliesToInteractive(t *testing.T) {
	d := &modelDialog{
		active:    true,
		view:      []provider.Model{{Provider: "openai-codex", ID: "gpt-5.6-luna", Reasoning: true}},
		reasoning: "high",
	}
	var changed string
	i := &Interactive{
		cfg: InteractiveConfig{
			Reasoning:          "high",
			OnReasoningChanged: func(level string) { changed = level },
		},
		modelDialog: d,
	}

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRight})
	if i.cfg.Reasoning != "xhigh" || changed != "xhigh" {
		t.Fatalf("reasoning = %q, callback = %q; want xhigh", i.cfg.Reasoning, changed)
	}
}

func TestModelDialogReasoningUsesLevelColor(t *testing.T) {
	tests := []struct {
		level string
		color tui.TerminalColor
	}{
		{"off", tui.Dark.Muted},
		{"low", tui.Dark.Accent},
		{"high", tui.Dark.Warning},
		{"max", tui.Dark.ThinkingMax},
	}
	for _, tt := range tests {
		if got := reasoningLevelColor(tui.Dark, tt.level); got != tt.color {
			t.Errorf("reasoningLevelColor(%q) = %#v, want %#v", tt.level, got, tt.color)
		}
	}

	d := &modelDialog{active: true, showReasoning: true, reasoning: "max"}
	text := strings.Join(d.Render(tui.Dark, 100), "\n")
	if !strings.Contains(text, tui.Dark.FGColor(tui.Dark.ThinkingMax, "max")) {
		t.Fatalf("max reasoning color missing from %q", text)
	}
	if !strings.Contains(text, "←/→ or h/l to change") {
		t.Fatalf("reasoning navigation hint missing from %q", text)
	}
}
