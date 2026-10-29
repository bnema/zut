package session

import (
	"context"
	"errors"
	"testing"

	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/providermocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// rescueController wires real TURN/COMPACT with a strict goal mock: every
// goalAfterTurn call must be expected by the test.
func rescueController(t *testing.T) (*Controller, context.Context, *mockgoalSeam, *mockconfirmSeam) {
	t.Helper()
	c, ctx := newTestController(t)
	goal := newMockgoalSeam(t)
	goal.EXPECT().goalBeforeTurn(mock.Anything).Return().Maybe()
	confirm := newMockconfirmSeam(t)
	require.NoError(t, c.call(ctx, func() {
		c.goalSeam, c.confirmSeam = goal, confirm
		c.settings.title = "t"
	}))
	return c, ctx, goal, confirm
}

// rateLimitedAgent fails its single stream with a rescue-eligible error.
func rateLimitedAgent(t *testing.T) *core.Agent {
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("openai").Maybe()
	client.EXPECT().Stream(mock.Anything, mock.Anything).Return(nil, errors.New("http 429: rate limit")).Once()
	ag := core.NewAgent(client, "old", "", nil)
	ag.MaxRetries = 0
	return ag
}

// promptAgent replies to every stream and reports the prompt text.
func promptAgent(t *testing.T, prompts chan<- string) *core.Agent {
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("openai").Maybe()
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
		prompts <- lastUserText(req)
		return reply("ok"), nil
	})
	return core.NewAgent(client, "new", "", nil)
}

func TestSubmitDuringTurnRescueQueuedBehindRetry(t *testing.T) {
	c, ctx, goal, confirm := rescueController(t)
	// L4 reports only the last turn of a queue chain: the retry starts "second".
	goal.EXPECT().goalAfterTurn(mock.MatchedBy(func(e core.EvTurnEnd) bool { return e.Err == nil })).Return().Once()
	asked, answer := make(chan struct{}), make(chan struct{})
	confirm.EXPECT().awaitPending(mock.Anything, mock.MatchedBy(func(r PendingRequest) bool { return r.Kind == "rescue" })).
		RunAndReturn(func(context.Context, PendingRequest) ([]tools.QuestionAnswer, error) {
			close(asked)
			<-answer
			return []tools.QuestionAnswer{{Custom: "openai/new"}}, nil
		}).Once()
	prompts := make(chan string, 2)
	next := promptAgent(t, prompts)
	builder := newMockAgentBuilderLocal(t)
	builder.EXPECT().BuildAgentForRescue("openai", "new").Return(next, "openai", "new", nil).Once()
	require.NoError(t, c.call(ctx, func() { c.turn.agent = rateLimitedAgent(t); c.deps.AgentBuilder = builder }))

	require.NoError(t, c.Submit(ctx, Input{Text: "first"}))
	recv(t, ctx, (<-chan struct{})(asked))
	require.Equal(t, RunStateRunning, c.Info().Run)
	require.NoError(t, c.Submit(ctx, Input{Text: "second"}))
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Len(t, snap.Queue, 1)
	close(answer)
	require.Equal(t, "first", recv(t, ctx, (<-chan string)(prompts)))
	require.Equal(t, "second", recv(t, ctx, (<-chan string)(prompts)))
	waitIdle(t, c, ctx)
}

func TestFailedTurnRescueReportsOriginalEndOnce(t *testing.T) {
	c, ctx, goal, confirm := rescueController(t)
	reported := make(chan core.EvTurnEnd, 2)
	goal.EXPECT().goalAfterTurn(mock.Anything).Run(func(e core.EvTurnEnd) { reported <- e }).Once()
	confirm.EXPECT().awaitPending(mock.Anything, mock.Anything).Return(nil, ErrNotAttached).Once()
	require.NoError(t, c.call(ctx, func() { c.turn.agent = rateLimitedAgent(t); c.deps.AgentBuilder = newMockAgentBuilderLocal(t) }))
	require.NoError(t, c.Submit(ctx, Input{Text: "first"}))
	end := recv(t, ctx, (<-chan core.EvTurnEnd)(reported))
	require.ErrorContains(t, end.Err, "rate limit")
	waitIdle(t, c, ctx)
	require.Empty(t, reported)
}

