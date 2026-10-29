package session

import (
	"context"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/providermocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func turnSeams(t *testing.T, c *Controller, ctx context.Context) {
	t.Helper()
	compact := newMockcompactSeam(t)
	compact.EXPECT().maybeAutoCompact(mock.Anything).Return(false).Maybe()
	goal := newMockgoalSeam(t)
	goal.EXPECT().goalBeforeTurn(mock.Anything).Return().Maybe()
	goal.EXPECT().goalAfterTurn(mock.Anything).Return().Maybe()
	goal.EXPECT().goalOnAbort().Return().Maybe()
	confirm := newMockconfirmSeam(t)
	confirm.EXPECT().denyAllPending("turn cancelled").Return().Maybe()
	require.NoError(t, c.call(ctx, func() { c.compactSeam = compact; c.goalSeam = goal; c.confirmSeam = confirm }))
}
func TestTurnStreamAndQueue(t *testing.T) {
	c, ctx := newTestController(t)
	turnSeams(t, c, ctx)
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("test").Maybe()
	first := make(chan provider.Event, 8)
	second := make(chan provider.Event, 8)
	started := make(chan struct{}, 2)
	count := 0
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
		count++
		started <- struct{}{}
		if count == 1 {
			return first, nil
		}
		return second, nil
	}).Twice()
	ag := core.NewAgent(client, "test", "", nil)
	require.NoError(t, c.call(ctx, func() { c.turn.agent = ag }))
	clientCtx, cancel := context.WithCancel(ctx)
	require.NoError(t, c.Submit(clientCtx, Input{Text: "first"}))
	cancel()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.NoError(t, c.Submit(ctx, Input{Text: "second", Mode: "queue"}))
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Len(t, snap.Queue, 1)
	first <- provider.EventTextDelta{Delta: "hel"}
	first <- provider.EventTextDelta{Delta: "lo"}
	first <- provider.EventUsage{Usage: provider.Usage{InputTokens: 12, OutputTokens: 2}}
	first <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "hello"}}}}
	close(first)
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	snap, err = c.Snapshot(ctx)
	require.NoError(t, err)
	events, err := c.Subscribe(ctx, snap.Seq)
	require.NoError(t, err)
	second <- provider.EventTextDelta{Delta: "world"}
	second <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "world"}}}}
	close(second)
	seq := snap.Seq
	done := false
	for !done {
		select {
		case e := <-events:
			seq++
			require.Equal(t, seq, e.Seq)
			if e.Kind == EvInfo && e.Data.(InfoEvent).Info.Run == RunStateIdle {
				done = true
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	snap, err = c.Snapshot(ctx)
	require.NoError(t, err)
	require.Empty(t, snap.Queue)
	require.Equal(t, "world", snap.Live.AssistantText)
	require.Equal(t, 12, snap.Info.ContextUsed)
}
func TestTurnQueueEditingAndAbort(t *testing.T) {
	c, ctx := newTestController(t)
	turnSeams(t, c, ctx)
	require.NoError(t, c.call(ctx, func() { c.turn.busy = true }))
	require.NoError(t, c.Submit(ctx, Input{Text: "a"}))
	require.NoError(t, c.Submit(ctx, Input{Text: "b"}))
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Len(t, snap.Queue, 2)
	require.NoError(t, c.CancelQueued(ctx, snap.Queue[0].ID))
	in, err := c.SlideBackQueued(ctx)
	require.NoError(t, err)
	require.Equal(t, "b", in.Text)
	_, err = c.SlideBackQueued(ctx)
	require.ErrorIs(t, err, ErrUnknownRequest)
	require.NoError(t, c.Abort(ctx))
}
func TestTurnToolProjection(t *testing.T) {
	c, ctx := newTestController(t)
	require.NoError(t, c.call(ctx, func() {
		for _, e := range []core.AgentEvent{core.EvToolUseStart{ID: "x", Name: "bash"}, core.EvToolUseArgs{ID: "x", Delta: `{}`}, core.EvToolUseEnd{ID: "x"}, core.EvToolCall{ID: "x", Name: "bash", Args: []byte(`{}`)}, core.EvToolExecutionStarted{ID: "x", Name: "bash"}, core.EvToolProgress{ID: "x", Text: "ok"}, core.EvToolResult{ID: "x", Result: core.ToolResult{}}} {
			c.applyAgentEvent(e)
		}
	}))
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Len(t, snap.Live.Tools, 1)
	require.Equal(t, "done", snap.Live.Tools[0].State)
	require.Equal(t, "ok", snap.Live.Tools[0].Progress)
}

func TestTurnSteerAndCancelProvider(t *testing.T) {
	c, ctx := newTestController(t)
	turnSeams(t, c, ctx)
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("test").Maybe()
	started := make(chan context.Context, 1)
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(run context.Context, _ provider.Request) (<-chan provider.Event, error) {
		started <- run
		return make(chan provider.Event), nil
	}).Once()
	ag := core.NewAgent(client, "test", "", nil)
	require.NoError(t, c.call(ctx, func() { c.turn.agent = ag }))
	require.NoError(t, c.Submit(ctx, Input{Text: "run"}))
	var run context.Context
	select {
	case run = <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.NoError(t, c.Submit(ctx, Input{Text: "steer", Mode: "steer", HostEvent: true}))
	pending := ag.PendingQueuedMessages()
	require.Len(t, pending, 1)
	require.True(t, pending[0].HostEvent)
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	require.NoError(t, c.Abort(ctx))
	select {
	case <-run.Done():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for {
		select {
		case e := <-events:
			if e.Kind == EvInfo && e.Data.(InfoEvent).Info.Run == RunStateIdle {
				return
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
