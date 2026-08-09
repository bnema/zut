package subagents

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func newCompletionTestAgent(id, task string, status Status) *Agent {
	return &Agent{
		ID:            id,
		Task:          task,
		status:        status,
		activity:      "testing",
		done:          make(chan struct{}),
		transcript:    []string{"tail line 1", "tail line 2"},
		lastAssistant: "final response",
	}
}

func TestCompletionTrackerTurnEndCompletesLongLivedWorker(t *testing.T) {
	agent := newCompletionTestAgent("worker-1", "original task", StatusRunning)
	t.Cleanup(func() { close(agent.done) })
	tracker := NewCompletionTracker()
	tracker.TrackTurn(agent, "ignored task", false)

	if agent.OnTurnEnd == nil {
		t.Fatal("TrackTurn did not install an event callback")
	}
	agent.OnTurnEnd(1, "")

	batch, err := tracker.WaitIdle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 1 {
		t.Fatalf("completion count = %d, want 1", len(batch))
	}
	if got := batch[0].Status; got != "completed" {
		t.Fatalf("status = %q, want completed", got)
	}
	if got := batch[0].Task; got != agent.Task {
		t.Fatalf("task = %q, want %q", got, agent.Task)
	}
	if tracker.Pending() != 0 {
		t.Fatalf("pending = %d, want 0", tracker.Pending())
	}
}

func TestCompletionTrackerStartupAndProcessFailuresUseExitPath(t *testing.T) {
	tests := []struct {
		name   string
		status Status
		err    error
		want   string
	}{
		{name: "startup failure", status: StatusFailed, err: errors.New("listener startup failed"), want: "failed"},
		{name: "stopped process", status: StatusKilled, err: context.Canceled, want: "killed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := newCompletionTestAgent("worker-"+tt.name, "task", tt.status)
			agent.lastErr = tt.err
			tracker := NewCompletionTracker()
			tracker.TrackExit(agent, agent.Task)
			close(agent.done)

			batch, err := tracker.WaitIdle(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(batch) != 1 || batch[0].Status != tt.want {
				t.Fatalf("batch = %+v, want one %s completion", batch, tt.want)
			}
			if batch[0].Error != tt.err.Error() {
				t.Fatalf("error = %q, want %q", batch[0].Error, tt.err)
			}
		})
	}
}

func TestCompletionTrackerQueuesSequentialTurnsForOneWorker(t *testing.T) {
	agent := newCompletionTestAgent("worker-1", "original task", StatusRunning)
	tracker := NewCompletionTracker()
	tracker.TrackTurn(agent, agent.Task, false)
	tracker.TrackTurn(agent, "follow-up task", true)
	tracker.TrackTurn(agent, "follow-up task", true)

	if got := tracker.Pending(); got != 3 {
		t.Fatalf("pending after queued turns = %d, want 3", got)
	}
	// Callback goroutines may be scheduled out of order. Completion still has
	// to match the registered turn identity and flush in registration order.
	agent.OnTurnEnd(2, "")
	if got := tracker.Pending(); got != 2 {
		t.Fatalf("pending after second turn completed first = %d, want 2", got)
	}
	// A duplicate lifecycle event for the same prompt-level turn must not
	// consume the next accepted follow-up, even when its prompt is identical.
	agent.OnTurnEnd(2, "")
	if got := tracker.Pending(); got != 2 {
		t.Fatalf("pending after duplicate second turn = %d, want 2", got)
	}
	agent.OnTurnEnd(1, "")
	if got := tracker.Pending(); got != 1 {
		t.Fatalf("pending after first turn = %d, want 1", got)
	}
	agent.OnTurnEnd(3, "")

	batch, err := tracker.WaitIdle(context.Background())
	if err != nil || len(batch) != 3 {
		t.Fatalf("wait = (%+v, %v), want three turn completions", batch, err)
	}
	if batch[0].Task != agent.Task || batch[1].Task != "follow-up task" || batch[2].Task != "follow-up task" {
		t.Fatalf("queued tasks = %q, %q, %q; want original then both follow-ups", batch[0].Task, batch[1].Task, batch[2].Task)
	}
	close(agent.done)
}

