package subagents

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Completion is the observed outcome of one delegated turn. The values are
// kept unbounded so callers can decide how much evidence to retain; the
// shared formatter applies the bounds used in parent-agent updates.
type Completion struct {
	AgentID       string
	Status        string
	Task          string
	Error         string
	FinalResponse string
	Tail          string

	// turnError preserves the label used by the interactive update for an
	// error reported by a prompt-level turn_end rather than by the worker
	// process itself.
	turnError    bool
	cancellation bool

	registrationSeq uint64
}

type completionEntry struct {
	state           *completionAgent
	task            string
	followUp        bool
	expectedStep    int
	strictStep      bool
	registrationSeq uint64
	done            bool
	completion      *Completion
}

type completionAgent struct {
	agent             *Agent
	entries           []*completionEntry
	seenTurnEnds      map[int]struct{}
	turnMu            sync.Mutex
	callbackInstalled bool
	waitCancel        context.CancelFunc
	waitingForProcess bool
	waitDone          chan struct{}
}

// CompletionTracker observes delegated turns through Agent's event and
// lifecycle callbacks. Completed values are buffered so a worker can finish
// before the parent begins waiting for the batch.
type CompletionTracker struct {
	mu               sync.Mutex
	active           map[*Agent]*completionAgent
	ready            []Completion
	nextRegistration uint64
	changed          chan struct{}
}

// NewCompletionTracker creates an empty completion tracker.
func NewCompletionTracker() *CompletionTracker {
	return &CompletionTracker{
		active:  make(map[*Agent]*completionAgent),
		changed: make(chan struct{}),
	}
}

func (t *CompletionTracker) initLocked() {
	if t.active == nil {
		t.active = make(map[*Agent]*completionAgent)
	}
	if t.changed == nil {
		t.changed = make(chan struct{})
	}
}

// TrackTurn tracks the worker's current prompt-level turn, or its first turn
// when no turn has started yet. Multiple turns for the same long-lived worker
// are retained and dispatched in registration order.
func (t *CompletionTracker) TrackTurn(a *Agent, task string, followUp bool) func() {
	return t.trackTurn(a, task, followUp, false)
}

// TrackFutureTurn tracks a prompt-level turn before its prompt is delivered.
// The returned function removes the registration without producing a
// completion; callers use it when prompt delivery is rejected.
func (t *CompletionTracker) TrackFutureTurn(a *Agent, task string, followUp bool) func() {
	return t.trackTurn(a, task, followUp, true)
}

func (t *CompletionTracker) trackTurn(a *Agent, task string, followUp, future bool) func() {
	if t == nil || a == nil {
		return nil
	}

	state, entry, installCallback := t.addTurn(a, task, followUp, future)
	if state == nil {
		return nil
	}
	if installCallback {
		// SetOnTurnEnd is deliberately installed before the process waiter and
		// before the caller sends a follow-up. It also replays notices that
		// arrived while no callback was registered.
		a.SetOnTurnEnd(func(step int, errMsg string) {
			state.turnMu.Lock()
			defer state.turnMu.Unlock()
			t.completeNextTurn(state, step, errMsg)
		})
	}
	t.startProcessWaiter(state)
	return func() { t.cancelEntry(entry) }
}

// TrackExit tracks only process termination. It is used for explicit worker
// stops when no prompt-level turn watcher owns the worker.
func (t *CompletionTracker) TrackExit(a *Agent, task string) {
	if t == nil || a == nil {
		return
	}
	state, entry := t.addExit(a, task)
	if state == nil || entry == nil {
		return
	}
	t.startProcessWaiter(state)
}

func (t *CompletionTracker) addTurn(a *Agent, task string, followUp, future bool) (*completionAgent, *completionEntry, bool) {
	entry := &completionEntry{task: task, followUp: followUp, expectedStep: expectedTurnStep(a, future), strictStep: future}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.initLocked()
	state := t.active[a]
	if state == nil {
		state = &completionAgent{agent: a, seenTurnEnds: make(map[int]struct{})}
		t.active[a] = state
	}
	if state.seenTurnEnds == nil {
		state.seenTurnEnds = make(map[int]struct{})
	}
	if count := len(state.entries); count != 0 {
		previousStep := state.entries[count-1].expectedStep
		if entry.expectedStep <= previousStep {
			entry.expectedStep = previousStep + 1
		}
	}
	t.nextRegistration++
	entry.registrationSeq = t.nextRegistration
	entry.state = state
	state.entries = append(state.entries, entry)
	installCallback := !state.callbackInstalled
	state.callbackInstalled = true
	return state, entry, installCallback
}

