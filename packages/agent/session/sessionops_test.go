package session

import (
	"context"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/stretchr/testify/require"
)

func nextEvent(t *testing.T, ctx context.Context, events <-chan Event, kind EventKind) Event {
	t.Helper()
	for {
		select {
		case ev, ok := <-events:
			require.True(t, ok, "subscription closed before %s", kind)
			if ev.Kind == kind {
				return ev
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestSetAttachPublishesAndUpdatesInfo(t *testing.T) {
	c, ctx := newTestController(t)
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	c.SetAttach(AttachStateAttached, "")
	require.Equal(t, AttachStateAttached, c.Info().Attach)
	ev := nextEvent(t, ctx, events, EvAttach)
	require.Equal(t, AttachEvent{State: AttachStateAttached}, ev.Data)
	c.SetAttach(AttachStateDetached, "client_detached")
	require.Equal(t, AttachStateDetached, c.Info().Attach)
	ev = nextEvent(t, ctx, events, EvAttach)
	require.Equal(t, AttachEvent{State: AttachStateDetached, Reason: "client_detached"}, ev.Data)
}

func TestSetAttachDetachDeniesPendingConfirmation(t *testing.T) {
	c, ctx := newTestController(t)
	c.SetAttach(AttachStateAttached, "")
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	confirmer, _ := c.HookBindings()
	decided := make(chan core.ConfirmDecision, 1)
	go func() { decided <- confirmer.ConfirmToolCall(core.ToolCallConfirmation{Name: "bash", Summary: "rm"}) }()
	nextEvent(t, ctx, events, EvPending)
	c.SetAttach(AttachStateDetached, "client_disconnected")
	select {
	case d := <-decided:
		require.False(t, d.Allow)
		require.Contains(t, d.Reason, "user detached")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	nextEvent(t, ctx, events, EvDeferred)
}

func TestSetAttachFromActorDoesNotDeadlock(t *testing.T) {
	c, ctx := newTestController(t)
	require.NoError(t, c.call(ctx, func() { c.SetAttach(AttachStateAttached, "") }))
	require.Equal(t, AttachStateAttached, c.Info().Attach)
}

func TestClosePublishesClosedAndIsIdempotent(t *testing.T) {
	c, ctx := newTestController(t)
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	require.NoError(t, c.Close(ctx, "user_exit"))
	ev := nextEvent(t, ctx, events, EvClosed)
	require.Equal(t, ClosedEvent{Reason: "user_exit"}, ev.Data)
	select {
	case <-c.Done():
	default:
		t.Fatal("Close returned before Done")
	}
	require.NoError(t, c.Close(ctx, "again"))
	require.ErrorIs(t, c.Submit(ctx, Input{Text: "x"}), ErrClosed)
}

func TestCloseFromActorReturnsWithoutWaiting(t *testing.T) {
	c, ctx := newTestController(t)
	returned := make(chan error, 1)
	require.NoError(t, c.post(ctx, func() { returned <- c.Close(context.Background(), "self") }))
	select {
	case err := <-returned:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Close on actor blocked")
	}
	<-c.Done()
}

func TestOptionsPresetIDAndInterruptedTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c := NewControllerWithOptions(ctx, Deps{}, Options{ID: "restored-1", InterruptedTurn: true})
	t.Cleanup(func() { cancel(); <-c.Done() })
	info := c.Info()
	require.Equal(t, "restored-1", info.ID)
	require.Equal(t, RunStateIdle, info.Run)
	require.Equal(t, "interrupted", info.Activity.Kind)
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	ev := nextEvent(t, ctx, events, EvNote)
	require.Equal(t, NoteEvent{Note: interruptedNote}, ev.Data)
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Contains(t, snap.Notes, interruptedNote)
}

func TestDefaultIDsAreRandom(t *testing.T) {
	a, _ := newTestController(t)
	b, _ := newTestController(t)
	require.Len(t, a.Info().ID, 12)
	require.NotEqual(t, a.Info().ID, b.Info().ID)
}
