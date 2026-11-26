package modes

import (
	"context"
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/require"

	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

func TestModelDialogReasoningNavigation(t *testing.T) {
	d := &modelDialog{
		active:        true,
		view:          pickerView(provider.Model{Provider: "openai-codex", ID: "gpt-5.6-luna", Reasoning: true}),
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
	act = d.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: 'H'})
	if !act.ReasoningChanged || act.Reasoning != "high" {
		t.Fatalf("H action = %+v; want high", act)
	}
}

func TestModelDialogReasoningAllowsLowercaseFilter(t *testing.T) {
	d := &modelDialog{active: true, showReasoning: true}
	for _, r := range "claude" {
		d.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: r})
	}
	if d.query.String() != "claude" {
		t.Fatalf("query = %q, want claude", d.query.String())
	}
}

func TestModelDialogWithoutReasoningTreatsHAsFilter(t *testing.T) {
	d := &modelDialog{active: true}
	d.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: 'H'})
	if d.query.String() != "H" {
		t.Fatalf("embedded picker query = %q, want H", d.query.String())
	}
}

func TestModelDialogReasoningNavigationStopsAtBounds(t *testing.T) {
	d := &modelDialog{
		active:        true,
		showReasoning: true,
		view:          pickerView(provider.Model{Provider: "openai-codex", ID: "gpt-5.6-luna", Reasoning: true}),
		reasoning:     "",
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
		active:        true,
		showReasoning: true,
		view:          pickerView(provider.Model{Provider: "openai-codex", ID: "gpt-5.6-luna", Reasoning: true}),
		reasoning:     "high",
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

func TestModelDialogWithoutReasoningIgnoresArrows(t *testing.T) {
	d := &modelDialog{
		active:    true,
		view:      pickerView(provider.Model{Provider: "openai-codex", ID: "gpt-5.6-luna", Reasoning: true}),
		reasoning: "high",
	}
	if act := d.HandleKey(tui.Key{Kind: tui.KeyRight}); act.ReasoningChanged || d.reasoning != "high" {
		t.Fatalf("hidden reasoning changed: action=%+v reasoning=%q", act, d.reasoning)
	}
}

func TestModelDialogReasoningReflectsHighlightedModel(t *testing.T) {
	d := &modelDialog{
		active:        true,
		showReasoning: true,
		reasoning:     "high",
		view:          pickerView(provider.Model{Provider: "google", ID: "gemini-2.0-flash"}),
	}
	text := stripANSIBytes(strings.Join(d.Render(tui.Dark, 100, 30), "\n"))
	if !strings.Contains(text, "reasoning: off") {
		t.Fatalf("effective reasoning level missing from %q", text)
	}
	if !strings.Contains(text, "unavailable for highlighted model") {
		t.Fatalf("unsupported-model hint missing from %q", text)
	}
	if strings.Contains(text, "H/L to change") {
		t.Fatalf("unsupported model advertises reasoning controls: %q", text)
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

	d := &modelDialog{
		active:        true,
		showReasoning: true,
		reasoning:     "max",
		view:          pickerView(provider.Model{Provider: "openai-codex", ID: "gpt-5.6-luna", Reasoning: true}),
	}
	text := strings.Join(d.Render(tui.Dark, 100, 30), "\n")
	if !strings.Contains(text, tui.Dark.FGColor(tui.Dark.ThinkingMax, "max")) {
		t.Fatalf("max reasoning color missing from %q", text)
	}
	if !strings.Contains(text, "←/→ or H/L to change") {
		t.Fatalf("reasoning navigation hint missing from %q", text)
	}
}

func pickerView(models ...provider.Model) []provider.PickerModel {
	out := make([]provider.PickerModel, len(models))
	for i, m := range models {
		out[i] = provider.PickerModel{Model: m}
	}
	return out
}

func pickerTestModels() []provider.Model {
	return []provider.Model{
		{Provider: "anthropic", ID: "claude-opus-4-5", DisplayName: "Claude Opus 4.5", Reasoning: true, ContextWindow: 200000, MaxOutput: 64000, PriceInput: 5, PriceOutput: 25, PriceCacheRead: 0.5},
		{Provider: "anthropic", ID: "claude-opus-4-5-20251101", DisplayName: "Claude Opus 4.5"},
		{Provider: "anthropic", ID: "claude-opus-5-5", DisplayName: "Claude Opus 5.5", Reasoning: true, ContextWindow: 1 << 20},
		{Provider: "anthropic", ID: "claude-opus-4-8", DisplayName: "Claude Opus 4.8", Speculative: true},
		{Provider: "openai", ID: "gpt-5.2", DisplayName: "GPT-5.2"},
		{Provider: "openrouter", ID: "anthropic/claude-opus-4.5", DisplayName: "Anthropic: Claude Opus 4.5"},
	}
}

func modelPickerText(d *modelDialog, width, height int) []string {
	rows := d.Render(tui.Dark, width, height)
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = stripANSIBytes(r)
	}
	return out
}

func TestModelPickerGroupsByProviderCurrentFirst(t *testing.T) {
	d := &modelDialog{active: true}
	d.setModels(pickerTestModels(), "gpt-5.2")

	require.Equal(t, []string{"", "openai", "anthropic", "openrouter"}, d.tabs)
	require.Equal(t, "gpt-5.2", d.view[d.cursor].ID, "cursor starts on the current model")
	var ids []string
	for _, m := range d.view {
		ids = append(ids, m.ID)
	}
	require.Equal(t, []string{"gpt-5.2", "claude-opus-5-5", "claude-opus-4-8", "claude-opus-4-5", "anthropic/claude-opus-4.5"}, ids,
		"newest first, dated snapshot hidden without a query")

	rows := modelPickerText(d, 80, 30)
	for _, row := range rows {
		require.Equal(t, 80, runewidth.StringWidth(row), "row %q", row)
	}
	text := strings.Join(rows, "\n")
	require.Contains(t, text, "[all] openai anthropic openrouter")
	require.Contains(t, text, " OpenAI · 1")
	require.Contains(t, text, " Anthropic (Claude Pro/Max) · 3")
	require.Contains(t, text, "Claude Opus 4.8 speculative")
	require.Regexp(t, `▸ GPT-5\.2 +gpt-5\.2 ● `, text)
}

func TestModelPickerTabFiltersProvider(t *testing.T) {
	d := &modelDialog{active: true}
	d.setModels(pickerTestModels(), "gpt-5.2")
	d.HandleKey(tui.Key{Kind: tui.KeyTab})
	d.HandleKey(tui.Key{Kind: tui.KeyTab})
	require.Equal(t, "anthropic", d.tabs[d.tab])
	for _, m := range d.view {
		require.Equal(t, "anthropic", m.Provider)
	}
	text := strings.Join(modelPickerText(d, 80, 30), "\n")
	require.NotContains(t, text, "Anthropic (Claude Pro/Max) ·", "a provider tab has no headers")

	d.HandleKey(tui.Key{Kind: tui.KeyShiftTab})
	d.HandleKey(tui.Key{Kind: tui.KeyShiftTab})
	require.Equal(t, 0, d.tab)
}

func TestModelPickerQueryRanksAndShowsSnapshots(t *testing.T) {
	d := &modelDialog{active: true}
	d.setModels(pickerTestModels(), "")
	for _, r := range "opus45" {
		d.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: r})
	}
	var ids []string
	for _, m := range d.view {
		ids = append(ids, m.ID)
	}
	require.ElementsMatch(t, []string{"claude-opus-4-5", "claude-opus-4-5-20251101", "anthropic/claude-opus-4.5"}, ids[:3],
		"literal matches first, snapshot listed while searching")

	text := strings.Join(modelPickerText(d, 100, 30), "\n")
	require.Contains(t, text, "anthropic · Claude Opus 4.5 snapshot", "mixed providers show the provider as group")
	require.NotContains(t, text, "· 3", "no group headers while searching")
}

func TestModelPickerDetailLine(t *testing.T) {
	d := &modelDialog{active: true}
	d.setModels(pickerTestModels(), "claude-opus-4-5")
	text := strings.Join(modelPickerText(d, 140, 30), "\n")
	require.Contains(t, text, "200k ctx · 64k out · $5 in / $25 out per 1M · cache $0.5 · reasoning off→")
	require.Equal(t, "1M", modelTokens(1<<20))
	require.Equal(t, "128k", modelTokens(131072))
	require.Equal(t, "1.5M", modelTokens(1_500_000))
}

func TestModelPickerFitsShortPane(t *testing.T) {
	d := &modelDialog{active: true, showReasoning: true}
	d.setModels(pickerTestModels(), "anthropic/claude-opus-4.5")
	rows := d.Render(tui.Dark, 60, 10)
	require.LessOrEqual(t, len(rows), 11)
	require.Contains(t, stripANSIBytes(strings.Join(rows, "\n")), "Anthropic: Claude Opus 4.5", "the cursor row stays visible")
	r, c := d.CursorPos()
	require.Equal(t, modelPickerQueryRow, r)
	require.Equal(t, 3, c)
	require.Contains(t, stripANSIBytes(rows[r]), " > ")
}