func (t *CompletionTracker) addExit(a *Agent, task string) (*completionAgent, *completionEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.initLocked()
	if state := t.active[a]; state != nil && len(state.entries) != 0 {
		// A turn watcher already owns this worker. Its process fallback is
		// sufficient and must not create a duplicate terminal completion.
		return nil, nil
	}
	state := &completionAgent{agent: a}
	t.nextRegistration++
	entry := &completionEntry{state: state, task: task, registrationSeq: t.nextRegistration}
	state.entries = append(state.entries, entry)
	t.active[a] = state
	return state, entry
}

func (t *CompletionTracker) startProcessWaiter(state *completionAgent) {
	if state == nil || state.agent == nil {
		return
	}
	t.mu.Lock()
	if t.active[state.agent] != state || state.waitingForProcess {
		t.mu.Unlock()
		return
	}
	waitCtx, cancel := context.WithCancel(context.Background())
	state.waitingForProcess = true
	state.waitCancel = cancel
	state.waitDone = make(chan struct{})
	waitDone := state.waitDone
	t.mu.Unlock()

	go func() {
		defer close(waitDone)
		err := state.agent.WaitContext(waitCtx)
		if err == nil {
			t.completeProcess(state)
		}
	}()
}

func expectedTurnStep(a *Agent, future bool) int {
	step := a.LifetimeTurnsValue()
	if turnID := strings.TrimPrefix(a.CurrentTurnID(), "turn-"); turnID != "" {
		if current, err := strconv.Atoi(turnID); err == nil && current > step {
			step = current
		}
	}
	if future || step == 0 {
		step++
	}
	return step
}

func (t *CompletionTracker) completeNextTurn(state *completionAgent, step int, turnErr string) {
	if state == nil {
		return
	}
	t.mu.Lock()
	if t.active[state.agent] != state || len(state.entries) == 0 {
		t.mu.Unlock()
		return
	}
	if _, duplicate := state.seenTurnEnds[step]; duplicate {
		t.mu.Unlock()
		return
	}
	state.seenTurnEnds[step] = struct{}{}
	var entry *completionEntry
	for _, candidate := range state.entries {
		if !candidate.done && candidate.expectedStep == step {
			entry = candidate
			break
		}
	}
	if entry == nil {
		// Post-success callers can register after a resumed worker was queued
		// but before its next turn.start updated metadata. Accept the first
		// later step for that compatibility path; pre-delivery registrations
		// use strict identity and never consume a stale or unrelated notice.
		for _, candidate := range state.entries {
			if !candidate.done && !candidate.strictStep && candidate.expectedStep < step {
				entry = candidate
				candidate.expectedStep = step
				break
			}
		}
	}
	t.mu.Unlock()
	if entry != nil {
		t.completeTurn(entry, turnErr)
	}
}

