package modes

import (
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/tui"
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

func TestLimitResidentSubagentActivityLinesSummarizesOverflow(t *testing.T) {
	got := plainResidentIndicatorLines(limitResidentSubagentActivityLines(tui.Dark, []string{"one", "two", "three"}, 0, 2, 80))
	want := []string{"one", "  … 2 more active subagents"}
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
