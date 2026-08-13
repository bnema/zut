package subagents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

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
	Task    string
	Err     error
}

// ResidentChild serializes prompt execution through one control goroutine.
// No mutex is held while a runner performs provider or tool I/O. When a
// journal is configured, every state boundary is committed by that goroutine.
type ResidentChild struct {
	runner       ResidentTurnRunner
	journal      *ResidentJournal
	workspace    WorkspaceHandle
	spec         ResidentChildSpec
	onCompletion func(ResidentCompletion)
	onUpdate     func()

	ctx    context.Context
	cancel context.CancelFunc
	inbox  chan residentPrompt
	done   chan struct{}
	once   sync.Once

	mu               sync.RWMutex
	state            ResidentState
	live             *residentLiveProjection
	cleanupWorkspace bool
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
	child := &ResidentChild{
		runner:    runner,
		journal:   journal,
		workspace: workspace,
		spec:      spec,
		ctx:       ctx,
		cancel:    cancel,
		inbox:     make(chan residentPrompt),
		done:      make(chan struct{}),
		state:     ResidentQueued,
		live:      newResidentLiveProjection(),
	}
	if journal != nil {
		journal.SetEventObserver(func(event core.AgentEvent) {
			child.live.Apply(event)
			child.notifyUpdate()
		})
	}
	go child.run()
	return child
}

// SetUpdateObserver receives state and live-projection changes after they are
// visible to readers. It is used by the host to schedule a redraw; it must not
// block resident execution.
func (c *ResidentChild) SetUpdateObserver(observer func()) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.onUpdate = observer
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

func (c *ResidentChild) setState(state ResidentState) {
	c.mu.Lock()
	c.state = state
	observer := c.onUpdate
	c.mu.Unlock()
	if observer != nil {
		observer()
	}
}

func (c *ResidentChild) notifyUpdate() {
	if c == nil {
		return
	}
	c.mu.RLock()
	observer := c.onUpdate
	c.mu.RUnlock()
	if observer != nil {
		observer()
	}
}

// Resume queues a prompt for the legacy in-memory construction seam. Manager
// callers must use resumeAccepted after the journal records acceptance.
func (c *ResidentChild) Resume(ctx context.Context, prompt string) error {
	return c.enqueue(ctx, residentPrompt{turnID: uuid.NewString(), prompt: prompt, ack: make(chan error, 1)})
}

func (c *ResidentChild) resumeAccepted(ctx context.Context, turnID, prompt string) error {
	return c.enqueue(ctx, residentPrompt{turnID: turnID, prompt: prompt, ack: make(chan error, 1)})
}

func (c *ResidentChild) enqueue(ctx context.Context, request residentPrompt) error {
	if c == nil || c.runner == nil {
		return errors.New("resident child: no runner")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request.prompt = strings.TrimSpace(request.prompt)
	if request.prompt == "" {
		return errors.New("resident child: prompt is empty")
	}
	if strings.TrimSpace(request.turnID) == "" {
		return errors.New("resident child: missing turn ID")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return errors.New("resident child: closed")
	case c.inbox <- request:
	}
	select {
	case err := <-request.ack:
		return err
	case <-c.ctx.Done():
		return errors.New("resident child: closed before prompt acceptance")
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
		c.cleanupWorkspace = c.state == ResidentIdle
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
	results := make(chan residentTurnResult, 1)
	var active residentPrompt
	running, interrupted := false, false
	for {
		if !interrupted && !running && len(queue) > 0 {
			active, queue = queue[0], queue[1:]
			if c.journal != nil {
				if err := c.journal.RecordTurnStarted(c.spec, active.turnID); err != nil {
					c.setState(ResidentFailed)
					c.cancel()
					continue
				}
			}
			c.live.Start(active.turnID)
			c.setState(ResidentRunning)
			running = true
			go func(request residentPrompt) {
				results <- residentTurnResult{turnID: request.turnID, err: c.runner(c.ctx, request.prompt)}
			}(active)
		}

		if interrupted && !running {
			c.live.Finish(ResidentInterrupted)
			c.setState(ResidentInterrupted)
			return
		}

		select {
		case <-c.ctx.Done():
			if interrupted {
				continue
			}
			interrupted = true
			if running && c.journal != nil {
				_ = c.journal.RecordTurnInterrupted(c.spec, active.turnID)
			}
			for _, pending := range queue {
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
			if terminalErr == nil && c.workspace != nil && c.workspace.Mode() == WorkspaceWorktree {
				captured, err := c.workspace.Capture(c.ctx)
				if err != nil {
					terminalErr = fmt.Errorf("capture resident worktree: %w", err)
				} else {
					capture = &captured
				}
			}
			if c.journal != nil {
				if err := c.journal.RecordTurnFinishedWithCapture(c.spec, result.turnID, terminalErr, capture); err != nil {
					c.setState(ResidentFailed)
					c.cancel()
					continue
				}
			}
			if terminalErr != nil {
				c.live.Finish(ResidentFailed)
				c.setState(ResidentFailed)
			} else {
				c.live.Finish(ResidentIdle)
				c.setState(ResidentIdle)
			}
			if c.onCompletion != nil {
				c.onCompletion(ResidentCompletion{ChildID: c.spec.ID, Task: active.prompt, Err: terminalErr})
			}
		}
	}
}