func TestSuccessfulTurnRescueReportsRetryEndOnce(t *testing.T) {
	c, ctx, goal, confirm := rescueController(t)
	reported := make(chan core.EvTurnEnd, 2)
	goal.EXPECT().goalAfterTurn(mock.Anything).Run(func(e core.EvTurnEnd) { reported <- e }).Once()
	confirm.EXPECT().awaitPending(mock.Anything, mock.Anything).Return([]tools.QuestionAnswer{{Selected: []string{"openai/new"}}}, nil).Once()
	prompts := make(chan string, 1)
	builder := newMockAgentBuilderLocal(t)
	builder.EXPECT().BuildAgentForRescue("openai", "new").Return(promptAgent(t, prompts), "openai", "new", nil).Once()
	require.NoError(t, c.call(ctx, func() { c.turn.agent = rateLimitedAgent(t); c.deps.AgentBuilder = builder }))
	require.NoError(t, c.Submit(ctx, Input{Text: "first"}))
	require.Equal(t, "first", recv(t, ctx, (<-chan string)(prompts)))
	require.NoError(t, recv(t, ctx, (<-chan core.EvTurnEnd)(reported)).Err)
	waitIdle(t, c, ctx)
	require.Empty(t, reported)
}

func TestCompactionNonEligibleErrorSkipsRescue(t *testing.T) {
	c, ctx := newTestController(t)
	turn := lane9Turn(t, c, ctx)
	turn.EXPECT().currentAgent().Return(failingAgent(t)).Maybe()
	drained := make(chan struct{})
	turn.EXPECT().drainQueued().RunAndReturn(func() { close(drained) }).Once()
	// No awaitPending expectation: a rescue prompt fails the test.
	confirm := newMockconfirmSeam(t)
	require.NoError(t, c.call(ctx, func() { c.confirmSeam = confirm; c.deps.AgentBuilder = newMockAgentBuilderLocal(t) }))
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	require.NoError(t, c.Compact(ctx))
	nextKinds(t, ctx, events, EvCompaction)
	require.Equal(t, "failed", nextKinds(t, ctx, events, EvCompaction).Data.(CompactionEvent).Phase)
	recv(t, ctx, (<-chan struct{})(drained))
}

func TestCompactionPromptStartFailureRequeuedAtFront(t *testing.T) {
	c, ctx := newTestController(t)
	turn := lane9Turn(t, c, ctx)
	turn.EXPECT().currentAgent().Return(lane9Agent(t)).Maybe()
	next := Input{Text: "pending", Origin: InputOriginUser}
	turn.EXPECT().startTurn(next).Return(errors.New("no agent")).Once()
	requeued := make(chan struct{})
	turn.EXPECT().enqueueFront(next).RunAndReturn(func(Input) { close(requeued) }).Once()
	drained := make(chan struct{})
	turn.EXPECT().drainQueued().RunAndReturn(func() { close(drained) }).Once()
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	require.NoError(t, c.call(ctx, func() { require.NoError(t, c.beginCompact(&next)) }))
	defer recv(t, ctx, (<-chan struct{})(drained))
	note := nextKinds(t, ctx, events, EvNote)
	select {
	case <-requeued:
	default:
		t.Fatal("note emitted before the prompt was re-queued")
	}
	require.Contains(t, note.Data.(NoteEvent).Note.Text, "no agent")
}

func TestAbortDuringCompactionRescueCleansPendingAndHoldsPrompt(t *testing.T) {
	c, ctx := newTestController(t)
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("openai").Maybe()
	client.EXPECT().Stream(mock.Anything, mock.Anything).Return(nil, errors.New("http 503 service unavailable")).Once()
	ag := core.NewAgent(client, "model", "", nil)
	for range 6 {
		ag.AppendUserContext("original", nil)
	}
	before := ag.Messages()
	trigger := Input{Text: "first", Origin: InputOriginUser}
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	// Real CONFIRM cluster: the rescue registers a pending request.
	require.NoError(t, c.call(ctx, func() {
		c.attach = AttachStateAttached
		c.settings.title = "t"
		c.turn.agent = ag
		c.deps.AgentBuilder = newMockAgentBuilderLocal(t)
		c.enqueue(Input{Text: "second"})
		require.NoError(t, c.beginCompact(&trigger))
	}))
	pending := nextKinds(t, ctx, events, EvPending).Data.(PendingEvent).Pending
	require.Equal(t, "rescue", pending.Kind)
	require.NoError(t, c.Abort(ctx))
	require.Equal(t, pending.ID, nextKinds(t, ctx, events, EvPendingResolved).Data.(PendingResolvedEvent).ID)
	require.Equal(t, "failed", nextKinds(t, ctx, events, EvCompaction).Data.(CompactionEvent).Phase)
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Empty(t, snap.Pending)
	require.Equal(t, RunStateIdle, snap.Info.Run)
	require.Len(t, snap.Queue, 2)
	require.Equal(t, "first", snap.Queue[0].Input.Text)
	require.Equal(t, "second", snap.Queue[1].Input.Text)
	require.Equal(t, before, ag.Messages())
	require.Equal(t, "queued prompts held; they run on your next message", nextKinds(t, ctx, events, EvNote).Data.(NoteEvent).Note.Text)
}

