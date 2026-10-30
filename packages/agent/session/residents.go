package session

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/bnema/zut/packages/agent/internal/orchestration"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
)

type residentState struct {
	controller  *Controller
	tracker     *subagents.CompletionTracker
	coordinator *orchestration.Coordinator
	specs       map[string]subagents.ResidentChildSpec
	tasks       map[string]string
	inboxMu     sync.Mutex
	inbox       []func()
	dirty       bool
	closed      bool
	wake        chan struct{}
	// stopDispatcher cancels a pending actor post; dispatcherDone joins it.
	stopDispatcher context.CancelFunc
	dispatcherDone chan struct{}
}

func (s *residentState) snapshotInto(out *Snapshot) {}

// ResidentAction forwards a dialog action without blocking the session actor on
// child shutdown or resume. The caller's context bounds that operation.
func (c *Controller) ResidentAction(ctx context.Context, command ResidentCommand) error {
	m := c.deps.ResidentManager
	if m == nil {
		return errors.New("resident manager unavailable")
	}
	switch command.Op {
	case "stop":
		return m.Stop(ctx, command.ChildID)
	case "interrupt":
		_, err := m.Interrupt(ctx, command.ChildID)
		return err
	case "resume":
		return m.Resume(ctx, command.ChildID, command.Prompt)
	default:
		return errors.New("unknown resident action: " + command.Op)
	}
}

func (c *Controller) residentsRunning() bool {
	for _, r := range c.residentSummaries() {
		if r.State == string(subagents.ResidentRunning) || r.State == string(subagents.ResidentQueued) {
			return true
		}
	}
	return false
}

// residentSummaries uses accepted specifications for workspace and spawn task;
// recovered children without an accepted specification leave these fields empty.
// Task previews contain the first line, bounded to 120 Unicode code points.
func (c *Controller) residentSummaries() []ResidentSummary {
	if c.deps.ResidentManager == nil {
		return nil
	}
	snapshots := c.deps.ResidentManager.Snapshot()
	out := make([]ResidentSummary, 0, len(snapshots))
	for _, r := range snapshots {
		// Children leased by another session are not ours to show.
		if r.OwnedElsewhere {
			continue
		}
		out = append(out, ResidentSummary{ID: r.ID, Profile: r.Profile, State: string(r.State), Workspace: c.res.specs[r.ID].Workspace, Task: c.res.tasks[r.ID], UpdatedAt: r.UpdatedAt, HistoryRev: r.HistoryRev})
	}
	return out
}

// Agent turn events include retry boundaries, not manager-run boundaries.
func (s *residentState) observe(core.AgentEvent) {}

func (c *Controller) residentsRunStarted() {
	c.res.deliver(c.res.coordinator.Apply(orchestration.Event{Kind: orchestration.EventManagerStarted}).Actions)
}
func (c *Controller) residentsRunEnded() {
	c.res.deliver(c.res.coordinator.Apply(orchestration.Event{Kind: orchestration.EventManagerFinished}).Actions)
}

// The mailbox has no numeric cap: it holds only acceptances and completions of
// tracked turns, both bounded by the residents the manager spawned (one
// completion per tracked turn; duplicates are rejected by the tracker). Updates
// carry no state and coalesce into one dirty flag. Refusing a tracked item
// would lose an async result the manager never retries.

// postResidentLocked queues observer work without waiting for the actor. The
// caller holds inboxMu, which serializes admission with residentsShutdown.
// A single dispatcher preserves order, including acceptance before completion.
func (c *Controller) postResidentLocked(fn func()) bool {
	s := &c.res
	if s.closed {
		return false
	}
	s.inbox = append(s.inbox, fn)
	s.wakeLocked()
	return true
}

