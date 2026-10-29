package session

import (
	"context"
	"errors"
	"github.com/bnema/zut/packages/core"
)

type turnState struct{}

func (s *turnState) snapshotInto(out *Snapshot) {}

// Submit is reserved for the turn cluster implementation.
func (c *Controller) Submit(ctx context.Context, in Input) error { return errors.New("unimplemented") }

// Abort is reserved for the turn cluster implementation.
func (c *Controller) Abort(ctx context.Context) error { return errors.New("unimplemented") }

// CancelQueued is reserved for the turn cluster implementation.
func (c *Controller) CancelQueued(ctx context.Context, id string) error {
	return errors.New("unimplemented")
}

// SlideBackQueued is reserved for the turn cluster implementation.
func (c *Controller) SlideBackQueued(ctx context.Context) (Input, error) {
	return Input{}, errors.New("unimplemented")
}
func (c *Controller) startTurn(in Input) error { return errors.New("unimplemented") }
func (c *Controller) enqueue(in Input)         {}
func (c *Controller) applyAgentEvent(ev core.AgentEvent) {
	c.goal.observe(ev)
	c.compact.observe(ev)
	c.res.observe(ev)
}
func (c *Controller) isBusy() bool            { return false }
func (c *Controller) turnOrigin() InputOrigin { return "" }
func (c *Controller) cancelTurn()             {}