func TestCompletionTrackerTurnEndReleasesProcessWaiterWithoutDuplicate(t *testing.T) {
	agent := newCompletionTestAgent("worker-1", "task", StatusRunning)
	agent.lastErr = errors.New("late process failure")
	tracker := NewCompletionTracker()
	tracker.TrackTurn(agent, agent.Task, false)

	tracker.mu.Lock()
	state := tracker.active[agent]
	waitDone := state.waitDone
	tracker.mu.Unlock()
	if waitDone == nil {
		t.Fatal("turn tracking did not start a process fallback waiter")
	}
	agent.OnTurnEnd(1, "")
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("process fallback waiter was not released after turn_end")
	}
	close(agent.done)

	batch, err := tracker.WaitIdle(context.Background())
	if err != nil || len(batch) != 1 {
		t.Fatalf("wait = (%+v, %v), want one completion", batch, err)
	}
	if batch[0].Status != "completed" {
		t.Fatalf("status = %q, want completed", batch[0].Status)
	}
}

func TestCompletionTrackerSingleWinnerDoesNotDuplicate(t *testing.T) {
	agent := newCompletionTestAgent("worker-1", "task", StatusRunning)
	tracker := NewCompletionTracker()
	tracker.TrackTurn(agent, agent.Task, false)

	agent.OnTurnEnd(1, "")
	close(agent.done)

	batch, err := tracker.WaitIdle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 1 {
		t.Fatalf("completion count = %d, want exactly 1", len(batch))
	}
	if second, err := tracker.WaitIdle(context.Background()); err != nil || len(second) != 0 {
		t.Fatalf("second wait = (%+v, %v), want no duplicate", second, err)
	}
}

func TestCompletionTrackerBatchesUntilAllActiveEntriesComplete(t *testing.T) {
	first := newCompletionTestAgent("worker-1", "first", StatusRunning)
	second := newCompletionTestAgent("worker-2", "second", StatusRunning)
	tracker := NewCompletionTracker()
	tracker.TrackTurn(first, first.Task, false)
	tracker.TrackTurn(second, second.Task, false)

	waited := make(chan []Completion, 1)
	go func() {
		batch, _ := tracker.WaitIdle(context.Background())
		waited <- batch
	}()
	// Complete the workers in reverse order. The batch must still follow the
	// registration sequence rather than callback arrival order.
	second.OnTurnEnd(1, "")
	if got := tracker.Pending(); got != 1 {
		t.Fatalf("pending after second completion = %d, want 1", got)
	}
	first.OnTurnEnd(1, "")

	batch := <-waited
	if len(batch) != 2 {
		t.Fatalf("batch count = %d, want 2", len(batch))
	}
	if batch[0].AgentID != first.ID || batch[1].AgentID != second.ID {
		t.Fatalf("batch order = %q, %q; want %q, %q", batch[0].AgentID, batch[1].AgentID, first.ID, second.ID)
	}
	close(first.done)
	close(second.done)
}

func TestCompletionTrackerReplaysTurnEndReceivedBeforeTracking(t *testing.T) {
	agent := newCompletionTestAgent("worker-1", "task", StatusRunning)
	notifyPromptTurnEnd(agent, NewEvent("turn_end", map[string]any{"step": float64(1)}))
	tracker := NewCompletionTracker()
	tracker.TrackTurn(agent, agent.Task, false)

	batch, err := tracker.WaitIdle(context.Background())
	if err != nil || len(batch) != 1 {
		t.Fatalf("wait = (%+v, %v), want replayed completion", batch, err)
	}
	if batch[0].Status != "completed" {
		t.Fatalf("replayed status = %q, want completed", batch[0].Status)
	}
	close(agent.done)
}

