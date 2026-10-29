package session

import (
	"context"
	"errors"
	"github.com/bnema/zut/packages/core"
)

type residentState struct{}

func (s *residentState) snapshotInto(out *Snapshot) {}

// ResidentAction is reserved for the residents cluster implementation.
func (c *Controller) ResidentAction(ctx context.Context, command ResidentCommand) error {
	return errors.New("unimplemented")
}
func (c *Controller) residentsRunning() bool               { return false }
func (c *Controller) residentSummaries() []ResidentSummary { return nil }
func (s *residentState) observe(ev core.AgentEvent)        {}

// residentsInit wires resident observers; implemented by the residents cluster.
func (c *Controller) residentsInit() {}

func (c *Controller) residentsRunStarted() {}
func (c *Controller) residentsRunEnded()   {}
