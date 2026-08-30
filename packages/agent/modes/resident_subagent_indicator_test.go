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
	spawnRunningResidentIndicatorChildren(t, manager, 10)

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

func TestInteractiveResidentSubagentActivitySummarizesBoundedPageUnderShortHeight(t *testing.T) {
	manager := subagents.NewResidentManagerWithLimit(t.TempDir(), 16, func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
		return func(ctx context.Context, _ string) error {
			<-ctx.Done()
			return ctx.Err()
		}, nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	spawnRunningResidentIndicatorChildren(t, manager, 6)

	term := &residentIndicatorTerminal{cols: 80, rows: 8}
	interactive := NewInteractive(InteractiveConfig{Terminal: term, ResidentManager: manager, Theme: tui.Dark})
	interactive.clock = func() time.Time { return time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC) }
	interactive.rend.Resize(term.cols, term.rows)
	term.Reset()
	interactive.redraw()

	plain := stripANSIBytes(term.String())
	if count := strings.Count(plain, "agent-"); count != 1 {
		t.Fatalf("interactive output showed %d active child rows, want one fitting row under short height:\n%s", count, plain)
	}
	if !strings.Contains(plain, "… 5 more active subagents") {
		t.Fatalf("interactive output did not summarize omitted active children:\n%s", plain)
	}
}

