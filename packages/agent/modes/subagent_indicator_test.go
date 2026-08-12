package modes

import (
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/tui"
	"github.com/mattn/go-runewidth"
)

func TestRenderSubagentActivityLinesShowsOnlyTraceOpenOperations(t *testing.T) {
	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	snapshots := []subagents.AgentSnapshot{
		{ID: "open", Subagent: "reviewer"},
		{ID: "alive", Subagent: "implementer", Activity: "working"},
	}
	views := map[string]subagents.AgentTraceView{
		"open":  {AgentID: "open", OpenOperations: []subagents.Operation{{Type: "tool.started", StartedAt: now.Add(-72 * time.Second)}}},
		"alive": {AgentID: "alive", LastEvent: subagents.TraceEvent{Type: "agent.ready", Timestamp: now.Add(-time.Second)}},
	}
	got := plainActivityLines(renderSubagentActivityLines(tui.Dark, "/", snapshots, views, 80, now))
	want := []string{"  / reviewer · running tool · 1m12s"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("activity lines = %#v, want %#v", got, want)
	}
}

func TestRenderSubagentActivityLinesShowsSafeLastObservation(t *testing.T) {
	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	lines := plainActivityLines(renderSubagentActivityLines(tui.Dark, "/", []subagents.AgentSnapshot{{ID: "review-123", Subagent: "reviewer"}}, map[string]subagents.AgentTraceView{
		"review-123": {PrimaryOperation: &subagents.Operation{Type: "turn.started", TurnID: "turn-1", StartedAt: now.Add(-time.Minute)}, LastObservation: &subagents.LiveObservation{Type: "assistant.stream.observed", TurnID: "turn-1", At: now.Add(-time.Second)}},
	}, 100, now))
	if got := strings.Join(lines, "\n"); !strings.Contains(got, "processing turn · assistant streaming 1s ago") || strings.Contains(got, "delta") {
		t.Fatalf("activity lines = %#v", lines)
	}
}

func TestRenderSubagentActivityLinesOmitsPriorTurnObservation(t *testing.T) {
	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	lines := plainActivityLines(renderSubagentActivityLines(tui.Dark, "/", []subagents.AgentSnapshot{{ID: "review-123", Subagent: "reviewer"}}, map[string]subagents.AgentTraceView{
		"review-123": {PrimaryOperation: &subagents.Operation{Type: "tool.started", TurnID: "turn-2", StartedAt: now.Add(-time.Minute)}, LastObservation: &subagents.LiveObservation{Type: "assistant.stream.observed", TurnID: "turn-1", At: now.Add(-time.Second)}},
	}, 100, now))
	if got := strings.Join(lines, "\n"); strings.Contains(got, "assistant streaming") || !strings.Contains(got, "running tool") || !strings.Contains(got, "1m") {
		t.Fatalf("activity lines = %#v", lines)
	}
}

func TestRenderSubagentActivityLinesKeepsNameAndAgeOnNarrowTerminals(t *testing.T) {
	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	lines := renderSubagentActivityLines(tui.Dark, "|", []subagents.AgentSnapshot{{ID: "review-123", Subagent: "reviewer"}}, map[string]subagents.AgentTraceView{
		"review-123": {OpenOperations: []subagents.Operation{{Type: "tool.started", StartedAt: now.Add(-72 * time.Second)}}},
	}, 27, now)
	if len(lines) != 1 {
		t.Fatalf("line count = %d, want 1", len(lines))
	}
	line := plainActivityLines(lines)[0]
	if runewidth.StringWidth(line) > 27 || !strings.Contains(line, "reviewer") || !strings.Contains(line, "1m12s") {
		t.Fatalf("narrow line = %q", line)
	}
}

func TestRenderSubagentActivityLinesSanitizesWorkerText(t *testing.T) {
	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	lines := renderSubagentActivityLines(tui.Dark, "/", []subagents.AgentSnapshot{{ID: "review-123", Subagent: "reviewer\x1b]52;c;ignored\a"}}, map[string]subagents.AgentTraceView{
		"review-123": {OpenOperations: []subagents.Operation{{Type: "tool.started", StartedAt: now.Add(-time.Second)}}},
	}, 80, now)
	plain := plainActivityLines(lines)[0]
	if strings.ContainsRune(plain, '\x1b') || !strings.Contains(plain, "reviewer") {
		t.Fatalf("sanitized indicator = %q", plain)
	}
}

func TestTruncateSubagentIndicatorTextPreservesGraphemeClusters(t *testing.T) {
	if got, want := truncateSubagentIndicatorText("a👩‍💻abcdef", 6), "a👩‍💻..."; got != want {
		t.Fatalf("truncated grapheme = %q, want %q", got, want)
	}
}

func TestFormatSubagentActivityAgeUsesOperationStart(t *testing.T) {
	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	if got, want := formatSubagentActivityAge(now.Add(-7*time.Second), now), "7s"; got != want {
		t.Fatalf("age = %q, want %q", got, want)
	}
}

func TestLimitSubagentActivityLinesSummarizesOmittedOperations(t *testing.T) {
	got := plainActivityLines(limitSubagentActivityLines(tui.Dark, []string{"one", "two", "three", "four"}, 3, 80))
	want := []string{"one", "two", "  … 2 more open subagent operations"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("limited lines = %#v, want %#v", got, want)
	}
}

func plainActivityLines(lines []string) []string {
	plain := make([]string, len(lines))
	for idx, line := range lines {
		plain[idx] = stripANSIBytes(line)
	}
	return plain
}
