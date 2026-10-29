package session

import (
	"context"
	"errors"
	"github.com/bnema/zut/packages/core"
)

type compactState struct{}

func (s *compactState) snapshotInto(out *Snapshot) {}

// Compact is reserved for the compaction cluster implementation.
func (c *Controller) Compact(ctx context.Context) error { return errors.New("unimplemented") }
func (c *Controller) maybeAutoCompact(next Input) bool  { return false }
func (s *compactState) observe(ev core.AgentEvent)      {}
