package subagents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/google/uuid"
)

// ResidentRuntime owns one child's agent. The child owns turn ordering and
// cancellation; the runtime owns agent/provider work.
//
// Run executes one accepted turn. Steer injects a follow-up into the turn Run
// is executing and reports false when no turn can still receive it; a true
// result guarantees Run delivers the follow-up before it returns successfully.
// PendingSteers counts follow-ups not yet delivered, and DrainSteers removes
// and returns them after a turn ended without delivering them.
type ResidentRuntime interface {
	Run(ctx context.Context, prompt string) error
	Steer(prompt string) bool
	PendingSteers() int
	DrainSteers() []string
}

// ResidentTurnRunner is a runtime that runs turns but cannot be steered, so
// every follow-up becomes a separate queued turn.
type ResidentTurnRunner func(context.Context, string) error

func (r ResidentTurnRunner) Run(ctx context.Context, prompt string) error { return r(ctx, prompt) }
func (ResidentTurnRunner) Steer(string) bool                              { return false }
func (ResidentTurnRunner) PendingSteers() int                             { return 0 }
func (ResidentTurnRunner) DrainSteers() []string                          { return nil }

type residentPrompt struct {
	turnID string
	prompt string
	ack    chan error
}

type residentControlKind int

const (
	residentControlSteer residentControlKind = iota + 1
	residentControlInterrupt
)

// residentControl is a request handled by the control goroutine, so it is
// ordered with turn start and completion.
type residentControl struct {
	kind   residentControlKind
	prompt string
	// onSteered runs on the control goroutine before the active turn can
	// report its completion, so a caller can subscribe without a race.
	onSteered func(turnID string)
	reply     chan residentControlReply
}

type residentControlReply struct {
	ok     bool
	turnID string
}

type residentTurnResult struct {
	turnID string
	err    error
}

// ResidentCompletion is a typed in-process terminal turn notification.
type ResidentCompletion struct {
	ChildID string
	TurnID  string
	Task    string
	Err     error
	Summary string
	// NotStarted marks a queued follow-up that was dropped before the child
	// ever ran it, for example because the child was stopped or interrupted.
	NotStarted bool
	// Undelivered lists follow-ups steered into this turn that the child never
	// read because the turn ended first.
	Undelivered []string
}

// ResidentChild serializes prompt execution through one control goroutine.
// No mutex is held while a runner performs provider or tool I/O. When a
// journal is configured, every state boundary is committed by that goroutine.
type ResidentChild struct {
	runtime       ResidentRuntime
	journal       *ResidentJournal
	workspace     WorkspaceHandle
	spec          ResidentChildSpec
	onCompletion  func(ResidentCompletion)
	onUpdate      func(historyChanged bool)
	onStateChange func(ResidentState)

	ctx     context.Context
	cancel  context.CancelFunc
	inbox   chan residentPrompt
	control chan residentControl
	done    chan struct{}
	once    sync.Once

	mu                sync.RWMutex
	state             ResidentState
	stateUpdatedAt    time.Time
	turnStartedAt     time.Time
	activityUpdatedAt time.Time
	live              *residentLiveProjection
	cleanupWorkspace  bool
	queuedTurns       int
}

func NewResidentChild(runtime ResidentRuntime) *ResidentChild {
	return newResidentChild(ResidentChildSpec{}, nil, runtime)
}

func newJournaledResidentChild(spec ResidentChildSpec, journal *ResidentJournal, runtime ResidentRuntime, onCompletion func(ResidentCompletion)) *ResidentChild {
	child := newResidentChildWithWorkspace(spec, journal, nil, runtime)
	child.onCompletion = onCompletion
	return child
}

func newJournaledResidentChildWithWorkspace(spec ResidentChildSpec, journal *ResidentJournal, workspace WorkspaceHandle, runtime ResidentRuntime, onCompletion func(ResidentCompletion)) *ResidentChild {
	child := newResidentChildWithWorkspace(spec, journal, workspace, runtime)
	child.onCompletion = onCompletion
	return child
}

func newResidentChild(spec ResidentChildSpec, journal *ResidentJournal, runtime ResidentRuntime) *ResidentChild {
	return newResidentChildWithWorkspace(spec, journal, nil, runtime)
}

func newResidentChildWithWorkspace(spec ResidentChildSpec, journal *ResidentJournal, workspace WorkspaceHandle, runtime ResidentRuntime) *ResidentChild {
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now().UTC()
	child := &ResidentChild{
		runtime:           runtime,
		journal:           journal,
		workspace:         workspace,
		spec:              spec,
		ctx:               ctx,
		cancel:            cancel,
		inbox:             make(chan residentPrompt),
		control:           make(chan residentControl),
		done:              make(chan struct{}),
		state:             ResidentQueued,
		stateUpdatedAt:    now,
		activityUpdatedAt: now,
		live:              newResidentLiveProjection(),
	}
	if journal != nil {
		child.live.SeedUsage(journal.usageSnapshot())
		journal.SetEventObserver(func(event core.AgentEvent) {
			child.live.Apply(event)
			child.recordActivity()
			child.notifyUpdate(residentEventChangesHistory(event))
		})
	}
	go child.run()
	return child
}

