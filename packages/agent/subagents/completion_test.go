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
		{errors.New("capture failed"), "failed"},
		{errors.Join(context.Canceled, errors.New("capture failed")), "interrupted"},
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

func TestFormatCompletionUpdateIdentifiesActiveSiblings(t *testing.T) {
	got := FormatCompletionUpdateWithActive(
		[]Completion{{AgentID: "finished", Status: "completed"}},
		[]string{"worker-a", "worker-b"},
		"continue",
	)
	for _, want := range []string{"finished: completed", "Still running: worker-a worker-b", "continue"} {
		if !strings.Contains(got, want) {
			t.Fatalf("completion update %q missing %q", got, want)
		}
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

func TestCompletionTrackerWaitReadyDoesNotWaitForSiblings(t *testing.T) {
	tracker := NewCompletionTracker()
	tracker.TrackResident("a", "turn-a")
	tracker.TrackResident("b", "turn-b")
	want := Completion{AgentID: "a", TurnID: "turn-a", Summary: "result"}
	tracker.Report(want)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := tracker.WaitReady(ctx)
	if err != nil || len(got) != 1 || got[0] != want {
		t.Fatalf("WaitReady = %#v, %v, want ready result despite pending sibling", got, err)
	}
	if _, err := tracker.WaitReady(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("second WaitReady error = %v, want cancellation with no duplicate result", err)
	}
	tracker.Report(Completion{AgentID: "b", TurnID: "turn-b"})
	got, err = tracker.WaitIdle(context.Background())
	if err != nil || len(got) != 1 || got[0].AgentID != "b" {
		t.Fatalf("WaitIdle = %#v, %v, want only remaining sibling", got, err)
	}
	got, err = tracker.WaitReady(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("idle WaitReady = %#v, %v", got, err)
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
