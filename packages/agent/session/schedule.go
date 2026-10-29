package session

import "github.com/bnema/zut/packages/agent/scheduler"

type schedState struct{}

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