// setUpdateObserver receives state and live-projection changes after they are
// visible to readers. historyChanged is true only after a finalized event was
// appended to the resident transcript. It must not block resident execution.
func (c *ResidentChild) setUpdateObserver(observer func(historyChanged bool)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.onUpdate = observer
	c.mu.Unlock()
}

// setStateObserver receives lifecycle transitions after the child state lock
// has been released. It is used by the manager to maintain cheap activity
// notifications for independent UI animation.
func (c *ResidentChild) setStateObserver(observer func(ResidentState)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.onStateChange = observer
	c.mu.Unlock()
}

// Live returns an immutable copy of the unfinished visible turn.
func (c *ResidentChild) Live() ResidentLiveSnapshot {
	if c == nil {
		return ResidentLiveSnapshot{}
	}
	return c.live.Snapshot()
}

func (c *ResidentChild) State() ResidentState {
	if c == nil {
		return ResidentStopped
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

func (c *ResidentChild) StateUpdatedAt() time.Time {
	if c == nil {
		return time.Time{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stateUpdatedAt
}

// TurnStartedAt is the start time of the active or most recent resident turn.
func (c *ResidentChild) TurnStartedAt() time.Time {
	if c == nil {
		return time.Time{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.turnStartedAt
}

// ActivityUpdatedAt is refreshed by visible turn events so callers can show
// how long the child has been waiting for its next observable action.
func (c *ResidentChild) ActivityUpdatedAt() time.Time {
	if c == nil {
		return time.Time{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.activityUpdatedAt
}

// PendingFollowUps counts follow-ups the child has accepted but not yet read:
// queued turns plus messages steered into the running turn.
func (c *ResidentChild) PendingFollowUps() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	queued := c.queuedTurns
	c.mu.RUnlock()
	if c.runtime != nil {
		queued += c.runtime.PendingSteers()
	}
	return queued
}

func (c *ResidentChild) setQueuedTurns(n int) {
	c.mu.Lock()
	c.queuedTurns = n
	c.mu.Unlock()
}

func (c *ResidentChild) setState(state ResidentState) {
	c.mu.Lock()
	now := time.Now().UTC()
	c.state = state
	c.stateUpdatedAt = now
	c.activityUpdatedAt = now
	observer := c.onUpdate
	stateObserver := c.onStateChange
	c.mu.Unlock()
	if stateObserver != nil {
		stateObserver(state)
	}
	if observer != nil {
		observer(false)
	}
}

func (c *ResidentChild) startTurn(turnID string) {
	if c == nil {
		return
	}
	c.live.Start(turnID)
	c.mu.Lock()
	now := time.Now().UTC()
	c.state = ResidentRunning
	c.stateUpdatedAt = now
	c.turnStartedAt = now
	c.activityUpdatedAt = now
	observer := c.onUpdate
	stateObserver := c.onStateChange
	c.mu.Unlock()
	if stateObserver != nil {
		stateObserver(ResidentRunning)
	}
	if observer != nil {
		observer(false)
	}
}

func (c *ResidentChild) recordActivity() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.activityUpdatedAt = time.Now().UTC()
	c.mu.Unlock()
}

func (c *ResidentChild) notifyUpdate(historyChanged bool) {
	if c == nil {
		return
	}
	c.mu.RLock()
	observer := c.onUpdate
	c.mu.RUnlock()
	if observer != nil {
		observer(historyChanged)
	}
}

func residentEventChangesHistory(event core.AgentEvent) bool {
	switch event.(type) {
	case core.EvUserMessage, core.EvAssistantMessage, core.EvToolCall, core.EvToolResult:
		return true
	default:
		return false
	}
}

// Resume queues a prompt for the legacy in-memory construction seam. Manager
// callers must use resumeAccepted after the journal records acceptance.
func (c *ResidentChild) Resume(ctx context.Context, prompt string) error {
	return c.enqueue(ctx, residentPrompt{turnID: uuid.NewString(), prompt: prompt, ack: make(chan error, 1)})
}

// resumeAccepted queues durably accepted work and reports whether the child
// took ownership of its terminal completion. Once it has taken ownership, it
// emits the completion itself even when turn startup fails.
func (c *ResidentChild) resumeAccepted(ctx context.Context, turnID, prompt string) (bool, error) {
	return c.enqueueAccepted(ctx, residentPrompt{turnID: turnID, prompt: prompt, ack: make(chan error, 1)})
}

func (c *ResidentChild) enqueue(ctx context.Context, request residentPrompt) error {
	_, err := c.enqueueAccepted(ctx, request)
	return err
}

// steer injects a follow-up into the running turn. It reports false, leaving
// the child unchanged, when no turn is executing or earlier follow-ups are
// still queued; the caller must then queue the follow-up as a new turn.
func (c *ResidentChild) steer(ctx context.Context, prompt string, onSteered func(turnID string)) (bool, string, error) {
	reply, err := c.sendControl(ctx, residentControl{kind: residentControlSteer, prompt: strings.TrimSpace(prompt), onSteered: onSteered})
	return reply.ok, reply.turnID, err
}

// interruptTurn cancels only the running turn. The child stays alive with its
// transcript, and queued follow-ups are dropped. It reports false when no turn
// is running.
func (c *ResidentChild) interruptTurn(ctx context.Context) (bool, error) {
	reply, err := c.sendControl(ctx, residentControl{kind: residentControlInterrupt})
	return reply.ok, err
}

func (c *ResidentChild) sendControl(ctx context.Context, request residentControl) (residentControlReply, error) {
	if c == nil || c.runtime == nil {
		return residentControlReply{}, errors.New("resident child: no runtime")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request.reply = make(chan residentControlReply, 1)
	select {
	case <-ctx.Done():
		return residentControlReply{}, ctx.Err()
	case <-c.done:
		return residentControlReply{}, errors.New("resident child: closed")
	case c.control <- request:
	}
	return <-request.reply, nil
}

func (c *ResidentChild) enqueueAccepted(ctx context.Context, request residentPrompt) (bool, error) {
	if c == nil || c.runtime == nil {
		return false, errors.New("resident child: no runner")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request.prompt = strings.TrimSpace(request.prompt)
	if request.prompt == "" {
		return false, errors.New("resident child: prompt is empty")
	}
	if strings.TrimSpace(request.turnID) == "" {
		return false, errors.New("resident child: missing turn ID")
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-c.ctx.Done():
		return false, errors.New("resident child: closed")
	case c.inbox <- request:
	}
	select {
	case err := <-request.ack:
		return true, err
	case <-c.ctx.Done():
		return true, errors.New("resident child: closed before prompt acceptance")
	}
}

func (c *ResidentChild) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.once.Do(func() {
		c.mu.Lock()
		c.cleanupWorkspace = c.state == ResidentIdle || c.state == ResidentCompleted
		c.mu.Unlock()
		c.cancel()
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return nil
	}
}

func (c *ResidentChild) run() {
	defer close(c.done)
	defer func() {
		if c.journal != nil {
			_ = c.journal.Close()
		}
		c.mu.RLock()
		cleanupWorkspace := c.cleanupWorkspace
		c.mu.RUnlock()
		if c.workspace != nil && c.workspace.Mode() == WorkspaceWorktree && cleanupWorkspace {
			_ = c.workspace.Cleanup(context.Background())
		}
	}()

	var queue []residentPrompt
	var interruptedPrompts []residentPrompt
	var droppedPrompts []residentPrompt
	results := make(chan residentTurnResult, 1)
	var active residentPrompt
	turnCancel := context.CancelFunc(func() {})
	defer func() { turnCancel() }()
	running, interrupted, turnInterrupted := false, false, false
	// dropQueued reports queued follow-ups that never started. Each was
	// accepted and tracked, so each still gets exactly one completion, marked
	// NotStarted so the parent does not mistake it for interrupted work.
	dropQueued := func(err error) {
		for _, pending := range queue {
			if c.journal != nil {
				_ = c.journal.RecordTurnInterrupted(c.spec, pending.turnID)
			}
			if c.onCompletion != nil {
				c.onCompletion(ResidentCompletion{ChildID: c.spec.ID, TurnID: pending.turnID, Task: pending.prompt, Err: err, NotStarted: true})
			}
		}
		queue = nil
	}
	for {
		c.setQueuedTurns(len(queue))
		if !interrupted && !running && len(queue) > 0 {
			active, queue = queue[0], queue[1:]
			if c.journal != nil {
				if err := c.journal.RecordTurnStarted(c.spec, active.turnID); err != nil {
					terminalErr := fmt.Errorf("persist resident child start state: %w", err)
					c.live.Finish(ResidentFailed)
					c.setState(ResidentFailed)
					c.cancel()
					if c.onCompletion != nil {
						c.onCompletion(ResidentCompletion{ChildID: c.spec.ID, TurnID: active.turnID, Task: active.prompt, Err: terminalErr})
						for _, pending := range queue {
							c.onCompletion(ResidentCompletion{ChildID: c.spec.ID, TurnID: pending.turnID, Task: pending.prompt, Err: terminalErr})
						}
					}
					return
				}
			}
			c.startTurn(active.turnID)
			running, turnInterrupted = true, false
			turnCtx, cancelTurn := context.WithCancel(c.ctx)
			turnCancel = cancelTurn
			go func(ctx context.Context, request residentPrompt) {
				results <- residentTurnResult{turnID: request.turnID, err: c.runtime.Run(ctx, request.prompt)}
			}(turnCtx, active)
		}

		if interrupted && !running {
			c.live.Finish(ResidentInterrupted)
			c.setState(ResidentInterrupted)
			if c.onCompletion != nil {
				for _, prompt := range interruptedPrompts {
					c.onCompletion(ResidentCompletion{ChildID: c.spec.ID, TurnID: prompt.turnID, Task: prompt.prompt, Err: context.Canceled, Undelivered: c.runtime.DrainSteers()})
				}
				for _, prompt := range droppedPrompts {
					c.onCompletion(ResidentCompletion{ChildID: c.spec.ID, TurnID: prompt.turnID, Task: prompt.prompt, Err: context.Canceled, NotStarted: true})
				}
			}
			return
		}

		canceled := c.ctx.Done()
		if interrupted {
			canceled = nil
		}
		select {
		case <-canceled:
			if interrupted {
				continue
			}
			interrupted = true
			if running {
				interruptedPrompts = append(interruptedPrompts, active)
				if c.journal != nil {
					_ = c.journal.RecordTurnInterrupted(c.spec, active.turnID)
				}
			}
			for _, pending := range queue {
				droppedPrompts = append(droppedPrompts, pending)
				if c.journal != nil {
					_ = c.journal.RecordTurnInterrupted(c.spec, pending.turnID)
				}
			}
			queue = nil
			c.setQueuedTurns(0)
		case request := <-c.control:
			switch request.kind {
			case residentControlSteer:
				// Steering past queued turns would reorder follow-ups, and an
				// interrupted turn can no longer read new input.
				if running && !interrupted && !turnInterrupted && len(queue) == 0 && request.prompt != "" && c.runtime.Steer(request.prompt) {
					if request.onSteered != nil {
						request.onSteered(active.turnID)
					}
					c.notifyUpdate(false)
					request.reply <- residentControlReply{ok: true, turnID: active.turnID}
				} else {
					request.reply <- residentControlReply{}
				}
			case residentControlInterrupt:
				if !running || interrupted || turnInterrupted {
					request.reply <- residentControlReply{}
					continue
				}
				turnInterrupted = true
				turnCancel()
				dropQueued(context.Canceled)
				c.setQueuedTurns(0)
				request.reply <- residentControlReply{ok: true, turnID: active.turnID}
			}
		case request := <-c.inbox:
			if interrupted {
				request.ack <- errors.New("resident child: closed")
				continue
			}
			queue = append(queue, request)
			request.ack <- nil
		case result := <-results:
			running = false
			turnCancel()
			if interrupted {
				continue
			}
			terminalErr := result.err
			if turnInterrupted && terminalErr == nil {
				// The turn finished before cancellation reached it; the
				// manager still asked for an interruption.
				terminalErr = context.Canceled
			}
			undelivered := c.runtime.DrainSteers()
			var capture *WorkspaceCapture
			if (terminalErr == nil || !errors.Is(terminalErr, context.Canceled)) && c.workspace != nil && c.workspace.Mode() == WorkspaceWorktree {
				captured, err := c.workspace.Capture(c.ctx)
				if err != nil {
					terminalErr = errors.Join(terminalErr, fmt.Errorf("capture resident worktree: %w", err))
				} else {
					capture = &captured
				}
			}
			state := ResidentIdle
			if terminalErr != nil {
				state = ResidentFailed
			}
			summary := ""
			persistenceFailed := false
			if c.journal != nil {
				projection, err := c.journal.finishTurn(c.spec, result.turnID, terminalErr, capture)
				if err != nil {
					terminalErr = fmt.Errorf("persist resident child terminal state: %w", err)
					state, persistenceFailed = ResidentFailed, true
				} else {
					state, summary = projection.State, projection.Summary
					if projection.Handoff != "" {
						summary = projection.Handoff
					}
				}
			}
			c.live.Finish(state)
			c.setState(state)
			if c.onCompletion != nil {
				c.onCompletion(ResidentCompletion{ChildID: c.spec.ID, TurnID: result.turnID, Task: active.prompt, Err: terminalErr, Summary: summary, Undelivered: undelivered})
			}
			if persistenceFailed {
				dropQueued(terminalErr)
				return
			}
		}
	}
}