func TestInteractiveResidentSubagentActivityShowsAllActiveChildrenWhenShortHeightFits(t *testing.T) {
	manager := subagents.NewResidentManagerWithLimit(t.TempDir(), 16, func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
		return func(ctx context.Context, _ string) error {
			<-ctx.Done()
			return ctx.Err()
		}, nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	spawnRunningResidentIndicatorChildren(t, manager, 2)

	term := &residentIndicatorTerminal{cols: 80, rows: 8}
	interactive := NewInteractive(InteractiveConfig{Terminal: term, ResidentManager: manager, Theme: tui.Dark})
	interactive.clock = func() time.Time { return time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC) }
	interactive.rend.Resize(term.cols, term.rows)
	term.Reset()
	interactive.redraw()

	plain := stripANSIBytes(term.String())
	for index := 0; index < 2; index++ {
		if want := fmt.Sprintf("agent-%02d", index); !strings.Contains(plain, want) {
			t.Fatalf("interactive output missing fitting active child %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "more active subagents") {
		t.Fatalf("interactive output summarized fitting active children:\n%s", plain)
	}
}

func spawnRunningResidentIndicatorChildren(t *testing.T, manager *subagents.ResidentManager, count int) {
	t.Helper()
	updates := make(chan string, count*2)
	manager.SetUpdateObserver(func(childID string) {
		select {
		case updates <- childID:
		default:
		}
	})
	spawnResidentIndicatorChildren(t, manager, count)
	defer manager.SetUpdateObserver(nil)

	running := make(map[string]struct{}, count)
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for len(running) < count {
		select {
		case childID := <-updates:
			snapshot, ok := manager.SnapshotFor(childID)
			if ok && snapshot.State == subagents.ResidentRunning {
				running[childID] = struct{}{}
			}
		case <-timer.C:
			snapshots, _ := manager.ActiveSnapshotPage(0)
			t.Fatalf("running resident children = %d, want %d; active snapshots=%#v", len(running), count, snapshots)
		}
	}
}

func spawnResidentIndicatorChildren(t *testing.T, manager *subagents.ResidentManager, count int) {
	t.Helper()
	for index := 0; index < count; index++ {
		spec := subagents.ResidentChildSpec{
			ID: fmt.Sprintf("child-%02d", index), SessionID: fmt.Sprintf("session-%02d", index), Profile: fmt.Sprintf("agent-%02d", index),
			Provider: "openai", Model: "gpt-test",
		}
		if _, err := manager.Spawn(context.Background(), spec, "task"); err != nil {
			t.Fatalf("Spawn(%d): %v", index, err)
		}
	}
}

func TestRenderResidentSubagentActivityPageReportsHiddenActiveChildren(t *testing.T) {
	manager := subagents.NewResidentManagerWithLimit(t.TempDir(), 16, func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
		return func(ctx context.Context, _ string) error {
			<-ctx.Done()
			return ctx.Err()
		}, nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	spawnResidentIndicatorChildren(t, manager, 5)

	lines, hidden := renderResidentSubagentActivityPage(tui.Dark, manager, "/", 2, 80, time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC))
	plain := strings.Join(plainResidentIndicatorLines(lines), "\n")
	if hidden != 3 {
		t.Fatalf("hidden active children = %d, want 3", hidden)
	}
	if count := strings.Count(plain, "agent-"); count != 2 {
		t.Fatalf("activity page rendered %d child rows, want bounded page of 2:\n%s", count, plain)
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

func TestResidentUsageMetadataShowsRolloutBudget(t *testing.T) {
	metadata := residentUsageMetadata(subagents.ResidentSnapshot{
		Budget: subagents.BudgetSnapshot{Used: 337_500, Limit: 375_000, Percent: 90, State: subagents.BudgetFinalizing},
	})
	if metadata != "budget:finalizing 90%/375k" {
		t.Fatalf("budget metadata = %q", metadata)
	}
}

func TestCompactResidentTokensAvoidsThousandKBoundary(t *testing.T) {
	for _, tc := range []struct {
		tokens int64
		want   string
	}{
		{tokens: 999_499, want: "999k"},
		{tokens: 999_500, want: "1.0M"},
		{tokens: 999_999, want: "1.0M"},
		{tokens: 1_000_000, want: "1.0M"},
	} {
		if got := compactResidentTokens(tc.tokens); got != tc.want {
			t.Errorf("compactResidentTokens(%d) = %q, want %q", tc.tokens, got, tc.want)
		}
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

func TestFitResidentSubagentActivityLinesKeepsActivityWhenWrappedMetadataDoesNotFit(t *testing.T) {
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	const width = 34
	lines := renderResidentSubagentActivityLines(tui.Dark, "/", []subagents.ResidentSnapshot{{
		ID: "running-id", Profile: "reviewer", State: subagents.ResidentRunning,
		Usage:       provider.Usage{InputTokens: 84_000, OutputTokens: 1_500, CacheReadTokens: 123_000, CacheMeasuredPromptTokens: 207_000, CacheMeasuredReadTokens: 123_000, CostUSD: 0.525},
		ContextUsed: 45_152, ContextMax: 272_000, Subscription: true,
	}}, width, now)
	if plain := plainResidentIndicatorLines(lines); len(plain) < 3 {
		t.Fatalf("activity lines = %#v, want wrapped metadata", plain)
	}

	got := plainResidentIndicatorLines(fitResidentSubagentActivityLines(tui.Dark, lines, 0, 1, width, func(candidate []string) int {
		return len(candidate)
	}))
	if len(got) != 1 || !strings.Contains(got[0], "reviewer") || strings.Contains(got[0], "more active") || strings.Contains(got[0], "↑84k") {
		t.Fatalf("constrained activity lines = %#v, want only the activity/name row", got)
	}
}

func TestRenderResidentSubagentActivityLinesOmitsMetadataWhenIndentDoesNotFit(t *testing.T) {
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	snapshot := subagents.ResidentSnapshot{
		ID: "running-id", Profile: "reviewer", State: subagents.ResidentRunning,
		Usage:       provider.Usage{InputTokens: 84_000, OutputTokens: 1_500, CacheReadTokens: 123_000, CacheMeasuredPromptTokens: 207_000, CacheMeasuredReadTokens: 123_000, CostUSD: 0.525},
		ContextUsed: 45_152, ContextMax: 272_000, Subscription: true,
	}
	for _, width := range []int{3, 4} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			lines := renderResidentSubagentActivityLines(tui.Dark, "/", []subagents.ResidentSnapshot{snapshot}, width, now)
			plain := plainResidentIndicatorLines(lines)
			wantActivity := "  " + strings.Repeat(".", width-runewidth.StringWidth("  "))
			if len(plain) != 1 || plain[0] != wantActivity {
				t.Fatalf("activity lines = %#v, want only activity row %#v", plain, []string{wantActivity})
			}
			if strings.Contains(strings.Join(plain, "\n"), "↑84k") {
				t.Fatalf("activity lines include metadata despite unavailable indent: %#v", plain)
			}
			if gotWidth := runewidth.StringWidth(plain[0]); gotWidth > width {
				t.Fatalf("activity line width = %d, want <= %d: %q", gotWidth, width, plain[0])
			}

			limited := plainResidentIndicatorLines(limitResidentSubagentActivityLines(tui.Dark, lines, 0, 1, width))
			if len(limited) != 1 || limited[0] != plain[0] || strings.Contains(limited[0], "more active") {
				t.Fatalf("limited activity lines = %#v, want preserved activity row %#v without overflow", limited, plain)
			}
		})
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

func TestLimitResidentSubagentActivityLinesUsesSingleRowForOverflowWhenAgentsAreHidden(t *testing.T) {
	got := plainResidentIndicatorLines(limitResidentSubagentActivityLines(tui.Dark, []string{"one"}, 2, 1, 80))
	want := []string{"  … 3 more active subagents"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("limited lines = %#v, want %#v", got, want)
	}
}

func TestLimitResidentSubagentActivityLinesPrefersActivityRowsBeforeMetadata(t *testing.T) {
	lines := []string{"  agent one", "    usage one", "  agent two", "    usage two"}
	got := plainResidentIndicatorLines(limitResidentSubagentActivityLines(tui.Dark, lines, 0, 3, 80))
	want := []string{"  agent one", "    usage one", "  agent two"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("limited lines = %#v, want %#v", got, want)
	}
}

func TestLimitResidentSubagentActivityLinesCountsOnlyHiddenAgentsWhenMetadataTruncates(t *testing.T) {
	lines := []string{"  agent one", "    usage one", "    usage continued", "  agent two", "    usage two"}
	got := plainResidentIndicatorLines(limitResidentSubagentActivityLines(tui.Dark, lines, 1, 2, 80))
	want := []string{"  agent one", "  … 2 more active subagents"}
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
