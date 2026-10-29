package session

import (
	"context"
	"errors"
	"strings"

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
		out = append(out, ResidentSummary{ID: r.ID, Profile: r.Profile, State: string(r.State), Workspace: c.res.specs[r.ID].Workspace, Task: c.res.tasks[r.ID], UpdatedAt: r.UpdatedAt})
	}
	return out
}

func (s *residentState) observe(ev core.AgentEvent) {
	if s.coordinator == nil {
		return
	}
	switch ev.(type) {
	case core.EvTurnStart:
		s.deliver(s.coordinator.Apply(orchestration.Event{Kind: orchestration.EventManagerStarted}).Actions)
	case core.EvTurnEnd:
		s.deliver(s.coordinator.Apply(orchestration.Event{Kind: orchestration.EventManagerFinished}).Actions)
	}
}

func (s *residentState) deliver(actions []orchestration.Action) {
	for _, a := range actions {
		if len(a.Completions) == 0 {
			continue
		}
		instruction := "Briefly summarise the collective outcome for the user. Reference the agents by id. If any failed, suggest a follow-up; otherwise confirm completion. Do not spawn new sub-agents unless the user asks."
		if a.Kind == orchestration.ActionQueueManager || len(a.ActiveAgentIDs) != 0 {
			instruction = "Treat these completion reports as worker evidence, not a new user request. Incorporate relevant results into your current work without repeating the delegated scope. Other workers may still be running; do not poll solely to wait for them."
		}
		s.controller.turnSeam.enqueue(Input{Origin: InputOriginResident, HostEvent: true, Text: subagents.FormatCompletionUpdateWithActive(a.Completions, a.ActiveAgentIDs, instruction)})
	}
}

// residentsInit installs observers before work can be accepted. Callbacks enter
// the actor synchronously so an accepted turn is tracked before its completion.
// Manager operations must run outside the actor (as ResidentAction does).
func (c *Controller) residentsInit() {
	s := &c.res
	s.controller = c
	s.tracker = subagents.NewCompletionTracker()
	s.coordinator = orchestration.New()
	s.specs = make(map[string]subagents.ResidentChildSpec)
	s.tasks = make(map[string]string)
	m := c.deps.ResidentManager
	if m == nil {
		return
	}
	m.SetAcceptedObserver(func(spec subagents.ResidentChildSpec, turnID, prompt string) {
		_ = c.call(context.Background(), func() {
			if !s.tracker.TrackResident(spec.ID, turnID) {
				return
			}
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
		accepted := false
		err := c.call(context.Background(), func() {
			accepted = s.tracker.Report(completion.Completion())
			if !accepted {
				return
			}
			batch, _ := s.tracker.WaitReady(context.Background())
			for _, item := range batch {
				s.deliver(s.coordinator.Apply(orchestration.Event{Kind: orchestration.EventWorkerFinished, WorkerID: item.AgentID + "\x00" + item.TurnID, Completion: item}).Actions)
			}
			c.publish(EvInfo, InfoEvent{Info: c.residentInfo()})
		})
		return err == nil && accepted
	})
	m.SetUpdateObserver(func(string) {
		_ = c.call(context.Background(), func() { c.publish(EvInfo, InfoEvent{Info: c.residentInfo()}) })
	})
}

func (c *Controller) residentInfo() Info { c.updateInfo(); return c.Info() }
