package tui

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"

	"github.com/bnema/zut/packages/provider"
)

func TestCompactionSummaryRendersSeparator(t *testing.T) {
	th := Dark
	summary := provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "## Context Summary (compacted)\n\nsecret summary"}},
		Meta: map[string]string{"compaction": "true", "tokens_before": "182400"}}
	v := &View{Theme: th, Messages: []provider.Message{summary}}
	out := stripANSI(strings.Join(v.Build(80), "\n"))
	if !strings.Contains(out, "── compacted · ~182k tokens · older messages not in model context · ctrl+o ─") {
		t.Fatalf("separator missing:\n%s", out)
	}
	if strings.Contains(out, "secret summary") {
		t.Fatal("collapsed summary text shown")
	}
	v.ExpandAll = true
	if !strings.Contains(stripANSI(strings.Join(v.Build(80), "\n")), "secret summary") {
		t.Fatal("expanded summary text missing")
	}
}

func TestCompactionSeparatorWidth(t *testing.T) {
	for _, w := range []int{1, 5, 6, 20, 80} {
		if got := runewidth.StringWidth(stripANSI(CompactionSeparator(Dark, w, "~1k tokens"))); got != w {
			t.Fatalf("width %d rendered %d cells", w, got)
		}
	}
	if CompactionSeparator(Dark, 0) != "" {
		t.Fatal("zero width rendered text")
	}
	if got := stripANSI(CompactionSeparator(Dark, 40, "", "x")); !strings.Contains(got, "compacted · x") {
		t.Fatalf("empty part not skipped: %q", got)
	}
	for raw, want := range map[string]string{"182400": "~182k tokens", "999": "~999 tokens", "": "", "x": "", "-3": ""} {
		if got := compactionTokensLabel(raw); got != want {
			t.Fatalf("compactionTokensLabel(%q) = %q, want %q", raw, got, want)
		}
	}
}
