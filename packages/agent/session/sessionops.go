package session

import (
	"context"
	"errors"
)

type opsState struct{}

func (s *opsState) snapshotInto(out *Snapshot) {}

// Close is reserved for the lifecycle implementation lane.
func (c *Controller) Close(ctx context.Context, reason string) error {
	return errors.New("unimplemented")
}

// SetAttach is reserved for the lifecycle implementation lane.
func (c *Controller) SetAttach(state AttachState, reason string) {}

// Idle reports the session-owned parts of the D7 eligibility predicate.
func (c *Controller) Idle() bool {
	var idle bool
	err := c.call(context.Background(), func() {
		idle = !c.turnSeam.isBusy() && c.schedSeam.scheduleCount() == 0 && c.goalSeam.goalIdle() && !c.residentSeam.residentsRunning()
	})
	return err == nil && idle
}
