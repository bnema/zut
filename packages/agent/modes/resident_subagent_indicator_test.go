package modes

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
	"github.com/mattn/go-runewidth"
)

func TestRenderResidentSubagentActivityLinesShowsOnlyActiveChildren(t *testing.T) {
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	lines := renderResidentSubagentActivityLines(tui.Dark, "/", []subagents.ResidentSnapshot{
		{ID: "running-id", Profile: "reviewer", State: subagents.ResidentRunning, TurnStartedAt: now.Add(-90 * time.Second), ActivityUpdatedAt: now, WaitingForModel: true},
		{ID: "queued-id", Profile: "planner", State: subagents.ResidentQueued, TurnStartedAt: now.Add(-10 * time.Second), ActivityUpdatedAt: now.Add(-5 * time.Second)},
		{ID: "foreign-id", Profile: "another process", State: subagents.ResidentRunning, OwnedElsewhere: true, TurnStartedAt: now.Add(-10 * time.Second)},
		{ID: "idle-id", Profile: "finished", State: subagents.ResidentIdle},
	}, 80, now)
	got := strings.Join(plainResidentIndicatorLines(lines), "\n")
	if !strings.Contains(got, "/ reviewer · Zzzz · activity 0s ago · 1m30s") || !strings.Contains(got, "… planner · queued · activity 5s ago · 10s") {
		t.Fatalf("activity lines = %q", got)
	}
	if strings.Contains(got, "finished") || strings.Contains(got, "another process") {
		t.Fatalf("activity lines include inactive or foreign child: %q", got)
	}
}

type residentIndicatorTerminal struct {
	alertTestTerminal
	cols int
	rows int
}

func (t *residentIndicatorTerminal) Size() (int, int) { return t.cols, t.rows }
func (t *residentIndicatorTerminal) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = nil
}

func TestInteractiveResidentSubagentActivityRendersAllActiveChildrenWhenHeightAllows(t *testing.T) {
	manager := subagents.NewResidentManagerWithLimit(t.TempDir(), 16, func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
		return func(ctx context.Context, _ string) error {
			<-ctx.Done()
			return ctx.Err()
		}, nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	for index := 0; index < 10; index++ {
		spec := subagents.ResidentChildSpec{
			ID: fmt.Sprintf("child-%02d", index), SessionID: fmt.Sprintf("session-%02d", index), Profile: fmt.Sprintf("agent-%02d", index),
			Provider: "openai", Model: "gpt-test",
		}
		if _, err := manager.Spawn(context.Background(), spec, "task"); err != nil {
			t.Fatalf("Spawn(%d): %v", index, err)
		}
	}

	term := &residentIndicatorTerminal{cols: 80, rows: 40}
	interactive := NewInteractive(InteractiveConfig{Terminal: term, ResidentManager: manager, Theme: tui.Dark})
	interactive.clock = func() time.Time { return time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC) }
	interactive.rend.Resize(term.cols, term.rows)
	term.Reset()
	interactive.redraw()

	plain := stripANSIBytes(term.String())
	for index := 0; index < 10; index++ {
		if want := fmt.Sprintf("agent-%02d", index); !strings.Contains(plain, want) {
			t.Fatalf("interactive output missing %q:\n%s", want, plain)
		}
	}

	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	term.Reset()
	interactive.redraw()
	plain = stripANSIBytes(term.String())
	if strings.Contains(plain, "agent-00") {
		t.Fatalf("interactive output kept finished subagent activity:\n%s", plain)
	}
}

func TestRenderResidentSubagentActivityLinesShowsEveryActiveChildProvided(t *testing.T) {
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	var snapshots []subagents.ResidentSnapshot
	for index := 0; index < 12; index++ {
		snapshots = append(snapshots, subagents.ResidentSnapshot{
			ID: fmt.Sprintf("child-%02d", index), Profile: fmt.Sprintf("agent-%02d", index), State: subagents.ResidentRunning,
			TurnStartedAt: now.Add(-time.Minute), ActivityUpdatedAt: now,
		})
	}
	lines := renderResidentSubagentActivityLines(tui.Dark, "/", snapshots, 80, now)
	plain := strings.Join(plainResidentIndicatorLines(lines), "\n")
	for index := 0; index < 12; index++ {
		if want := fmt.Sprintf("agent-%02d", index); !strings.Contains(plain, want) {
			t.Fatalf("activity lines missing %q:\n%s", want, plain)
		}
	}
}

func TestRenderResidentSubagentActivityLinesRightAlignsUsageMetadataWhenItFits(t *testing.T) {
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	const width = 120
	lines := renderResidentSubagentActivityLines(tui.Dark, "/", []subagents.ResidentSnapshot{{
		ID: "running-id", Profile: "審査", State: subagents.ResidentRunning,
		Usage:       provider.Usage{InputTokens: 84_000, OutputTokens: 1_500, CacheReadTokens: 123_000, CacheMeasuredPromptTokens: 207_000, CacheMeasuredReadTokens: 123_000, CostUSD: 0.525},
		ContextUsed: 45_152, ContextMax: 272_000, Subscription: true,
	}}, width, now)
	plain := plainResidentIndicatorLines(lines)
	if len(plain) != 1 {
		t.Fatalf("activity lines = %#v, want one right-aligned row", plain)
	}
	metadata := "↑84k ↓1.5k R123k/ C59.4% $0.525 (sub) 16.6%/272k"
	if !strings.HasSuffix(plain[0], metadata) {
		t.Fatalf("activity line = %q, want metadata suffix %q", plain[0], metadata)
	}
	if gotWidth := runewidth.StringWidth(plain[0]); gotWidth != width {
		t.Fatalf("activity line width = %d, want %d: %q", gotWidth, width, plain[0])
	}
}