func (s *residentState) wakeLocked() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// runPending runs on the actor. Work stays queued until the actor takes it, so
// an abandoned post never loses accepted completions. Delivered updates are
// only enqueued; an idle manager has no turn end to drain them, so runPending
// starts the queue itself unless the session is shutting down.
func (s *residentState) runPending() {
	for {
		s.inboxMu.Lock()
		items, dirty, closed := s.inbox, s.dirty, s.closed
		s.inbox, s.dirty = nil, false
		s.inboxMu.Unlock()
		if len(items) == 0 && !dirty {
			return
		}
		for _, fn := range items {
			fn()
		}
		if len(items) > 0 && !closed {
			s.controller.turnSeam.drainQueued()
		}
		if dirty {
			s.controller.publish(EvInfo, InfoEvent{Info: s.controller.residentInfo()})
		}
	}
}

// residentsShutdown runs on the actor before its loop exits. It closes
// admission (later callbacks return false), joins the dispatcher, and drains
// accepted work into the host queue so each accepted completion is delivered
// exactly once. It is idempotent.
func (c *Controller) residentsShutdown() {
	s := &c.res
	if s.dispatcherDone == nil {
		return
	}
	s.inboxMu.Lock()
	already := s.closed
	s.closed = true
	s.inboxMu.Unlock()
	if already {
		return
	}
	s.stopDispatcher()
	<-s.dispatcherDone
	s.runPending()
	s.inboxMu.Lock()
	s.inbox, s.dirty = nil, false
	s.inboxMu.Unlock()
}

// HeadlessCompletionInstruction is the parent instruction appended to
// completion updates of headless --orchestrate sessions.
const HeadlessCompletionInstruction = `Treat the completion reports below as evidence, including failures. Do not retry failed workers automatically. Decide whether another independent dependency wave is required; if so, use the manager tools and end or yield the turn again. Otherwise synthesize the final answer for the user. Do not mention this orchestration protocol unless it is relevant to the answer.`

func (s *residentState) deliver(actions []orchestration.Action) {
	for _, a := range actions {
		if len(a.Completions) == 0 {
			continue
		}
		if s.controller.deps.HeadlessOrchestrate {
			// Mirrors the pre-daemon headless --orchestrate continuation.
			s.controller.turnSeam.enqueue(Input{Origin: InputOriginResident, HostEvent: true, Text: subagents.FormatCompletionUpdate(a.Completions, HeadlessCompletionInstruction)})
			continue
		}
		instruction := "Briefly summarise the collective outcome for the user. Reference the agents by id. If any failed, suggest a follow-up; otherwise confirm completion. Do not spawn new sub-agents unless the user asks."
		if a.Kind == orchestration.ActionQueueManager || len(a.ActiveAgentIDs) != 0 {
			instruction = "Treat these completion reports as worker evidence, not a new user request. Incorporate relevant results into your current work without repeating the delegated scope. Other workers may still be running; do not poll solely to wait for them."
		}
		s.controller.turnSeam.enqueue(Input{Origin: InputOriginResident, HostEvent: true, Text: subagents.FormatCompletionUpdateWithActive(a.Completions, a.ActiveAgentIDs, instruction)})
	}
}

