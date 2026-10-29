package session

import (
	"context"
	"errors"
	"github.com/bnema/zut/packages/agent/tools"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/providermocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// blockingAgent streams nothing until the compaction context is cancelled.
func blockingAgent(t *testing.T, started chan<- struct{}) *core.Agent {
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("openai").Maybe()
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
		ch := make(chan provider.Event)
		close(started)
		go func() { <-ctx.Done(); close(ch) }()
		return ch, nil
	}).Once()
	ag := core.NewAgent(client, "model", "", nil)
	for range 6 {
		ag.AppendUserContext("original", nil)
	}
	return ag
}

func nextKinds(t *testing.T, ctx context.Context, events <-chan Event, want EventKind) Event {
	t.Helper()
	for {
		select {
		case ev := <-events:
			if ev.Kind == want {
				return ev
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestAbortCompactionFailsWithoutReplacingMessages(t *testing.T) {
	c, ctx := newTestController(t)
	turn := lane9Turn(t, c, ctx)
	started := make(chan struct{})
	ag := blockingAgent(t, started)
	before := ag.Messages()
	turn.EXPECT().currentAgent().Return(ag).Maybe()
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	require.NoError(t, c.Compact(ctx))
	<-started
	require.NoError(t, c.call(ctx, func() { require.True(t, c.compacting()); c.abortCompaction() }))
	ev := nextKinds(t, ctx, events, EvCompaction)
	require.Equal(t, "start", ev.Data.(CompactionEvent).Phase)
	ev = nextKinds(t, ctx, events, EvCompaction)
	require.Equal(t, "failed", ev.Data.(CompactionEvent).Phase)
	require.NoError(t, c.call(ctx, func() { require.False(t, c.compacting()) }))
	require.Equal(t, before, ag.Messages())
	// No note, no message replacement, no queued drain after an explicit abort.
	select {
	case ev := <-events:
		t.Fatalf("unexpected event %s", ev.Kind)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestCompactionCompletionDrainsQueue(t *testing.T) {
	c, ctx := newTestController(t)
	turn := lane9Turn(t, c, ctx)
	turn.EXPECT().currentAgent().Return(lane9Agent(t)).Maybe()
	drained := make(chan struct{})
	turn.EXPECT().drainQueued().RunAndReturn(func() { close(drained) }).Once()
	require.NoError(t, c.Compact(ctx))
	select {
	case <-drained:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestCompactBusyWhileCompacting(t *testing.T) {
	c, ctx := newTestController(t)
	turn := lane9Turn(t, c, ctx)
	started := make(chan struct{})
	turn.EXPECT().currentAgent().Return(blockingAgent(t, started)).Maybe()
	require.NoError(t, c.Compact(ctx))
	<-started
	require.ErrorIs(t, c.Compact(ctx), ErrBusy)
	require.ErrorIs(t, c.SetTitle(ctx, "x"), ErrBusy)
	require.NoError(t, c.call(ctx, c.abortCompaction))
}

func TestRescueTurnDetachedPublishesNote(t *testing.T) {
	c, ctx := newTestController(t)
	lane9Turn(t, c, ctx)
	confirm := newMockconfirmSeam(t)
	confirm.EXPECT().awaitPending(mock.Anything, mock.Anything).Return(nil, ErrNotAttached).Once()
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	require.NoError(t, c.call(ctx, func() {
		c.confirmSeam = confirm
		c.rescueTurn(errors.New("overflow"), Input{Text: "x"})
	}))
	ev := nextKinds(t, ctx, events, EvNote)
	require.Contains(t, ev.Data.(NoteEvent).Note.Text, ErrNotAttached.Error())
}

// Lane9Harness bridges external tests (which own sessionmocks for Deps, since
// package session cannot import it) to generated internal seam mocks.
type Lane9Harness struct {
	C       *Controller
	Ctx     context.Context
	turn    *mockturnSeam
	agent   *core.Agent
	Retried chan Input
}

// NewLane9Harness starts a controller whose TURN seam is a generated mock that
// owns ag; setAgent swaps it like the real TURN cluster when idle.
func NewLane9Harness(t *testing.T, deps Deps, ag *core.Agent) *Lane9Harness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c := NewController(ctx, deps)
	t.Cleanup(func() { cancel(); <-c.Done() })
	h := &Lane9Harness{C: c, Ctx: ctx, agent: ag, Retried: make(chan Input, 1)}
	h.turn = lane9Turn(t, c, ctx)
	// Only the actor touches h.agent.
	h.turn.EXPECT().currentAgent().RunAndReturn(func() *core.Agent { return h.agent }).Maybe()
	h.turn.EXPECT().setAgent(mock.Anything).RunAndReturn(func(a *core.Agent) error { h.agent = a; return nil }).Maybe()
	h.turn.EXPECT().startTurn(mock.Anything).RunAndReturn(func(in Input) error { h.Retried <- in; return nil }).Maybe()
	return h
}

// Agent returns the TURN-owned agent through the actor.
func (h *Lane9Harness) Agent(t *testing.T) *core.Agent {
	var ag *core.Agent
	require.NoError(t, h.C.call(h.Ctx, func() { ag = h.agent }))
	return ag
}

// RescueTurn installs a confirm seam answering value off the actor and starts
// a rescue for retry. The answer first round-trips through the actor with a
// bounded deadline: if awaitPending were called on the loop, this deadlocks
// and the test fails at the deadline instead of hanging.
func (h *Lane9Harness) RescueTurn(t *testing.T, value string, retry Input) {
	confirm := newMockconfirmSeam(t)
	confirm.EXPECT().awaitPending(mock.Anything, mock.MatchedBy(func(r PendingRequest) bool { return r.Kind == "rescue" && r.Rescue != nil })).
		RunAndReturn(func(ctx context.Context, _ PendingRequest) ([]tools.QuestionAnswer, error) {
			dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			if err := h.C.call(dctx, func() {}); err != nil {
				t.Errorf("awaitPending ran on the actor loop: %v", err)
				return nil, err
			}
			return []tools.QuestionAnswer{{Selected: []string{value}}}, nil
		}).Once()
	require.NoError(t, h.C.call(h.Ctx, func() {
		h.C.confirmSeam = confirm
		h.C.rescueTurn(errors.New("rate limited"), retry)
	}))
}
