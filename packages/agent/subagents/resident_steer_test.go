package subagents

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// steerableRuntime is a ResidentRuntime whose turn blocks until released or
// canceled, recording steered follow-ups it would deliver at the next boundary.
type steerableRuntime struct {
	started chan string
	release chan struct{}

	mu        sync.Mutex
	running   bool
	pending   []string
	delivered []string
}

func newSteerableRuntime() *steerableRuntime {
	return &steerableRuntime{started: make(chan string, 4), release: make(chan struct{})}
}

func (r *steerableRuntime) Run(ctx context.Context, prompt string) error {
	r.mu.Lock()
	r.running = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()
	r.started <- prompt
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.release:
	}
	r.mu.Lock()
	r.delivered = append(r.delivered, r.pending...)
	r.pending = nil
	r.mu.Unlock()
	return nil
}

func (r *steerableRuntime) Steer(prompt string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return false
	}
	r.pending = append(r.pending, prompt)
	return true
}

func (r *steerableRuntime) PendingSteers() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

func (r *steerableRuntime) DrainSteers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.pending
	r.pending = nil
	return out
}

func (r *steerableRuntime) awaitStart(t *testing.T) string {
	t.Helper()
	select {
	case prompt := <-r.started:
		return prompt
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not start")
		return ""
	}
}

func newSteerTestManager(t *testing.T, runtime ResidentRuntime) (*ResidentManager, chan ResidentCompletion) {
	t.Helper()
	manager := NewResidentManager(t.TempDir(), func(ResidentChildSpec, *ResidentJournal) (ResidentRuntime, error) { return runtime, nil })
	completions := make(chan ResidentCompletion, 8)
	manager.SetCompletionObserver(func(c ResidentCompletion) { completions <- c })
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	return manager, completions
}

func drainCompletions(t *testing.T, completions chan ResidentCompletion, want int) []ResidentCompletion {
	t.Helper()
	var got []ResidentCompletion
	for len(got) < want {
		select {
		case c := <-completions:
			got = append(got, c)
		case <-time.After(5 * time.Second):
			t.Fatalf("got %d completions, want %d: %#v", len(got), want, got)
		}
	}
	select {
	case extra := <-completions:
		t.Fatalf("unexpected extra completion %#v", extra)
	case <-time.After(50 * time.Millisecond):
	}
	return got
}

