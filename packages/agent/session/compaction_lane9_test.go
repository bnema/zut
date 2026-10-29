package session

import (
	"context"
	"errors"
	"github.com/bnema/zut/packages/agent/tools"
	"sync/atomic"
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
	// No pending prompt: abort only drains what was already queued.
	turn.EXPECT().drainQueued().Return().Once()
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
	// No note and no message replacement after an explicit abort.
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
	turn.EXPECT().drainQueued().Return().Maybe()
	require.NoError(t, c.Compact(ctx))
	<-started
	require.ErrorIs(t, c.Compact(ctx), ErrBusy)
	require.ErrorIs(t, c.SetTitle(ctx, "x"), ErrBusy)
	require.NoError(t, c.call(ctx, c.abortCompaction))
}

// failingAgent's summary request fails immediately with a provider error.
func failingAgent(t *testing.T) *core.Agent {
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("openai").Maybe()
	client.EXPECT().Stream(mock.Anything, mock.Anything).Return(nil, errors.New("boom")).Once()
	ag := core.NewAgent(client, "model", "", nil)
	for range 6 {
		ag.AppendUserContext("original", nil)
	}
	return ag
}

func TestAbortCompactionRequeuesPromptAndDrains(t *testing.T) {
	c, ctx := newTestController(t)
	turn := lane9Turn(t, c, ctx)
	started := make(chan struct{})
	turn.EXPECT().currentAgent().Return(blockingAgent(t, started)).Maybe()
	next := Input{Text: "pending", Origin: InputOriginUser}
	// The prompt rejoins the queue behind already-queued input, then the queue drains.
	requeued := make(chan struct{})
	turn.EXPECT().enqueue(next).RunAndReturn(func(Input) { close(requeued) }).Once()
	drained := make(chan struct{})
	turn.EXPECT().drainQueued().RunAndReturn(func() {
		select {
		case <-requeued:
		default:
			t.Error("drained before re-queueing the prompt")
		}
		close(drained)
	}).Once()
	require.NoError(t, c.call(ctx, func() { require.NoError(t, c.beginCompact(&next)) }))
	<-started
	require.NoError(t, c.call(ctx, c.abortCompaction))
	select {
	case <-drained:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestFailedCompactionStillSendsPromptThenDrains(t *testing.T) {
	c, ctx := newTestController(t)
	turn := lane9Turn(t, c, ctx)
	turn.EXPECT().currentAgent().Return(failingAgent(t)).Maybe()
	next := Input{Text: "pending", Origin: InputOriginUser}
	sent := make(chan struct{})
	turn.EXPECT().startTurn(next).RunAndReturn(func(Input) error {
		require.True(t, c.compact.skipOnce, "prompt must bypass auto-compaction once")
		close(sent)
		return nil
	}).Once()
	drained := make(chan struct{})
	turn.EXPECT().drainQueued().RunAndReturn(func() { <-sent; close(drained) }).Once()
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	require.NoError(t, c.call(ctx, func() { require.NoError(t, c.beginCompact(&next)) }))
	nextKinds(t, ctx, events, EvCompaction)
	require.Equal(t, "failed", nextKinds(t, ctx, events, EvCompaction).Data.(CompactionEvent).Phase)
	select {
	case <-drained:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestRescueWithoutBuilderSkipsPrompt(t *testing.T) {
	c, ctx := newTestController(t)
	lane9Turn(t, c, ctx)
	// No awaitPending expectation: calling it fails the test.
	confirm := newMockconfirmSeam(t)
	require.NoError(t, c.call(ctx, func() { c.confirmSeam = confirm }))
	cause := errors.New("overflow")
	_, err := c.rescue(ctx, cause)
	require.Same(t, cause, err)
}

func TestTitleGenerationCancelledOnClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cctx, stop := context.WithCancel(ctx)
	c := NewController(cctx, Deps{})
	turn := lane9Turn(t, c, ctx)
	streaming := make(chan struct{})
	var cancelled atomic.Bool
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("openai").Maybe()
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(sctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
		close(streaming)
		<-sctx.Done()
		cancelled.Store(true)
		return nil, sctx.Err()
	}).Once()
	turn.EXPECT().currentAgent().Return(core.NewAgent(client, "m", "", nil)).Maybe()
	require.NoError(t, c.call(ctx, func() { c.maybeGenerateTitle(Input{Text: "hello", Origin: InputOriginUser}) }))
	<-streaming
	stop()
	<-c.Done()
	// Well under the 10s title timeout: only close-driven cancellation passes.
	deadline := time.Now().Add(2 * time.Second)
	for !cancelled.Load() {
		if time.Now().After(deadline) {
			t.Fatal("title generation not cancelled on close")
		}
		time.Sleep(5 * time.Millisecond)
	}
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

// SetConfirm installs a confirm seam whose awaitPending returns (answers, err).
func (h *Lane9Harness) SetConfirm(t *testing.T, answers []tools.QuestionAnswer, err error) {
	confirm := newMockconfirmSeam(t)
	confirm.EXPECT().awaitPending(mock.Anything, mock.MatchedBy(func(r PendingRequest) bool { return r.Kind == "rescue" && r.Rescue != nil })).Return(answers, err).Once()
	require.NoError(t, h.C.call(h.Ctx, func() { h.C.confirmSeam = confirm }))
}

// Rescue runs the shared rescue flow off the actor.
func (h *Lane9Harness) Rescue(cause error) (*core.Agent, error) { return h.C.rescue(h.Ctx, cause) }

// StartRescueTurn starts the turn rescue on the actor.
func (h *Lane9Harness) StartRescueTurn(t *testing.T, cause error, retry Input) {
	require.NoError(t, h.C.call(h.Ctx, func() { h.C.rescueTurn(cause, retry) }))
}
