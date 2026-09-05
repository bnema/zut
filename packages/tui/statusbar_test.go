package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func TestStatusBarCompact(t *testing.T) {
	lines := StatusBar(StatusBarParams{
		Theme: Dark, Provider: "openai-codex", Model: "gpt-6-astra", Reasoning: "medium", Cols: 120,
		Usage: provider.Usage{InputTokens: 261_000, OutputTokens: 34_000, CacheReadTokens: 12_000_000,
			CacheMeasuredPromptTokens: 1000, CacheMeasuredReadTokens: 979, CostUSD: 8.522},
		Subscription: true, ContextUsed: 155_000, ContextMax: 500_000, WeeklyUsage: "weekly:69%",
		CWD: "/tmp/x",
	})
	if len(lines) != 2 {
		t.Fatalf("want stats + cwd, got %q", lines)
	}
	want := "  gpt-6-astra:medium  ↑261k ↓34k R12M C98% $8.52 weekly:69% ctx31%/500k"
	if got := stripANSI(lines[0]); got != want {
		t.Fatalf("status = %q, want %q", got, want)
	}
	if got := stripANSI(lines[1]); got != "  /tmp/x" {
		t.Fatalf("cwd = %q", got)
	}
}

func TestStatusBarShowsCacheHitRatio(t *testing.T) {
	lines := StatusBar(StatusBarParams{
		Theme: Dark, Provider: "openai-codex", Model: "gpt-test", Cols: 200,
		Usage: provider.Usage{InputTokens: 84_000, OutputTokens: 1_500, CacheReadTokens: 123_000,
			CacheMeasuredPromptTokens: 207_000, CacheMeasuredReadTokens: 123_000},
		ContextUsed: 45_152, ContextMax: 272_000,
	})
	plain := stripANSI(strings.Join(lines, "\n"))
	if !strings.Contains(plain, "↑84k ↓1.5k R123k C59%") {
		t.Fatalf("status bar = %q, want compact cache hit ratio", plain)
	}
}

func TestStatusBarOmitsCacheRatioWhenCacheDetailsAreUnavailable(t *testing.T) {
	lines := StatusBar(StatusBarParams{
		Theme: Dark, Provider: "compatible", Model: "model", Cols: 200,
		Usage: provider.Usage{InputTokens: 84_000, CacheReadTokens: 123_000},
	})
	plain := stripANSI(strings.Join(lines, "\n"))
	if strings.Contains(plain, "C") {
		t.Fatalf("status bar = %q, want no cache ratio without measured details", plain)
	}
}

func TestStatusBarNarrow(t *testing.T) {
	for _, cols := range []int{24, 30, 32, 40, 64} {
		for _, busy := range []string{"", "⠋ Working"} {
			t.Run(fmt.Sprintf("%d/busy=%t", cols, busy != ""), func(t *testing.T) {
				lines := StatusBar(StatusBarParams{
					Theme: Dark, Provider: "openai-codex", Model: "gpt-5.5", Reasoning: "minimum", Cols: cols,
					BusyPrefix: busy, CWD: "/tmp/x", FastMode: true, WeeklyUsage: "weekly:69%",
					Usage: provider.Usage{InputTokens: 84_000, OutputTokens: 1_500, CacheReadTokens: 123_000,
						CacheWriteTokens: 2_000, CacheMeasuredPromptTokens: 209_000, CacheMeasuredReadTokens: 123_000, CostUSD: 0.525},
					ContextUsed: 209_000, ContextMax: 272_000,
				})
				plain := stripANSI(strings.Join(lines, "\n"))
				for _, want := range []string{"gpt-5.5:minimal", "fast mode", "↑84k", "↓1.5k", "R123k", "C59%", "W2.0k", "$0.53", "weekly:69%", "ctx77%/272k", "/tmp/x"} {
					if !strings.Contains(plain, want) {
						t.Fatalf("missing %q in %q", want, plain)
					}
				}
				for _, line := range lines {
					if width := visibleWidth(line); width > cols {
						t.Fatalf("line width = %d, want <= %d: %q", width, cols, stripANSI(line))
					}
				}
				if busy != "" && stripANSI(lines[0]) != "  "+busy {
					t.Fatalf("busy prefix should stay on its own row: %q", lines)
				}
			})
		}
	}
}