func TestCompletionTrackerFutureTurnIgnoresPriorPendingTurnEnd(t *testing.T) {
	agent := newCompletionTestAgent("worker-1", "original task", StatusRunning)
	agent.setTurnCounts(1, 1)
	agent.setTurnState(TurnIdle, "turn-1")
	notifyPromptTurnEnd(agent, NewEvent("turn_end", map[string]any{"step": float64(1)}))

	tracker := NewCompletionTracker()
	tracker.TrackFutureTurn(agent, "follow-up task", true)
	if got := tracker.Pending(); got != 1 {
		t.Fatalf("pending after stale turn_end replay = %d, want 1", got)
	}
	agent.OnTurnEnd(2, "")

	batch, err := tracker.WaitIdle(context.Background())
	if err != nil || len(batch) != 1 || batch[0].Task != "follow-up task" {
		t.Fatalf("future turn completion = (%+v, %v), want follow-up turn", batch, err)
	}
	close(agent.done)
}

func TestCompletionTrackerCurrentTurnReplaysFastCompletion(t *testing.T) {
	agent := newCompletionTestAgent("worker-1", "original task", StatusRunning)
	agent.setTurnCounts(2, 1)
	agent.setTurnState(TurnSucceeded, "turn-2")
	notifyPromptTurnEnd(agent, NewEvent("turn_end", map[string]any{"step": float64(2)}))

	tracker := NewCompletionTracker()
	tracker.TrackTurn(agent, "follow-up task", true)
	batch, err := tracker.WaitIdle(context.Background())
	if err != nil || len(batch) != 1 || batch[0].Task != "follow-up task" {
		t.Fatalf("current turn replay = (%+v, %v), want follow-up turn", batch, err)
	}
	close(agent.done)
}

func TestCompletionTrackerPostSuccessRegistrationAcceptsNextStartedTurn(t *testing.T) {
	agent := newCompletionTestAgent("worker-1", "original task", StatusRunning)
	agent.setTurnCounts(1, 1)
	agent.setTurnState(TurnQueued, "turn-1")

	tracker := NewCompletionTracker()
	tracker.TrackTurn(agent, "follow-up task", true)
	agent.OnTurnEnd(2, "")
	batch, err := tracker.WaitIdle(context.Background())
	if err != nil || len(batch) != 1 || batch[0].Task != "follow-up task" {
		t.Fatalf("post-success next turn = (%+v, %v), want follow-up turn", batch, err)
	}
	close(agent.done)
}

func TestCompletionTrackerKeepsCompletionsBeforeWaiting(t *testing.T) {
	agent := newCompletionTestAgent("worker-1", "task", StatusRunning)
	tracker := NewCompletionTracker()
	tracker.TrackTurn(agent, agent.Task, false)
	agent.OnTurnEnd(1, "")

	batch, err := tracker.WaitIdle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 1 || batch[0].AgentID != agent.ID {
		t.Fatalf("batch = %+v, want buffered worker completion", batch)
	}
	close(agent.done)
}

