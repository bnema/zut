package subagents

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestResidentCompletionProjection(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status string
	}{
		{nil, "completed"},
		{errors.New("failure"), "failed"},
		{context.Canceled, "interrupted"},
		{errors.Join(ErrBudgetExceeded, errors.New("capture failed")), "budget_exhausted"},
		{errors.Join(context.Canceled, ErrBudgetExceeded), "interrupted"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			got := (ResidentCompletion{ChildID: "child", TurnID: "turn", Task: "task", Summary: "saved progress", Err: tc.err}).Completion()
			if got.Status != tc.status || got.AgentID != "child" || got.TurnID != "turn" || got.Task != "task" || got.Summary != "saved progress" {
				t.Fatalf("completion = %#v", got)
			}
			if tc.err != nil && got.Error != tc.err.Error() {
				t.Fatalf("error = %q, want %q", got.Error, tc.err.Error())
			}
			text := FormatCompletionUpdate([]Completion{got}, "")
			label := "partial: "
			if tc.err == nil {
				label = "final: "
			}
			if !strings.Contains(text, label+"saved progress") {
				t.Fatalf("update = %q", text)
			}
		})
	}
}

func TestFormatCompletionUpdateIncludesFinalSummary(t *testing.T) {
	got := FormatCompletionUpdate([]Completion{{AgentID: "child", Status: "completed", Task: "review", Summary: "found the regression"}}, "")
	if !strings.Contains(got, "final: found the regression") {
		t.Fatalf("completion update = %q", got)
	}
}

func TestCompletionTrackerDropsReportsAfterCancellationReset(t *testing.T) {
	tracker := NewCompletionTracker()
	if !tracker.TrackResident("child", "turn-1") {
		t.Fatal("TrackResident returned false")
	}
	tracker.Reset()
	if tracker.Report(Completion{AgentID: "child", TurnID: "turn-1"}) {
		t.Fatal("Report returned true after reset")
	}
	got, err := tracker.WaitIdle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("stale completions = %#v", got)
	}
}

func TestCompletionTrackerMatchesTerminalReportsToAcceptedTurns(t *testing.T) {
	tracker := NewCompletionTracker()
	if !tracker.TrackResident("child", "turn-1") || !tracker.TrackResident("child", "turn-2") {
		t.Fatal("TrackResident returned false")
	}
	if tracker.Report(Completion{AgentID: "child", TurnID: "other"}) {
		t.Fatal("Report accepted an unknown turn")
	}
	if !tracker.Report(Completion{AgentID: "child", TurnID: "turn-1"}) {
		t.Fatal("Report rejected an accepted turn")
	}
	if tracker.Report(Completion{AgentID: "child", TurnID: "turn-1"}) {
		t.Fatal("Report accepted a duplicate terminal result")
	}
	if got := tracker.Pending(); got != 1 {
		t.Fatalf("Pending = %d, want 1", got)
	}
	if !tracker.Report(Completion{AgentID: "child", TurnID: "turn-2"}) {
		t.Fatal("Report rejected second accepted turn")
	}
	got, err := tracker.WaitIdle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("completions = %#v, want 2", got)
	}
}
