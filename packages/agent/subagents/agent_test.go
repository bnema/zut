package subagents

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestAgentAttemptValueAndTranscriptAccounting(t *testing.T) {
	a := &Agent{maxOutputBytes: 6, maxOutputLines: 10}
	if got := a.AttemptValue(); got != 0 {
		t.Fatalf("initial attempt = %d, want 0", got)
	}
	if got := a.incrementAttempt(); got != 1 || a.AttemptValue() != 1 {
		t.Fatalf("attempt after increment = %d, value = %d; want 1", got, a.AttemptValue())
	}

	a.appendTranscript("a\nbb")
	if got := a.outputBytes; got != 5 {
		t.Fatalf("output bytes after append = %d, want 5", got)
	}
	if got := a.outputLines; got != 2 {
		t.Fatalf("output lines after append = %d, want 2", got)
	}
	a.appendTranscript("ccc")
	if got, want := a.Transcript(), []string{"ccc"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("transcript after byte eviction = %q, want %q", got, want)
	}
	if a.outputBytes != 4 || a.outputLines != 1 {
		t.Fatalf("accounting after byte eviction = (%d bytes, %d lines), want (4, 1)", a.outputBytes, a.outputLines)
	}

	a.maxOutputBytes = 1
	a.appendTranscript("z")
	if got := a.Transcript(); len(got) != 0 {
		t.Fatalf("transcript after evicting every line = %q, want empty", got)
	}
	if a.outputBytes != 0 || a.outputLines != 0 {
		t.Fatalf("empty accounting = (%d bytes, %d lines), want (0, 0)", a.outputBytes, a.outputLines)
	}
}

func TestAgentTranscriptLineCapAccounting(t *testing.T) {
	a := &Agent{maxOutputBytes: 100, maxOutputLines: 2}
	a.appendTranscript("one\ntwo\nthree")

	if got, want := a.Transcript(), []string{"two", "three"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
	if a.outputBytes != len("two")+1+len("three")+1 || a.outputLines != 2 {
		t.Fatalf("line-cap accounting = (%d bytes, %d lines), want (10, 2)", a.outputBytes, a.outputLines)
	}
}

func TestAgentSnapshotConcurrentTranscriptTruncation(t *testing.T) {
	a := &Agent{maxOutputBytes: 8, maxOutputLines: 1}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for range 10_000 {
			a.appendTranscript("overflow")
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for range 10_000 {
			_ = a.Snapshot()
		}
	}()
	close(start)
	wg.Wait()

	if !a.Snapshot().OutputTruncated {
		t.Fatal("snapshot did not report transcript truncation")
	}
}

func TestWaitTurnResultBroadcastsToMultipleObservers(t *testing.T) {
	a := &Agent{ID: "worker-1", done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	results := make(chan *TurnResult, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			result, err := a.WaitTurnResult(ctx, "turn-1")
			results <- result
			errs <- err
		}()
	}
	a.setResult(&TurnResult{Version: ProtocolVersion, AgentID: a.ID, TurnID: "turn-1", Status: ResultSucceeded})

	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("wait error = %v", err)
		}
		if result := <-results; result == nil || result.TurnID != "turn-1" {
			t.Fatalf("wait result = %#v, want turn-1", result)
		}
	}
}

func TestWaitTargetTurnIDAdvancesQueuedResume(t *testing.T) {
	a := &Agent{LifetimeTurns: 1, turnState: TurnQueued, currentTurnID: "turn-1"}
	if got := a.WaitTargetTurnID(); got != "turn-2" {
		t.Fatalf("wait target = %q, want turn-2", got)
	}
}
