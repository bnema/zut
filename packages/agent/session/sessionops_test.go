package session

import (
	"context"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/providermocks"
	"github.com/stretchr/testify/mock"
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
	c.SetAttach(ctx, AttachStateAttached, "")
	require.Equal(t, AttachStateAttached, c.Info().Attach)
	ev := nextEvent(t, ctx, events, EvAttach)
	require.Equal(t, AttachEvent{State: AttachStateAttached}, ev.Data)
	c.SetAttach(ctx, AttachStateDetached, "client_detached")
	require.Equal(t, AttachStateDetached, c.Info().Attach)
	ev = nextEvent(t, ctx, events, EvAttach)
	require.Equal(t, AttachEvent{State: AttachStateDetached, Reason: "client_detached"}, ev.Data)
}

func TestSetAttachDetachDeniesPendingConfirmation(t *testing.T) {
	c, ctx := newTestController(t)
	c.SetAttach(ctx, AttachStateAttached, "")
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	confirmer, _ := c.HookBindings()
	decided := make(chan core.ConfirmDecision, 1)
	go func() { decided <- confirmer.ConfirmToolCall(core.ToolCallConfirmation{Name: "bash", Summary: "rm"}) }()
	nextEvent(t, ctx, events, EvPending)
	c.SetAttach(ctx, AttachStateDetached, "client_disconnected")
	select {
	case d := <-decided:
		require.False(t, d.Allow)
		require.Contains(t, d.Reason, "user detached")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	nextEvent(t, ctx, events, EvDeferred)
}

func TestSetAttachOnActorForm(t *testing.T) {
	c, ctx := newTestController(t)
	require.NoError(t, c.call(ctx, func() { c.setAttachOnActor(AttachStateAttached, "") }))
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
	go func() {
		_ = c.Do(ctx, func(actx context.Context) {
			require.True(t, c.OnActor(actx))
			returned <- c.Close(actx, "self")
		})
	}()
	select {
	case err := <-returned:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Close on actor blocked")
	}
	<-c.Done()
}

func TestDaemonContextEndClosesAsDaemonStop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	life, stop := context.WithCancel(ctx)
	c := NewController(life, Deps{})
	c.SetAttach(ctx, AttachStateAttached, "")
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	confirmer, _ := c.HookBindings()
	decided := make(chan core.ConfirmDecision, 1)
	go func() { decided <- confirmer.ConfirmToolCall(core.ToolCallConfirmation{Name: "bash", Summary: "x"}) }()
	nextEvent(t, ctx, events, EvPending)
	// Detach so the pending request is recorded for review on close.
	c.SetAttach(ctx, AttachStateDetached, "")
	require.False(t, (<-decided).Allow)
	nextEvent(t, ctx, events, EvDeferred)
	go func() { decided <- confirmer.ConfirmToolCall(core.ToolCallConfirmation{Name: "bash", Summary: "y"}) }()
	stop()
	ev := nextEvent(t, ctx, events, EvClosed)
	require.Equal(t, ClosedEvent{Reason: "daemon_stop"}, ev.Data)
	<-c.Done()
	require.False(t, (<-decided).Allow)
}

func TestCloseMidTurnWaitsForTurnGoroutine(t *testing.T) {
	c, ctx := newTestController(t)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	proceed := make(chan struct{})
	returned := make(chan struct{})
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("openai").Maybe()
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(sctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
		close(started)
		<-sctx.Done()
		close(cancelled)
		// The agent keeps going after cancellation until the test lets it.
		<-proceed
		close(returned)
		return nil, sctx.Err()
	}).Once()
	// The agent may retry once more with the cancelled context.
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(sctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
		return nil, sctx.Err()
	}).Maybe()
	ag := core.NewAgent(client, "m", "", nil)
	// A preset title keeps title generation (another Stream call) out of the test.
	require.NoError(t, c.call(ctx, func() { c.turn.agent = ag; c.settings.title = "t" }))
	require.NoError(t, c.Submit(ctx, Input{Text: "hi"}))
	<-started
	closed := make(chan error, 1)
	go func() { closed <- c.Close(ctx, "user_exit") }()
	<-cancelled
	select {
	case <-closed:
		t.Fatal("Close returned while the turn goroutine was still running")
	default:
	}
	close(proceed)
	require.NoError(t, <-closed)
	select {
	case <-returned:
	default:
		t.Fatalf("Close returned before the turn goroutine finished (turnDone set: %v)", c.turnDone != nil)
	}
}

func TestDefaultIDsAreRandom(t *testing.T) {
	a, _ := newTestController(t)
	b, _ := newTestController(t)
	require.Len(t, a.Info().ID, 12)
	require.NotEqual(t, a.Info().ID, b.Info().ID)
}

func TestSetAttachWithActorContext(t *testing.T) {
	c, ctx := newTestController(t)
	var escaped context.Context
	require.NoError(t, c.Do(ctx, func(actx context.Context) {
		c.SetAttach(actx, AttachStateAttached, "")
		escaped = actx
	}))
	require.Equal(t, AttachStateAttached, c.Info().Attach)
	// The actor context is cancelled once Do returns and no longer counts.
	require.Error(t, escaped.Err())
	require.False(t, c.OnActor(escaped))
}

func TestCloseDuringCompactionWaitsForIt(t *testing.T) {
	c, ctx := newTestController(t)
	started := make(chan struct{})
	ag := blockingAgent(t, started)
	require.NoError(t, c.call(ctx, func() { c.turn.agent = ag; c.settings.title = "t" }))
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	require.NoError(t, c.Compact(ctx))
	<-started
	require.NoError(t, c.Close(ctx, "user_exit"))
	select {
	case <-c.compactDone:
	default:
		t.Fatal("Close returned before the compaction goroutine finished")
	}
	nextEvent(t, ctx, events, EvClosed)
}