func TestRenderResidentSubagentActivityLinesWrapsUsageMetadataWhenItDoesNotFit(t *testing.T) {
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	const width = 34
	lines := renderResidentSubagentActivityLines(tui.Dark, "/", []subagents.ResidentSnapshot{{
		ID: "running-id", Profile: "reviewer", State: subagents.ResidentRunning,
		Usage:       provider.Usage{InputTokens: 84_000, OutputTokens: 1_500, CacheReadTokens: 123_000, CacheMeasuredPromptTokens: 207_000, CacheMeasuredReadTokens: 123_000, CostUSD: 0.525},
		ContextUsed: 45_152, ContextMax: 272_000, Subscription: true,
	}}, width, now)
	plain := plainResidentIndicatorLines(lines)
	if len(plain) < 3 {
		t.Fatalf("activity lines = %#v, want wrapped metadata", plain)
	}
	if strings.Contains(plain[0], "↑84k") || !strings.HasPrefix(plain[1], "    ") {
		t.Fatalf("activity lines = %#v, want metadata on indented wrapped lines", plain)
	}
	joinedMetadata := strings.Join(plain[1:], " ")
	for _, want := range []string{"↑84k", "↓1.5k", "$0.525 (sub)", "16.6%/272k"} {
		if !strings.Contains(joinedMetadata, want) {
			t.Fatalf("wrapped metadata = %q, missing %q", joinedMetadata, want)
		}
	}
	for _, line := range plain {
		if gotWidth := runewidth.StringWidth(line); gotWidth > width {
			t.Fatalf("wrapped line width = %d, want <= %d: %q", gotWidth, width, line)
		}
	}
}

func TestRenderResidentSubagentActivityLinesAnimatesModelWait(t *testing.T) {
	started := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	now := started.Add(2 * time.Second)
	lines := renderResidentSubagentActivityLines(tui.Dark, "/", []subagents.ResidentSnapshot{{
		ID: "running-id", Profile: "reviewer", State: subagents.ResidentRunning,
		TurnStartedAt: started, ActivityUpdatedAt: started, WaitingForModel: true,
	}}, 80, now)
	got := strings.Join(plainResidentIndicatorLines(lines), "\n")
	if !strings.Contains(got, "zzZz") {
		t.Fatalf("model-wait indicator = %q, want animated waiting frame", got)
	}
}

func TestRenderResidentSubagentActivityLinesShowsRunningDuringToolWork(t *testing.T) {
	now := time.Date(2026, time.August, 13, 12, 0, 2, 0, time.UTC)
	lines := renderResidentSubagentActivityLines(tui.Dark, "/", []subagents.ResidentSnapshot{{
		ID: "running-id", Profile: "reviewer", State: subagents.ResidentRunning,
		TurnStartedAt: now.Add(-time.Minute), ActivityUpdatedAt: now.Add(-time.Second),
	}}, 80, now)
	got := strings.Join(plainResidentIndicatorLines(lines), "\n")
	if !strings.Contains(got, "/ reviewer · running · activity 1s ago · 1m00s") || strings.Contains(got, "Zzzz") {
		t.Fatalf("tool-work indicator = %q", got)
	}
}

func TestFitResidentSubagentActivityLinesUsesAvailableHeightBeforeOverflow(t *testing.T) {
	lines := []string{"  one", "  two", "  three", "  four"}
	bottomRows := func(candidate []string) int { return 2 + len(candidate) }

	allFit := plainResidentIndicatorLines(fitResidentSubagentActivityLines(tui.Dark, lines, 0, 6, 80, bottomRows))
	if strings.Join(allFit, "\n") != strings.Join(lines, "\n") {
		t.Fatalf("fit lines = %#v, want all activity rows", allFit)
	}

	compact := plainResidentIndicatorLines(fitResidentSubagentActivityLines(tui.Dark, lines, 0, 4, 80, bottomRows))
	wantCompact := []string{"  one", "  … 3 more active subagents"}
	if strings.Join(compact, "\n") != strings.Join(wantCompact, "\n") {
		t.Fatalf("compact lines = %#v, want %#v", compact, wantCompact)
	}
}

func TestLimitResidentSubagentActivityLinesSummarizesOverflow(t *testing.T) {
	got := plainResidentIndicatorLines(limitResidentSubagentActivityLines(tui.Dark, []string{"one", "two", "three"}, 0, 2, 80))
	want := []string{"one", "  … 2 more active subagents"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("limited lines = %#v, want %#v", got, want)
	}
}

func TestLimitResidentSubagentActivityLinesKeepsMetadataWithItsAgent(t *testing.T) {
	lines := []string{"  agent one", "    usage one", "  agent two", "    usage two"}
	got := plainResidentIndicatorLines(limitResidentSubagentActivityLines(tui.Dark, lines, 0, 3, 80))
	want := []string{"  agent one", "    usage one", "  … 1 more active subagents"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("limited lines = %#v, want %#v", got, want)
	}
}

func TestLimitResidentSubagentActivityLinesIncludesAlreadyHiddenChildren(t *testing.T) {
	got := plainResidentIndicatorLines(limitResidentSubagentActivityLines(tui.Dark, []string{"one", "two", "three"}, 4, 2, 80))
	want := []string{"one", "  … 6 more active subagents"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("limited lines = %#v, want %#v", got, want)
	}
}

func plainResidentIndicatorLines(lines []string) []string {
	plain := make([]string, len(lines))
	for index, line := range lines {
		plain[index] = stripANSIBytes(line)
	}
	return plain
}
