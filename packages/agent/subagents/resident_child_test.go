package subagents

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
)

func TestJournaledResidentChildReceivesLiveJournalEvents(t *testing.T) {
	journal, err := OpenResidentJournal(t.TempDir(), "live-child")
	if err != nil {
		t.Fatal(err)
	}
	child := newJournaledResidentChild(ResidentChildSpec{}, journal, func(context.Context, string) error { return nil }, nil)
	defer child.Close(context.Background())
	oldActivity := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	child.mu.Lock()
	child.activityUpdatedAt = oldActivity
	child.mu.Unlock()
	child.live.Start("turn-1")
	if err := journal.RecordAgentEvent(core.EvTextDelta{Delta: "streaming"}); err != nil {
		t.Fatal(err)
	}
	if got := child.Live(); got.AssistantText != "streaming" || got.Revision == 0 {
		t.Fatalf("live child = %#v", got)
	}
	if got := child.ActivityUpdatedAt(); !got.After(oldActivity) {
		t.Fatalf("activity time = %s, want after %s", got, oldActivity)
	}
}

func TestResidentChildRunsAcceptedPromptsFIFOWithOneActiveTurn(t *testing.T) {
	started := make(chan string, 2)
	releaseFirst := make(chan struct{})
	completed := make(chan string, 2)
	child := NewResidentChild(func(ctx context.Context, prompt string) error {
		started <- prompt
		if prompt == "first" {
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		completed <- prompt
		return nil
	})
	defer child.Close(context.Background())

	if err := child.Resume(context.Background(), "first"); err != nil {
		t.Fatalf("first Resume: %v", err)
	}
	if err := child.Resume(context.Background(), "second"); err != nil {
		t.Fatalf("second Resume: %v", err)
	}
	select {
	case got := <-started:
		if got != "first" {
			t.Fatalf("first start = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("first turn did not start")
	}
	select {
	case got := <-started:
		t.Fatalf("second turn started before first completed: %q", got)
	default:
	}
	close(releaseFirst)
	for _, want := range []string{"first", "second"} {
		select {
		case got := <-completed:
			if got != want {
				t.Fatalf("completion = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s turn did not complete", want)
		}
	}
}

func TestResidentChildReportsAndPreservesFailureWhenTurnStartCannotPersist(t *testing.T) {
	journal, err := OpenResidentJournal(t.TempDir(), "start-persistence-failure")
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	completed := make(chan ResidentCompletion, 1)
	run := make(chan struct{}, 1)
	child := newJournaledResidentChild(
		ResidentChildSpec{ID: "start-persistence-failure"},
		journal,
		func(context.Context, string) error {
			run <- struct{}{}
			return nil
		},
		func(completion ResidentCompletion) { completed <- completion },
	)

	request := residentPrompt{turnID: "turn", prompt: "task", ack: make(chan error, 1)}
	select {
	case child.inbox <- request:
	case <-time.After(time.Second):
		t.Fatal("child did not accept test prompt")
	}
	select {
	case completion := <-completed:
		if completion.ChildID != "start-persistence-failure" || completion.TurnID != "turn" || completion.Task != "task" {
			t.Fatalf("completion = %#v", completion)
		}
		if completion.Err == nil || !strings.Contains(completion.Err.Error(), "persist resident child start state") {
			t.Fatalf("completion error = %v", completion.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("failed turn did not report completion")
	}
	select {
	case <-child.done:
	case <-time.After(time.Second):
		t.Fatal("child did not stop after start persistence failure")
	}
	if state := child.State(); state != ResidentFailed {
		t.Fatalf("state = %q, want %q", state, ResidentFailed)
	}
	select {
	case <-run:
		t.Fatal("runner started after turn-start persistence failure")
	default:
	}
}