// GO-004: after an aborted compaction rescue, the next message drains the
// held queue in order.
func TestSubmitAfterAbortedRescueRunsQueueInOrder(t *testing.T) {
	c, ctx := newTestController(t)
	goal := newMockgoalSeam(t)
	goal.EXPECT().goalBeforeTurn(mock.Anything).Return().Maybe()
	goal.EXPECT().goalAfterTurn(mock.Anything).Return().Maybe()
	prompts := make(chan string, 3)
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("openai").Maybe()
	calls := 0
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("http 503 service unavailable")
		}
		prompts <- lastUserText(req)
		return reply("ok"), nil
	})
	ag := core.NewAgent(client, "model", "", nil)
	ag.MaxRetries = 0
	for range 6 {
		ag.AppendUserContext("original", nil)
	}
	trigger := Input{Text: "first", Origin: InputOriginUser}
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	require.NoError(t, c.call(ctx, func() {
		c.goalSeam = goal
		c.attach = AttachStateAttached
		c.settings.title = "t"
		c.turn.agent = ag
		c.deps.AgentBuilder = newMockAgentBuilderLocal(t)
		c.enqueue(Input{Text: "second"})
		require.NoError(t, c.beginCompact(&trigger))
	}))
	nextKinds(t, ctx, events, EvPending)
	require.NoError(t, c.Abort(ctx))
	nextKinds(t, ctx, events, EvNote)
	require.NoError(t, c.Submit(ctx, Input{Text: "third"}))
	require.Equal(t, "first", recv(t, ctx, (<-chan string)(prompts)))
	require.Equal(t, "second", recv(t, ctx, (<-chan string)(prompts)))
	require.Equal(t, "third", recv(t, ctx, (<-chan string)(prompts)))
	waitIdle(t, c, ctx)
}

// GO-005: a failed rescue with queued input reports exactly one turn end.
func TestFailedRescueWithQueuedInputReportsOnce(t *testing.T) {
	c, ctx, goal, confirm := rescueController(t)
	reported := make(chan core.EvTurnEnd, 3)
	goal.EXPECT().goalAfterTurn(mock.Anything).Run(func(e core.EvTurnEnd) { reported <- e }).Once()
	asked, answer := make(chan struct{}), make(chan struct{})
	confirm.EXPECT().awaitPending(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, PendingRequest) ([]tools.QuestionAnswer, error) {
		close(asked)
		<-answer
		return nil, errors.New("declined")
	}).Once()
	prompts := make(chan string, 1)
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("openai").Maybe()
	calls := 0
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("http 429: rate limit")
		}
		prompts <- lastUserText(req)
		return reply("ok"), nil
	}).Twice()
	ag := core.NewAgent(client, "old", "", nil)
	ag.MaxRetries = 0
	require.NoError(t, c.call(ctx, func() { c.turn.agent = ag; c.deps.AgentBuilder = newMockAgentBuilderLocal(t) }))
	require.NoError(t, c.Submit(ctx, Input{Text: "first"}))
	recv(t, ctx, (<-chan struct{})(asked))
	require.NoError(t, c.Submit(ctx, Input{Text: "second"}))
	close(answer)
	require.Equal(t, "second", recv(t, ctx, (<-chan string)(prompts)))
	require.NoError(t, recv(t, ctx, (<-chan core.EvTurnEnd)(reported)).Err)
	waitIdle(t, c, ctx)
	require.Empty(t, reported)
}
