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

// Inputs stacked during a turn run as one follow-up turn, not one turn each.
func TestQueuedUserInputsRunAsOneTurn(t *testing.T) {
	c, ctx := newTestController(t)
	turnSeams(t, c, ctx)
	residents := newMockresidentSeam(t)
	residents.EXPECT().residentsRunStarted().Return().Twice()
	residents.EXPECT().residentsRunEnded().Return().Twice()
	residents.EXPECT().residentSummaries().Return(nil).Maybe()
	require.NoError(t, c.call(ctx, func() { c.residentSeam = residents }))

	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("test").Maybe()
	first := make(chan provider.Event, 4)
	second := make(chan provider.Event, 4)
	started := make(chan provider.Request, 2)
	count := 0
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
		count++
		started <- req
		if count == 1 {
			return first, nil
		}
		return second, nil
	}).Twice()
	ag := core.NewAgent(client, "test", "", nil)
	require.NoError(t, c.call(ctx, func() { c.turn.agent = ag }))

	require.NoError(t, c.Submit(ctx, Input{Text: "first"}))
	<-started
	for _, text := range []string{"a", "b", "c"} {
		require.NoError(t, c.Submit(ctx, Input{Text: text, Mode: SubmitModeQueue}))
	}
	first <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "ok"}}}}
	close(first)

	var req provider.Request
	select {
	case req = <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	last := req.Messages[len(req.Messages)-1]
	require.Equal(t, provider.RoleUser, last.Role)
	var texts []string
	for _, block := range last.Content {
		if tb, ok := block.(provider.TextBlock); ok {
			texts = append(texts, tb.Text)
		}
	}
	require.Contains(t, texts, "a\n\nb\n\nc")
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Empty(t, snap.Queue)
	events, err := c.Subscribe(ctx, snap.Seq)
	require.NoError(t, err)
	second <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "done"}}}}
	close(second)
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
