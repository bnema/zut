package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ToolRefreshHarness lets external tests (which own sessionmocks) drive the
// actor-only busy state around RequestToolRefresh.
type ToolRefreshHarness struct {
	C   *Controller
	Ctx context.Context
}

func NewToolRefreshHarness(t *testing.T, deps Deps) *ToolRefreshHarness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c := NewController(ctx, deps)
	t.Cleanup(func() { cancel(); <-c.Done() })
	return &ToolRefreshHarness{C: c, Ctx: ctx}
}

// SetBusy marks a turn running or ended; ending drains like finishTurn does.
func (h *ToolRefreshHarness) SetBusy(t *testing.T, busy bool) {
	require.NoError(t, h.C.call(h.Ctx, func() {
		h.C.turn.busy = busy
		if !busy {
			h.C.drainQueued()
		}
	}))
}

// BlockActor occupies the actor until the returned release is called, so
// requests made meanwhile queue up deterministically.
func (h *ToolRefreshHarness) BlockActor(t *testing.T) (release func()) {
	entered, gate := make(chan struct{}), make(chan struct{})
	require.NoError(t, h.C.post(h.Ctx, func() { close(entered); <-gate }))
	<-entered
	return func() { close(gate) }
}

// Queued reports whether a RequestToolRefresh hand-off is still in flight.
func (h *ToolRefreshHarness) Queued() bool { return h.C.refreshQueued.Load() }

// Pending reports whether a deferred refresh is recorded.
func (h *ToolRefreshHarness) Pending(t *testing.T) bool {
	var dirty bool
	require.NoError(t, h.C.call(h.Ctx, func() { dirty = h.C.settings.toolsDirty }))
	return dirty
}
