package session

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type residentObservers struct {
	accepted  func(subagents.ResidentChildSpec, string, string)
	completed func(subagents.ResidentCompletion) bool
	update    func(string)
}

func newGatedResidentController(t *testing.T) (*Controller, context.CancelFunc, *residentObservers, chan Input, func() func()) {
	m := newMockResidentHostLocal(t)
	obs := &residentObservers{}
	m.EXPECT().SetAcceptedObserver(mock.Anything).Run(func(f func(subagents.ResidentChildSpec, string, string)) { obs.accepted = f })
	m.EXPECT().SetCompletionObserver(mock.Anything).Run(func(f func(subagents.ResidentCompletion) bool) { obs.completed = f })
	m.EXPECT().SetUpdateObserver(mock.Anything).Run(func(f func(string)) { obs.update = f })
	m.EXPECT().Snapshot().Return(nil).Maybe()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c := NewController(ctx, Deps{ResidentManager: m})
	t.Cleanup(func() { cancel(); <-c.Done() })
	turn := newMockturnSeam(t)
	turn.EXPECT().isBusy().Return(false).Maybe()
	turn.EXPECT().turnOrigin().Return(InputOriginUser).Maybe()
	turn.EXPECT().turnInfo().Return(turnInfoProjection{}).Maybe()
	delivered := make(chan Input, 512)
	turn.EXPECT().enqueue(mock.Anything).Run(func(in Input) { delivered <- in }).Maybe()
	require.NoError(t, c.call(ctx, func() { c.turnSeam = turn }))
	gate := func() func() {
		entered, release := make(chan struct{}), make(chan struct{})
		require.NoError(t, c.post(ctx, func() { close(entered); <-release }))
		<-entered
		return func() { close(release) }
	}
	return c, cancel, obs, delivered, gate
}

func TestResidentShutdownDrainsAcceptedCompletion(t *testing.T) {
	c, cancel, obs, delivered, gate := newGatedResidentController(t)
	release := gate()
	obs.accepted(subagents.ResidentChildSpec{ID: "child"}, "turn", "task")
	obs.update("child")
	require.True(t, obs.completed(subagents.ResidentCompletion{ChildID: "child", TurnID: "turn", Summary: "done"}))
	cancel()
	release()
	<-c.Done()
	require.Len(t, delivered, 1)
	require.Contains(t, (<-delivered).Text, "done")
	require.False(t, obs.completed(subagents.ResidentCompletion{ChildID: "child", TurnID: "turn", Summary: "late"}))
	// GO-005: acceptance after shutdown must not grow the tracker.
	obs.accepted(subagents.ResidentChildSpec{ID: "late"}, "turn", "task")
	require.True(t, c.res.tracker.TrackResident("late", "turn"), "post-shutdown acceptance was tracked")
	c.res.inboxMu.Lock()
	defer c.res.inboxMu.Unlock()
	require.Nil(t, c.res.inbox)
	require.False(t, c.res.dirty)
}

func TestResidentMailboxBoundedAndUpdatesCoalesce(t *testing.T) {
	c, _, obs, delivered, gate := newGatedResidentController(t)
	release := gate()
	for n := 0; n < residentMailboxCap; n++ {
		obs.accepted(subagents.ResidentChildSpec{ID: "c" + strconv.Itoa(n)}, "turn", "task")
	}
	for n := 0; n < 1000; n++ {
		obs.update("c0")
	}
	c.res.inboxMu.Lock()
	require.Len(t, c.res.inbox, residentMailboxCap)
	require.True(t, c.res.dirty)
	c.res.inboxMu.Unlock()
	// Overflowing acceptance stays untracked; overflowing completion is refused.
	obs.accepted(subagents.ResidentChildSpec{ID: "overflow"}, "turn", "task")
	require.True(t, c.res.tracker.TrackResident("overflow", "turn"))
	require.False(t, obs.completed(subagents.ResidentCompletion{ChildID: "c1", TurnID: "turn", Summary: "done"}))
	release()
	require.Eventually(t, func() bool {
		c.res.inboxMu.Lock()
		defer c.res.inboxMu.Unlock()
		return len(c.res.inbox) == 0 && !c.res.dirty
	}, 5*time.Second, time.Millisecond)
	// The refused completion is retryable once capacity returns.
	require.True(t, obs.completed(subagents.ResidentCompletion{ChildID: "c1", TurnID: "turn", Summary: "done"}))
	select {
	case in := <-delivered:
		require.Contains(t, in.Text, "done")
	case <-time.After(5 * time.Second):
		t.Fatal("completion not delivered")
	}
}