// residentsInit installs non-blocking, ordered observers before work is accepted.
func (c *Controller) residentsInit() {
	s := &c.res
	s.controller = c
	s.tracker = subagents.NewCompletionTracker()
	s.coordinator = orchestration.New()
	s.specs = make(map[string]subagents.ResidentChildSpec)
	s.tasks = make(map[string]string)
	s.wake = make(chan struct{}, 1)
	ctx, stop := context.WithCancel(context.Background())
	s.stopDispatcher = stop
	s.dispatcherDone = make(chan struct{})
	// The dispatcher only posts; it never waits for the actor to run the work.
	go func() {
		defer close(s.dispatcherDone)
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.done:
				return
			case <-s.wake:
			}
			if c.post(ctx, s.runPending) != nil {
				return
			}
		}
	}()
	m := c.deps.ResidentManager
	if m == nil {
		return
	}
	m.SetAcceptedObserver(func(spec subagents.ResidentChildSpec, turnID, prompt string) {
		s.inboxMu.Lock()
		defer s.inboxMu.Unlock()
		if s.closed {
			return
		}
		if !s.tracker.TrackResident(spec.ID, turnID) {
			return
		}
		c.logger().Info("subagent spawned", "agent", spec.ID, "turn", turnID)
		c.postResidentLocked(func() {
			if _, exists := s.specs[spec.ID]; !exists {
				s.specs[spec.ID] = spec
				line, _, _ := strings.Cut(prompt, "\n")
				runes := []rune(strings.TrimSpace(line))
				if len(runes) > 120 {
					runes = runes[:120]
				}
				s.tasks[spec.ID] = string(runes)
			}
			// An acceptance outside a manager turn forms an implicit sealed wave.
			busy := c.turnSeam.isBusy()
			s.coordinator.Apply(orchestration.Event{Kind: orchestration.EventManagerStarted})
			s.coordinator.Apply(orchestration.Event{Kind: orchestration.EventWorkerRegistered, WorkerID: spec.ID + "\x00" + turnID, AgentID: spec.ID})
			if !busy {
				s.deliver(s.coordinator.Apply(orchestration.Event{Kind: orchestration.EventManagerFinished}).Actions)
			}
			c.publish(EvInfo, InfoEvent{Info: c.residentInfo()})
		})
	})
	m.SetCompletionObserver(func(completion subagents.ResidentCompletion) bool {
		s.inboxMu.Lock()
		defer s.inboxMu.Unlock()
		if s.closed {
			return false
		}
		done := completion.Completion()
		if !s.tracker.Report(done) {
			return false
		}
		c.logger().Info("subagent finished", "agent", done.AgentID, "turn", done.TurnID, "status", done.Status)
		batch, _ := s.tracker.WaitReady(context.Background())
		return c.postResidentLocked(func() {
			for _, item := range batch {
				s.deliver(s.coordinator.Apply(orchestration.Event{Kind: orchestration.EventWorkerFinished, WorkerID: item.AgentID + "\x00" + item.TurnID, Completion: item}).Actions)
			}
			c.publish(EvInfo, InfoEvent{Info: c.residentInfo()})
		})
	})
	m.SetUpdateObserver(func(string) {
		s.inboxMu.Lock()
		defer s.inboxMu.Unlock()
		if !s.closed {
			s.dirty = true
			s.wakeLocked()
		}
	})
}

func (c *Controller) residentInfo() Info { c.updateInfo(); return c.Info() }

// ResidentListMaxLimit caps one resident_list page; larger limits are clamped.
const ResidentListMaxLimit = 200

// ResidentList exposes the manager's recent-first state projection. Paging
// comes from untrusted clients: a negative offset or limit is rejected, a zero
// limit returns only the total, and limits above ResidentListMaxLimit clamp.
func (c *Controller) ResidentList(ctx context.Context, offset, limit int) ([]subagents.ResidentSnapshot, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if offset < 0 || limit < 0 {
		return nil, 0, errors.New("resident list: offset and limit must not be negative")
	}
	limit = min(limit, ResidentListMaxLimit)
	m := c.deps.ResidentManager
	if m == nil {
		return nil, 0, errors.New("resident manager unavailable")
	}
	rows, total := m.RecentSnapshotPage(offset, limit)
	return rows, total, ctx.Err()
}

// ResidentHistory reads through the manager-owned root, never a client path.
func (c *Controller) ResidentHistory(ctx context.Context, childID, olderCursor string, limit int) (subagents.ResidentHistoryPage, error) {
	if err := ctx.Err(); err != nil {
		return subagents.ResidentHistoryPage{}, err
	}
	m := c.deps.ResidentManager
	if m == nil {
		return subagents.ResidentHistoryPage{}, errors.New("resident manager unavailable")
	}
	page, err := m.HistoryPage(childID, olderCursor, limit)
	if err != nil {
		return subagents.ResidentHistoryPage{}, err
	}
	return page, ctx.Err()
}
