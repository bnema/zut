package session

import (
	"github.com/bnema/zut/packages/agent/scheduler"
	"github.com/bnema/zut/packages/core"
)

type schedState struct {
	// guardAgent is the agent whose repetition guard a running scheduled
	// turn disabled; finishTurn restores it.
	guardAgent *core.Agent
}

func (s *schedState) snapshotInto(out *Snapshot) {}

// deliverScheduled is actor-only. The turn cluster's FIFO owns idle draining;
// scheduled work never steers or interrupts a running turn.
func (c *Controller) deliverScheduled(task scheduler.Task) {
	c.turnSeam.enqueue(Input{Origin: InputOriginScheduler, Mode: SubmitModeQueue, Text: task.Message})
}
func (c *Controller) scheduleCount() int {
	if c.deps.Scheduler == nil {
		return 0
	}
	return len(c.deps.Scheduler.List())
}

// applyScheduledGuard disables the repetition guard for a scheduled turn, as
// the in-process scheduler did: scheduled work is deliberate and unattended,
// so a guard stop would silently drop it. An operator opt-out is kept.
func (c *Controller) applyScheduledGuard(ag *core.Agent, origin InputOrigin) {
	if origin != InputOriginScheduler || ag == nil || ag.DisableRepetitionGuard {
		return
	}
	ag.DisableRepetitionGuard = true
	c.sched.guardAgent = ag
}

// restoreScheduledGuard re-enables the guard a scheduled turn disabled.
func (c *Controller) restoreScheduledGuard() {
	if c.sched.guardAgent != nil {
		c.sched.guardAgent.DisableRepetitionGuard = false
		c.sched.guardAgent = nil
	}
}
