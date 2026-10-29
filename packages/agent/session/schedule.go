package session

import (
	"github.com/bnema/zut/packages/agent/scheduler"
)

type schedState struct{}

func (s *schedState) snapshotInto(out *Snapshot)           {}
func (c *Controller) deliverScheduled(task scheduler.Task) {}
func (c *Controller) scheduleCount() int                   { return 0 }
