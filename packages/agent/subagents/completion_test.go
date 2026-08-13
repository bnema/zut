package subagents

import (
	"context"
	"strings"
	"testing"
)

func TestFormatCompletionUpdateIncludesFinalSummary(t *testing.T) {
	got := FormatCompletionUpdate([]Completion{{AgentID: "child", Status: "completed", Task: "review", Summary: "found the regression"}}, "")
	if !strings.Contains(got, "final: found the regression") {
		t.Fatalf("completion update = %q", got)
	}
}

func TestCompletionTrackerDropsReportsAfterCancellationReset(t *testing.T) {
	tracker := NewCompletionTracker()
	tracker.TrackResident()
	tracker.Reset()
	tracker.Report(Completion{AgentID: "stale"})
	got, err := tracker.WaitIdle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("stale completions = %#v", got)
	}
}