func TestStatusBarVeryNarrowModel(t *testing.T) {
	for _, busy := range []string{"", "⠋"} {
		lines := StatusBar(StatusBarParams{Theme: Dark, Model: "模型-with-long-name", Reasoning: "max", BusyPrefix: busy, Cols: 10})
		for _, line := range lines {
			if visibleWidth(line) > 10 {
				t.Fatalf("model overflows narrow terminal: %q", line)
			}
		}
	}
}

func TestStatusBarShowsActiveGoal(t *testing.T) {
	for _, cols := range []int{100, 20} {
		lines := StatusBar(StatusBarParams{Theme: Dark, Model: "gpt-test", GoalStatus: "active", CWD: "/tmp/project", Cols: cols})
		if !strings.Contains(stripANSI(strings.Join(lines, "\n")), "goal:active") {
			t.Fatalf("status bar = %q, want active goal", lines)
		}
	}
}

func TestStatusBarAlwaysTwoLines(t *testing.T) {
	lines := StatusBar(StatusBarParams{
		Theme: Dark, Provider: "anthropic", Model: "claude-opus-4-7", CWD: "/tmp/x",
		Usage:        provider.Usage{InputTokens: 476_000, OutputTokens: 3_400, CostUSD: 1.242},
		Subscription: true, ContextUsed: 55_000, ContextMax: 1_000_000, Cols: 500,
	})
	if len(lines) != 2 || !strings.Contains(lines[0], "claude-opus-4-7") || stripANSI(lines[1]) != "  /tmp/x" {
		t.Fatalf("want model/stats followed by indented cwd: %q", lines)
	}
}

func TestStatusBarNoCWD(t *testing.T) {
	lines := StatusBar(StatusBarParams{Theme: Dark, Model: "gpt-5.4", Cols: 200})
	if len(lines) != 1 {
		t.Fatalf("empty cwd: want 1 line, got %q", lines)
	}
}

func TestStatusBarShowsFastMode(t *testing.T) {
	lines := StatusBar(StatusBarParams{Theme: Dark, Model: "gpt-5.6-luna", FastMode: true, CWD: "/tmp/x", Cols: 200})
	if len(lines) != 2 || !strings.Contains(stripANSI(lines[0]), "fast mode") {
		t.Fatalf("fast mode should be visible: %q", lines)
	}
}

func TestStatusBarReasoningSuffix(t *testing.T) {
	for _, tc := range []struct{ level, want string }{
		{"medium", "gpt-test:medium"}, {"minimum", "gpt-test:minimal"}, {"xhigh", "gpt-test:xhigh"},
		{"", "gpt-test"}, {"off", "gpt-test"},
	} {
		lines := StatusBar(StatusBarParams{Theme: Dark, Provider: "openai-codex", Model: "gpt-test", Reasoning: tc.level, Cols: 200})
		if got := stripANSI(lines[0]); got != "  "+tc.want {
			t.Fatalf("reasoning %q: got %q, want %q", tc.level, got, tc.want)
		}
	}
}

func TestStatusBarUsesReasoningMaxThemeColor(t *testing.T) {
	th := Dark
	th.ThinkingMax = Color256(201)
	for _, cols := range []int{200, 25} {
		lines := StatusBar(StatusBarParams{Theme: th, Model: "gpt-5.6-sol", Reasoning: "max", Cols: cols, WeeklyUsage: "weekly:0%"})
		if !strings.Contains(lines[0], "\x1b[38;5;201m") || !strings.Contains(stripANSI(lines[0]), "gpt-5.6-sol:max") {
			t.Fatalf("max reasoning style missing: %q", lines)
		}
	}
}

func TestStatusBarContextWarningsAndCompaction(t *testing.T) {
	for _, tc := range []struct {
		used  int
		color TerminalColor
	}{{75, Dark.Warning}, {95, Dark.Error}} {
		lines := StatusBar(StatusBarParams{Theme: Dark, Model: "model", ContextUsed: tc.used, ContextMax: 100, AutoCompacting: true, Cols: 200})
		want := Dark.FGColor(tc.color, fmt.Sprintf("ctx%d%%/100 (auto)", tc.used))
		if !strings.Contains(lines[0], want) {
			t.Fatalf("context warning missing: %q", lines)
		}
	}
}

func TestStatusBarNoYoloTagPrecedesCWD(t *testing.T) {
	lines := StatusBar(StatusBarParams{Theme: Dark, Model: "gpt-5.5", CWD: "/tmp/x", NoYolo: true, Cols: 200})
	if len(lines) != 2 || !strings.Contains(stripANSI(lines[1]), "yolo mode disabled - /tmp/x") {
		t.Fatalf("cwd line should include no-yolo tag: %q", lines)
	}
}
