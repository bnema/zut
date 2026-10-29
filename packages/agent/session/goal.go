package session

import (
	"context"
	"errors"
	"github.com/bnema/zut/packages/core"
)

type goalState struct{}

func (s *goalState) snapshotInto(out *Snapshot) {}

// SetGoal is reserved for the goal cluster implementation.
func (c *Controller) SetGoal(ctx context.Context, command GoalCommand) error {
	return errors.New("unimplemented")
}
func (c *Controller) goalAfterTurn(end core.EvTurnEnd) {}
func (c *Controller) goalIdle() bool                   { return true }
func (s *goalState) observe(ev core.AgentEvent)        {}
