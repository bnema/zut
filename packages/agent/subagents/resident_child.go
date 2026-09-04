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

// ResidentTurnRunner executes one already-accepted prompt. The child owns the
// ordering and cancellation boundary; implementations own agent/provider work.
type ResidentTurnRunner func(context.Context, string) error

type residentPrompt struct {
	turnID string
	prompt string
	ack    chan error
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
}

// ResidentChild serializes prompt execution through one control goroutine.
// No mutex is held while a runner performs provider or tool I/O. When a
// journal is configured, every state boundary is committed by that goroutine.
type ResidentChild struct {
	runner        ResidentTurnRunner
	journal       *ResidentJournal
	workspace     WorkspaceHandle
	spec          ResidentChildSpec
	onCompletion  func(ResidentCompletion)
	onUpdate      func(historyChanged bool)
	onStateChange func(ResidentState)

	ctx    context.Context
	cancel context.CancelFunc
	inbox  chan residentPrompt
	done   chan struct{}
	once   sync.Once

	mu                sync.RWMutex
	state             ResidentState
	stateUpdatedAt    time.Time
	turnStartedAt     time.Time
	activityUpdatedAt time.Time
	live              *residentLiveProjection
	cleanupWorkspace  bool
}

func NewResidentChild(runner ResidentTurnRunner) *ResidentChild {
	return newResidentChild(ResidentChildSpec{}, nil, runner)
}

func newJournaledResidentChild(spec ResidentChildSpec, journal *ResidentJournal, runner ResidentTurnRunner, onCompletion func(ResidentCompletion)) *ResidentChild {
	child := newResidentChildWithWorkspace(spec, journal, nil, runner)
	child.onCompletion = onCompletion
	return child
}

func newJournaledResidentChildWithWorkspace(spec ResidentChildSpec, journal *ResidentJournal, workspace WorkspaceHandle, runner ResidentTurnRunner, onCompletion func(ResidentCompletion)) *ResidentChild {
	child := newResidentChildWithWorkspace(spec, journal, workspace, runner)
	child.onCompletion = onCompletion
	return child
}

func newResidentChild(spec ResidentChildSpec, journal *ResidentJournal, runner ResidentTurnRunner) *ResidentChild {
	return newResidentChildWithWorkspace(spec, journal, nil, runner)
}

func newResidentChildWithWorkspace(spec ResidentChildSpec, journal *ResidentJournal, workspace WorkspaceHandle, runner ResidentTurnRunner) *ResidentChild {
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now().UTC()
	child := &ResidentChild{
		runner:            runner,
		journal:           journal,
		workspace:         workspace,
		spec:              spec,
		ctx:               ctx,
		cancel:            cancel,
		inbox:             make(chan residentPrompt),
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

func (c *ResidentChild) enqueueAccepted(ctx context.Context, request residentPrompt) (bool, error) {
	if c == nil || c.runner == nil {
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
	results := make(chan residentTurnResult, 1)
	var active residentPrompt
	running, interrupted := false, false
	for {
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
			running = true
			go func(request residentPrompt) {
				results <- residentTurnResult{turnID: request.turnID, err: c.runner(c.ctx, request.prompt)}
			}(active)
		}

		if interrupted && !running {
			c.live.Finish(ResidentInterrupted)
			c.setState(ResidentInterrupted)
			if c.onCompletion != nil {
				for _, prompt := range interruptedPrompts {
					c.onCompletion(ResidentCompletion{ChildID: c.spec.ID, TurnID: prompt.turnID, Task: prompt.prompt, Err: context.Canceled})
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
				interruptedPrompts = append(interruptedPrompts, pending)
				if c.journal != nil {
					_ = c.journal.RecordTurnInterrupted(c.spec, pending.turnID)
				}
			}
			queue = nil
		case request := <-c.inbox:
			if interrupted {
				request.ack <- errors.New("resident child: closed")
				continue
			}
			queue = append(queue, request)
			request.ack <- nil
		case result := <-results:
			running = false
			if interrupted {
				continue
			}
			terminalErr := result.err
			var capture *WorkspaceCapture
			if (terminalErr == nil || errors.Is(terminalErr, ErrBudgetExceeded)) && c.workspace != nil && c.workspace.Mode() == WorkspaceWorktree {
				captured, err := c.workspace.Capture(c.ctx)
				if err != nil {
					terminalErr = errors.Join(terminalErr, fmt.Errorf("capture resident worktree: %w", err))
				} else {
					capture = &captured
				}
			}
			persistenceFailed := false
			if c.journal != nil {
				if err := c.journal.RecordTurnFinishedWithCapture(c.spec, result.turnID, terminalErr, capture); err != nil {
					terminalErr = fmt.Errorf("persist resident child terminal state: %w", err)
					persistenceFailed = true
				}
			}
			state := ResidentIdle
			if terminalErr != nil {
				state = ResidentFailed
				if errors.Is(terminalErr, ErrBudgetExceeded) {
					state = ResidentBudgetExhausted
				}
			}
			summary := ""
			if c.journal != nil && !persistenceFailed {
				if result, err := c.journal.Result(); err == nil {
					state = result.State
					summary = result.Summary
					if result.Handoff != "" {
						summary = result.Handoff
					}
				}
			}
			c.live.Finish(state)
			c.setState(state)
			if c.onCompletion != nil {
				c.onCompletion(ResidentCompletion{ChildID: c.spec.ID, TurnID: result.turnID, Task: active.prompt, Err: terminalErr, Summary: summary})
				if persistenceFailed {
					for _, pending := range queue {
						c.onCompletion(ResidentCompletion{ChildID: c.spec.ID, TurnID: pending.turnID, Task: pending.prompt, Err: terminalErr})
					}
				}
			}
			if persistenceFailed {
				return
			}
		}
	}
}