func TestCompletionTrackerWaitIdleCancellationLeavesActiveWork(t *testing.T) {
	agent := newCompletionTestAgent("worker-1", "task", StatusRunning)
	tracker := NewCompletionTracker()
	tracker.TrackTurn(agent, agent.Task, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	batch, err := tracker.WaitIdle(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if batch != nil {
		t.Fatalf("cancelled batch = %+v, want nil", batch)
	}
	if tracker.Pending() != 1 {
		t.Fatalf("pending after cancellation = %d, want 1", tracker.Pending())
	}

	agent.OnTurnEnd(1, "")
	close(agent.done)
	batch, err = tracker.WaitIdle(context.Background())
	if err != nil || len(batch) != 1 {
		t.Fatalf("post-cancellation wait = (%+v, %v), want one completion", batch, err)
	}
}

func TestCompletionTrackerStoresRawFieldsAndResumedTask(t *testing.T) {
	longTask := strings.Repeat("task ", 80)
	longError := errors.New(strings.Repeat("error ", 80))
	agent := newCompletionTestAgent("worker-1", "original task", StatusRunning)
	agent.Task = longTask
	agent.lastErr = longError
	agent.lastAssistant = ""
	tracker := NewCompletionTracker()
	const followUp = "follow-up task"
	tracker.TrackTurn(agent, followUp, true)
	agent.OnTurnEnd(1, "")

	batch, err := tracker.WaitIdle(context.Background())
	if err != nil || len(batch) != 1 {
		t.Fatalf("wait = (%+v, %v), want one completion", batch, err)
	}
	completion := batch[0]
	if completion.Task != followUp {
		t.Fatalf("resumed task = %q, want %q", completion.Task, followUp)
	}
	if completion.Error != longError.Error() {
		t.Fatalf("stored error was truncated: got %d chars, want %d", len(completion.Error), len(longError.Error()))
	}
	if completion.Tail == "" {
		t.Fatal("stored tail is empty")
	}
	close(agent.done)
}

func TestFormatCompletionUpdateBoundsFieldsAndPreservesEvidence(t *testing.T) {
	longTask := strings.Repeat("t", 300)
	longError := strings.Repeat("e", 300)
	longTail := strings.Repeat("x", 700)
	instruction := "Summarise the collective outcome without spawning new workers."
	update := FormatCompletionUpdate([]Completion{{
		AgentID: "worker-1", Status: "failed", Task: longTask, Error: longError, Tail: longTail,
	}}, instruction)

	if !strings.HasPrefix(update, "[auto-subagents update] 1 sub-agent(s) finished:\n\n") {
		t.Fatalf("update has unexpected header: %q", update)
	}
	if !strings.Contains(update, "1. agent worker-1 - status: failed\n   task: ") {
		t.Fatalf("update omitted stable labels: %q", update)
	}
	if !strings.Contains(update, "   error: ") || !strings.Contains(update, "   tail: ") {
		t.Fatalf("update omitted error or tail: %q", update)
	}
	if !strings.HasSuffix(update, instruction) {
		t.Fatalf("update omitted instruction: %q", update)
	}
	if strings.Contains(strings.ToLower(update), "poll") {
		t.Fatalf("update instructs the parent to poll: %q", update)
	}

	taskLine := ""
	errorLine := ""
	for _, line := range strings.Split(update, "\n") {
		if strings.HasPrefix(line, "   task: ") {
			taskLine = strings.TrimPrefix(line, "   task: ")
		}
		if strings.HasPrefix(line, "   error: ") {
			errorLine = strings.TrimPrefix(line, "   error: ")
		}
	}
	if got := len([]rune(taskLine)); got != 240 {
		t.Fatalf("formatted task length = %d, want 240", got)
	}
	if got := len([]rune(errorLine)); got != 240 {
		t.Fatalf("formatted error length = %d, want 240", got)
	}
	tailLine := ""
	for _, line := range strings.Split(update, "\n") {
		if strings.HasPrefix(line, "   tail: ") {
			tailLine = strings.TrimPrefix(line, "   tail: ")
		}
	}
	if got := len([]rune(tailLine)); got != 600 {
		t.Fatalf("formatted tail length = %d, want 600", got)
	}
}

func TestFormatCompletionUpdateUsesFinalResponseBeforeTailFallback(t *testing.T) {
	final := "complete final response\nwith multiple lines"
	withFinal := FormatCompletionUpdate([]Completion{{
		AgentID: "worker-1", Status: "completed", FinalResponse: final, Tail: "old tail",
	}}, "instruction")
	if !strings.Contains(withFinal, "   final response:\n"+final+"\n") {
		t.Fatalf("final response missing: %q", withFinal)
	}
	if strings.Contains(withFinal, "tail:") {
		t.Fatalf("tail should not be used when final response is present: %q", withFinal)
	}

	withTail := FormatCompletionUpdate([]Completion{{
		AgentID: "worker-2", Status: "completed", Tail: "  fallback tail  ",
	}}, "instruction")
	if !strings.Contains(withTail, "   tail: fallback tail\n") {
		t.Fatalf("tail fallback missing or not trimmed: %q", withTail)
	}
}