// Issue 226: stopping a child must not echo an undelivered queued follow-up
// back to the parent as if it were an interrupted task.
func TestResidentStopReportsQueuedFollowUpAsDroppedNotInterrupted(t *testing.T) {
	runtime := ResidentTurnRunner(func(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() })
	started := make(chan struct{}, 1)
	manager, completions := newSteerTestManager(t, ResidentTurnRunner(func(ctx context.Context, prompt string) error {
		started <- struct{}{}
		return runtime(ctx, prompt)
	}))
	spec := ResidentChildSpec{ID: "stop-echo", InitialTurnID: "initial", SessionID: "session", Provider: "openai", Model: "test"}
	if _, err := manager.Spawn(t.Context(), spec, "investigate"); err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := manager.ResumeFollowUp(t.Context(), spec.ID, "stop and report now", ResumeQueue, "follow-up", nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	got := drainCompletions(t, completions, 2)
	byTurn := map[string]Completion{}
	for _, c := range got {
		byTurn[c.TurnID] = c.Completion()
	}
	if byTurn["initial"].Status != string(ResidentInterrupted) {
		t.Fatalf("initial completion = %#v", byTurn["initial"])
	}
	if byTurn["follow-up"].Status != CompletionDropped {
		t.Fatalf("follow-up completion = %#v, want dropped", byTurn["follow-up"])
	}
	update := FormatCompletionUpdate([]Completion{byTurn["initial"], byTurn["follow-up"]}, "")
	if strings.Contains(update, "interrupted — stop and report now") || !strings.Contains(update, "dropped (queued follow-up never started: stop and report now)") {
		t.Fatalf("update = %q", update)
	}
}

// A steered follow-up joins the running turn: no new turn, no extra
// completion, and the running runtime receives it.
func TestResidentResumeSteersRunningTurn(t *testing.T) {
	runtime := newSteerableRuntime()
	manager, completions := newSteerTestManager(t, runtime)
	spec := ResidentChildSpec{ID: "steered", InitialTurnID: "initial", SessionID: "session", Provider: "openai", Model: "test"}
	if _, err := manager.Spawn(t.Context(), spec, "investigate"); err != nil {
		t.Fatal(err)
	}
	runtime.awaitStart(t)
	var watched string
	outcome, err := manager.ResumeFollowUp(t.Context(), spec.ID, "wrap up now", ResumeSteer, "unused", func(turnID string) { watched = turnID })
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Steered || outcome.TurnID != "initial" || watched != "initial" {
		t.Fatalf("outcome = %#v, watched = %q", outcome, watched)
	}
	if snapshot, _ := manager.SnapshotFor(spec.ID); snapshot.PendingFollowUps != 1 {
		t.Fatalf("pending follow-ups = %d, want 1", snapshot.PendingFollowUps)
	}
	close(runtime.release)
	got := drainCompletions(t, completions, 1)
	if got[0].TurnID != "initial" || got[0].Err != nil || len(got[0].Undelivered) != 0 {
		t.Fatalf("completion = %#v", got[0])
	}
	if len(runtime.delivered) != 1 || runtime.delivered[0] != "wrap up now" {
		t.Fatalf("delivered = %#v", runtime.delivered)
	}
}

// An idle child cannot be steered, so steer mode falls back to a new turn.
func TestResidentResumeSteerFallsBackToNewTurnWhenIdle(t *testing.T) {
	runtime := newSteerableRuntime()
	close(runtime.release)
	manager, completions := newSteerTestManager(t, runtime)
	spec := ResidentChildSpec{ID: "idle", InitialTurnID: "initial", SessionID: "session", Provider: "openai", Model: "test"}
	if _, err := manager.Spawn(t.Context(), spec, "task"); err != nil {
		t.Fatal(err)
	}
	drainCompletions(t, completions, 1)
	outcome, err := manager.ResumeFollowUp(t.Context(), spec.ID, "next", ResumeSteer, "second", nil)
	if err != nil || outcome.Steered || outcome.TurnID != "second" {
		t.Fatalf("outcome = %#v, err = %v", outcome, err)
	}
	if got := drainCompletions(t, completions, 1); got[0].TurnID != "second" {
		t.Fatalf("completion = %#v", got[0])
	}
}

// Stopping a child with a steered follow-up it never read reports that
// follow-up as undelivered on the interrupted turn.
func TestResidentStopReportsUndeliveredSteer(t *testing.T) {
	runtime := newSteerableRuntime()
	manager, completions := newSteerTestManager(t, runtime)
	spec := ResidentChildSpec{ID: "undelivered", InitialTurnID: "initial", SessionID: "session", Provider: "openai", Model: "test"}
	if _, err := manager.Spawn(t.Context(), spec, "investigate"); err != nil {
		t.Fatal(err)
	}
	runtime.awaitStart(t)
	if _, err := manager.ResumeFollowUp(t.Context(), spec.ID, "stop and report now", ResumeSteer, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	got := drainCompletions(t, completions, 1)
	completion := got[0].Completion()
	if completion.Status != string(ResidentInterrupted) || len(completion.Undelivered) != 1 || completion.Undelivered[0] != "stop and report now" {
		t.Fatalf("completion = %#v", completion)
	}
	if update := FormatCompletionUpdate([]Completion{completion}, ""); !strings.Contains(update, "undelivered: 1 follow-up(s) the child never read:\n    - stop and report now") {
		t.Fatalf("update = %q", update)
	}
}

// Interrupt cancels only the running turn; the child stays live and a later
// resume runs as a new turn with the same runtime.
func TestResidentInterruptKeepsChildResumable(t *testing.T) {
	runtime := newSteerableRuntime()
	manager, completions := newSteerTestManager(t, runtime)
	spec := ResidentChildSpec{ID: "interrupted", InitialTurnID: "initial", SessionID: "session", Provider: "openai", Model: "test", Required: true}
	if _, err := manager.Spawn(t.Context(), spec, "investigate"); err != nil {
		t.Fatal(err)
	}
	runtime.awaitStart(t)
	if _, err := manager.ResumeFollowUp(t.Context(), spec.ID, "queued work", ResumeQueue, "queued", nil); err != nil {
		t.Fatal(err)
	}
	interrupted, err := manager.Interrupt(t.Context(), spec.ID)
	if err != nil || !interrupted {
		t.Fatalf("Interrupt = %t, %v", interrupted, err)
	}
	got := drainCompletions(t, completions, 2)
	byTurn := map[string]Completion{}
	for _, c := range got {
		byTurn[c.TurnID] = c.Completion()
	}
	if byTurn["initial"].Status != string(ResidentInterrupted) || byTurn["queued"].Status != CompletionDropped {
		t.Fatalf("completions = %#v", byTurn)
	}
	if manager.Get(spec.ID) == nil {
		t.Fatal("interrupt removed the live child")
	}
	if again, err := manager.Interrupt(t.Context(), spec.ID); err != nil || again {
		t.Fatalf("second Interrupt = %t, %v, want no running turn", again, err)
	}
	close(runtime.release)
	outcome, err := manager.ResumeFollowUp(t.Context(), spec.ID, "wrap up with what you have", ResumeSteer, "wrap-up", nil)
	if err != nil || outcome.Steered {
		t.Fatalf("resume after interrupt = %#v, %v", outcome, err)
	}
	if prompt := runtime.awaitStart(t); prompt != "wrap up with what you have" {
		t.Fatalf("resumed prompt = %q", prompt)
	}
	if got := drainCompletions(t, completions, 1); got[0].TurnID != "wrap-up" || got[0].Err != nil {
		t.Fatalf("wrap-up completion = %#v", got[0])
	}
	if len(manager.UnmetRequired()) != 0 {
		t.Fatal("successful wrap-up left required work unmet")
	}
}

// An interrupt can be accepted just before the turn finishes on its own. The
// turn must still report interrupted exactly once, and a late interrupt on the
// now-idle child must be refused.
func TestResidentInterruptRacingNaturalCompletion(t *testing.T) {
	started, release := make(chan struct{}, 1), make(chan struct{})
	// This runtime ignores cancellation, so its turn ends successfully after
	// the interrupt was already accepted.
	runtime := ResidentTurnRunner(func(context.Context, string) error {
		started <- struct{}{}
		<-release
		return nil
	})
	manager, completions := newSteerTestManager(t, runtime)
	spec := ResidentChildSpec{ID: "racing", InitialTurnID: "initial", SessionID: "session", Provider: "openai", Model: "test"}
	if _, err := manager.Spawn(t.Context(), spec, "work"); err != nil {
		t.Fatal(err)
	}
	<-started
	if ok, err := manager.Interrupt(t.Context(), spec.ID); err != nil || !ok {
		t.Fatalf("Interrupt = %t, %v", ok, err)
	}
	close(release)
	got := drainCompletions(t, completions, 1)
	if got[0].TurnID != "initial" || got[0].Completion().Status != string(ResidentInterrupted) {
		t.Fatalf("completion = %#v", got[0])
	}
	if ok, err := manager.Interrupt(t.Context(), spec.ID); err != nil || ok {
		t.Fatalf("late Interrupt = %t, %v, want refused on idle child", ok, err)
	}
	if manager.Get(spec.ID) == nil {
		t.Fatal("interrupt removed the live child")
	}
}

// Steering must not jump ahead of a queued follow-up.
func TestResidentSteerQueuesBehindPendingTurns(t *testing.T) {
	runtime := newSteerableRuntime()
	manager, _ := newSteerTestManager(t, runtime)
	spec := ResidentChildSpec{ID: "ordered", InitialTurnID: "initial", SessionID: "session", Provider: "openai", Model: "test"}
	if _, err := manager.Spawn(t.Context(), spec, "investigate"); err != nil {
		t.Fatal(err)
	}
	runtime.awaitStart(t)
	if _, err := manager.ResumeFollowUp(t.Context(), spec.ID, "first", ResumeQueue, "first", nil); err != nil {
		t.Fatal(err)
	}
	outcome, err := manager.ResumeFollowUp(t.Context(), spec.ID, "second", ResumeSteer, "second", nil)
	if err != nil || outcome.Steered || outcome.TurnID != "second" {
		t.Fatalf("outcome = %#v, err = %v", outcome, err)
	}
	if snapshot, _ := manager.SnapshotFor(spec.ID); snapshot.PendingFollowUps != 2 {
		t.Fatalf("pending follow-ups = %d, want 2 queued turns", snapshot.PendingFollowUps)
	}
}