func (t *CompletionTracker) cancelEntry(entry *completionEntry) {
	if entry == nil || entry.state == nil {
		return
	}
	var cancel context.CancelFunc
	t.mu.Lock()
	state := entry.state
	if entry.done || t.active[state.agent] != state {
		t.mu.Unlock()
		return
	}
	entry.done = true
	t.flushReadyLocked(state)
	if len(state.entries) == 0 {
		delete(t.active, state.agent)
		cancel = state.waitCancel
		state.waitCancel = nil
		state.waitingForProcess = false
	}
	t.signalLocked()
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (t *CompletionTracker) completeTurn(entry *completionEntry, turnErr string) {
	if entry == nil || entry.state == nil || entry.state.agent == nil {
		return
	}
	snap := entry.state.agent.Snapshot()
	completion := makeCompletion(entry, snap, turnErr, true)

	var cancel context.CancelFunc
	t.mu.Lock()
	state := entry.state
	if entry.done || t.active[state.agent] != state {
		t.mu.Unlock()
		return
	}
	entry.done = true
	entry.completion = &completion
	t.flushReadyLocked(state)
	if len(state.entries) == 0 {
		delete(t.active, state.agent)
		cancel = state.waitCancel
		state.waitCancel = nil
		state.waitingForProcess = false
	}
	t.signalLocked()
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (t *CompletionTracker) completeProcess(state *completionAgent) {
	if state == nil || state.agent == nil {
		return
	}
	snap := state.agent.Snapshot()

	t.mu.Lock()
	if t.active[state.agent] != state {
		t.mu.Unlock()
		return
	}
	for _, entry := range state.entries {
		if entry.done {
			continue
		}
		entry.done = true
		completion := makeCompletion(entry, snap, "", false)
		entry.completion = &completion
	}
	t.flushReadyLocked(state)
	delete(t.active, state.agent)
	state.waitCancel = nil
	state.waitingForProcess = false
	t.signalLocked()
	t.mu.Unlock()
}

func (t *CompletionTracker) flushReadyLocked(state *completionAgent) {
	for len(state.entries) != 0 && state.entries[0].done {
		entry := state.entries[0]
		state.entries[0] = nil
		state.entries = state.entries[1:]
		if entry.completion != nil {
			t.ready = append(t.ready, *entry.completion)
		}
	}
}

func makeCompletion(entry *completionEntry, snap AgentSnapshot, turnErr string, turnEnd bool) Completion {
	status := string(snap.Status)
	if turnEnd {
		status = "completed"
		if turnErr != "" {
			status = "failed"
		}
	} else if snap.Status == StatusDone {
		status = "completed"
	}

	task := entry.task
	if !entry.followUp && snap.Task != "" {
		task = snap.Task
	}
	if task == "" {
		task = snap.Task
	}

	errText := ""
	cancellation := false
	if snap.Result != nil && snap.Result.ShutdownOrigin != "" {
		if snap.Result.Status == ResultCanceled {
			status = "cancelled"
		}
		if snap.Result.Error != nil {
			errText = snap.Result.Error.Message
			cancellation = true
		}
	}
	turnError := false
	if errText == "" {
		errText = snap.Err
	}
	if errText == "" && turnErr != "" {
		errText = turnErr
		turnError = true
	}
	return Completion{
		AgentID:         snap.ID,
		Status:          status,
		Task:            task,
		Error:           errText,
		FinalResponse:   snap.LastAssistant,
		Tail:            snap.Tail,
		turnError:       turnError,
		cancellation:    cancellation,
		registrationSeq: entry.registrationSeq,
	}
}

func (t *CompletionTracker) signalLocked() {
	close(t.changed)
	t.changed = make(chan struct{})
}

func (t *CompletionTracker) takeReadyLocked() []Completion {
	if len(t.ready) == 0 {
		return nil
	}
	batch := append([]Completion(nil), t.ready...)
	t.ready = nil
	sort.SliceStable(batch, func(i, j int) bool {
		return batch[i].registrationSeq < batch[j].registrationSeq
	})
	return batch
}

// Pending reports the number of tracked entries that have not completed.
func (t *CompletionTracker) Pending() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	pending := 0
	for _, state := range t.active {
		for _, entry := range state.entries {
			if !entry.done {
				pending++
			}
		}
	}
	return pending
}

// WaitIdle returns all completions buffered so far and all completions that
// arrive until no tracked entry remains. A cancelled wait leaves active and
// buffered entries intact for a later caller.
func (t *CompletionTracker) WaitIdle(ctx context.Context) ([]Completion, error) {
	if t == nil {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	for {
		t.mu.Lock()
		t.initLocked()
		if len(t.active) == 0 {
			batch := t.takeReadyLocked()
			t.mu.Unlock()
			return batch, nil
		}
		changed := t.changed
		t.mu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			// Prefer an already-finished batch if completion won the race
			// with cancellation; otherwise preserve the active work.
			t.mu.Lock()
			if len(t.active) == 0 {
				batch := t.takeReadyLocked()
				t.mu.Unlock()
				return batch, nil
			}
			t.mu.Unlock()
			return nil, ctx.Err()
		}
	}
}

// FormatCompletionUpdate formats the stable evidence payload injected into a
// parent agent. Task and error fields are bounded to 240 characters and tail
// evidence is bounded to 600 characters; final responses retain their full
// inline text as they did in interactive mode.
func FormatCompletionUpdate(batch []Completion, instruction string) string {
	if len(batch) == 0 {
		return ""
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "[auto-subagents update] %d sub-agent(s) finished:\n\n", len(batch))
	for idx, completion := range batch {
		fmt.Fprintf(&sb, "%d. agent %s - status: %s\n", idx+1, completion.AgentID, completion.Status)
		fmt.Fprintf(&sb, "   task: %s\n", truncateCompletionField(completion.Task, 240))
		if completion.Error != "" {
			label := "error"
			if completion.cancellation {
				label = "cancellation"
			} else if completion.turnError {
				label = "turn error"
			}
			fmt.Fprintf(&sb, "   %s: %s\n", label, truncateCompletionField(completion.Error, 240))
		}
		if completion.FinalResponse != "" {
			sb.WriteString("   final response:\n")
			sb.WriteString(completion.FinalResponse)
			sb.WriteString("\n")
		} else if tail := strings.TrimSpace(completion.Tail); tail != "" {
			fmt.Fprintf(&sb, "   tail: %s\n", truncateCompletionField(tail, 600))
		}
		sb.WriteString("\n")
	}
	if instruction != "" {
		sb.WriteString(instruction)
	}
	return sb.String()
}

func truncateCompletionField(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit <= 3 {
		return string(runes[:limit])
	}
	return string(runes[:limit-3]) + "..."
}
