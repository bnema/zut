package session

import (
	"context"
	"errors"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/providermocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func lane9Agent(t *testing.T) *core.Agent {
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("openai").Maybe()
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
		ch := make(chan provider.Event, 2)
		ch <- provider.EventTextDelta{Delta: "summary"}
		ch <- provider.EventDone{}
		close(ch)
		return ch, nil
	}).Once()
	ag := core.NewAgent(client, "model", "", nil)
	for range 6 {
		ag.AppendUserContext("original", nil)
	}
	return ag
}
func TestCompactionReplacesMessagesAndContinuesPrompt(t *testing.T) {
	c, ctx := newTestController(t)
	turn := lane9Turn(t, c, ctx)
	ag := lane9Agent(t)
	next := Input{Text: "pending", Origin: InputOriginUser, Images: []provider.ImageBlock{{Data: []byte("image")}}}
	turn.EXPECT().currentAgent().Return(ag).Maybe()
	continued := make(chan Input, 1)
	turn.EXPECT().startTurn(next).RunAndReturn(func(in Input) error { continued <- in; return nil }).Once()
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	require.NoError(t, c.call(ctx, func() { require.NoError(t, c.beginCompact(&next)) }))
	select {
	case got := <-continued:
		require.Equal(t, next, got)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.Less(t, ag.MessageCount(), 6)
	var phases []string
	for {
		select {
		case ev := <-events:
			if ev.Kind == EvCompaction {
				phases = append(phases, ev.Data.(CompactionEvent).Phase)
			}
			if ev.Kind == EvMessagesReplaced {
				require.Equal(t, []string{"start", "done"}, phases)
				return
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
func TestRescueDetachedFails(t *testing.T) {
	c, ctx := newTestController(t)
	lane9Turn(t, c, ctx)
	confirm := newMockconfirmSeam(t)
	confirm.EXPECT().awaitPending(mock.Anything, mock.MatchedBy(func(req PendingRequest) bool { return req.Kind == "rescue" && req.Rescue.Error == "overflow" })).Return(nil, ErrNotAttached).Once()

	require.NoError(t, c.call(ctx, func() { c.confirmSeam = confirm }))
	_, err := c.rescue(ctx, errors.New("overflow"))
	require.ErrorIs(t, err, ErrNotAttached)
}
func TestCompactMissingAgent(t *testing.T) {
	c, ctx := newTestController(t)
	turn := lane9Turn(t, c, ctx)
	turn.EXPECT().currentAgent().Return(nil).Once()
	require.EqualError(t, c.Compact(ctx), "agent unavailable")
}

func TestAutoCompactionPreservesPrompt(t *testing.T) {
	c, ctx := newTestController(t)
	turn := lane9Turn(t, c, ctx, turnInfoProjection{ContextUsed: 90, ContextMax: 100})
	ag := lane9Agent(t)
	turn.EXPECT().currentAgent().Return(ag).Maybe()
	// A resident input does not trigger the unrelated title-generation request.
	in := Input{Text: "continue work", Origin: InputOriginResident}
	done := make(chan struct{})
	turn.EXPECT().startTurn(in).RunAndReturn(func(Input) error { close(done); return nil }).Once()
	require.NoError(t, c.call(ctx, func() { require.True(t, c.maybeAutoCompact(in)) }))
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.NoError(t, c.call(ctx, func() { require.False(t, c.maybeAutoCompact(in)) }))
}
