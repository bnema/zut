package daemon

import (
	"context"
	"time"

	"github.com/bnema/zut/packages/agent/session"
)

// gcLoop closes D7-eligible sessions every GCInterval; IdleGC 0 disables it.
func (d *Daemon) gcLoop(ctx context.Context) {
	if d.opts.IdleGC <= 0 {
		return
	}
	t := time.NewTicker(d.opts.GCInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.collect()
		}
	}
}

// collect closes every eligible session and returns their IDs.
func (d *Daemon) collect() []session.ID {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	if d.isStopping() || d.opts.IdleGC <= 0 {
		return nil
	}
	var closed []session.ID
	now := d.now()
	for _, e := range d.entries() {
		if !d.gcEligible(e, now) {
			continue
		}
		if err := d.closeLocked(e.id, ReasonIdleGC, ""); err != nil {
			d.log.Warn("idle gc close", "session", e.id, "err", err)
		}
		closed = append(closed, e.id)
	}
	return closed
}

// gcEligible is the D7 predicate: detached (no controller), idle with no
// schedules, active goal or running resident (Handle.Idle), and no
// detach or user input for at least IdleGC.
func (d *Daemon) gcEligible(e *entry, now time.Time) bool {
	d.mu.Lock()
	controlled := d.controller[e.id] != nil
	last := e.detachedAt
	d.mu.Unlock()
	if controlled {
		return false
	}
	info := e.h.Info()
	if info.Attach == session.AttachStateAttached {
		return false
	}
	if info.LastUserInputAt.After(last) {
		last = info.LastUserInputAt
	}
	if now.Sub(last) < d.opts.IdleGC {
		return false
	}
	return e.h.Idle()
}
