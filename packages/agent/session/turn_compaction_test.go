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

// integratedController wires the real TURN and COMPACT clusters together.
// Goal and confirm are mocks; confirm has no denyAllPending expectation, so a
// cancelled turn fails the test.
func integratedController(t *testing.T) (*Controller, context.Context, *mockconfirmSeam) {
	t.Helper()
	c, ctx := newTestController(t)
	goal := newMockgoalSeam(t)
	goal.EXPECT().goalBeforeTurn(mock.Anything).Return().Maybe()
	goal.EXPECT().goalAfterTurn(mock.Anything).Return().Maybe()
	confirm := newMockconfirmSeam(t)
	require.NoError(t, c.call(ctx, func() {
		c.goalSeam = goal
		c.confirmSeam = confirm
		// A preset title keeps title generation off the provider mock.
		c.settings.title = "t"
	}))
	return c, ctx, confirm
}

// lastUserText returns the prompt text of the newest user message.
func lastUserText(req provider.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != provider.RoleUser {
			continue
		}
		content := req.Messages[i].Content
		// The agent prefixes a message-time block; the prompt is the last block.
		for j := len(content) - 1; j >= 0; j-- {
			if b, ok := content[j].(provider.TextBlock); ok {
				return b.Text
			}
		}
	}
	return ""
}
func reply(text string) <-chan provider.Event {
	ch := make(chan provider.Event, 2)
	ch <- provider.EventTextDelta{Delta: text}
	ch <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: text}}}}
	close(ch)
	return ch
}

// compactingClient blocks its first (summary) stream until release is closed
// or its context ends, then replies to turn prompts and reports their text.
func compactingClient(t *testing.T, compactStarted chan<- struct{}, release <-chan struct{}, prompts chan<- string) *core.Agent {
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("openai").Maybe()
	calls := 0
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
		calls++
		if calls == 1 {
			ch := make(chan provider.Event, 2)
			close(compactStarted)
			go func() {
				select {
				case <-release:
					ch <- provider.EventTextDelta{Delta: "summary"}
					ch <- provider.EventDone{Stop: provider.StopEnd}
				case <-ctx.Done():
				}
				close(ch)
			}()
			return ch, nil
		}
		prompts <- lastUserText(req)
		return reply("ok"), nil
	})
	ag := core.NewAgent(client, "model", "", nil)
	for range 6 {
		ag.AppendUserContext("original", nil)
	}
	return ag
}
func waitIdle(t *testing.T, c *Controller, ctx context.Context) {
	t.Helper()
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	for {
		var idle bool
		require.NoError(t, c.call(ctx, func() { idle = !c.isBusy() && len(c.turn.queue) == 0 }))
		if idle {
			return
		}
		select {
		case <-events:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
func recv[T any](t *testing.T, ctx context.Context, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var zero T
	return zero
}

func TestSubmitDuringCompactionQueuedUntilItEnds(t *testing.T) {
	c, ctx, _ := integratedController(t)
	started, release, prompts := make(chan struct{}), make(chan struct{}), make(chan string, 4)
	ag := compactingClient(t, started, release, prompts)
	require.NoError(t, c.call(ctx, func() { c.turn.agent = ag }))
	require.NoError(t, c.Compact(ctx))
	recv(t, ctx, (<-chan struct{})(started))

	info := c.Info()
	require.Equal(t, RunStateCompacting, info.Run)
	require.Equal(t, "compacting", info.Activity.Kind)
	require.NoError(t, c.Submit(ctx, Input{Text: "hello"}))
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Len(t, snap.Queue, 1)
	require.Equal(t, "hello", snap.Queue[0].Input.Text)
	require.Empty(t, prompts)

	close(release)
	require.Equal(t, "hello", recv(t, ctx, (<-chan string)(prompts)))
	waitIdle(t, c, ctx)
	require.Equal(t, RunStateIdle, c.Info().Run)
}

func TestAbortDuringCompactionRunsTriggeringPromptFirst(t *testing.T) {
	c, ctx, _ := integratedController(t)
	started, prompts := make(chan struct{}), make(chan string, 4)
	ag := compactingClient(t, started, make(chan struct{}), prompts)
	before := ag.Messages()
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	trigger := Input{Text: "first", Origin: InputOriginUser}
	require.NoError(t, c.call(ctx, func() {
		c.turn.agent = ag
		require.NoError(t, c.beginCompact(&trigger))
	}))
	recv(t, ctx, (<-chan struct{})(started))
	require.NoError(t, c.Submit(ctx, Input{Text: "second"}))

	// No goalOnAbort/denyAllPending expectation: cancelling a turn fails here.
	require.NoError(t, c.Abort(ctx))
	ev := nextKinds(t, ctx, events, EvCompaction)
	require.Equal(t, "start", ev.Data.(CompactionEvent).Phase)
	ev = nextKinds(t, ctx, events, EvCompaction)
	require.Equal(t, "failed", ev.Data.(CompactionEvent).Phase)
	// Input stacked behind the trigger joins it, trigger first, in one turn.
	require.Equal(t, "first\n\nsecond", recv(t, ctx, (<-chan string)(prompts)))
	waitIdle(t, c, ctx)
	// The transcript keeps the pre-compaction history ahead of the turn.
	require.Equal(t, before, ag.Messages()[:len(before)])
}

func TestDrainQueuedNoOpWhileBusy(t *testing.T) {
	c, ctx := newTestController(t)
	require.NoError(t, c.call(ctx, func() {
		c.enqueue(Input{Text: "queued"})
		c.turn.busy = true
		c.drainQueued()
		require.Len(t, c.turn.queue, 1)
		c.turn.busy = false
		c.compact.busy = true
		c.drainQueued()
		require.Len(t, c.turn.queue, 1)
		require.True(t, c.isBusy())
		require.Equal(t, RunStateCompacting, func() RunState { c.updateInfo(); return c.Info().Run }())
		c.compact.busy = false
	}))
}

func TestRescueEligibleTurnErrorRetriesOnNewAgent(t *testing.T) {
	c, ctx, confirm := integratedController(t)
	confirm.EXPECT().awaitPending(mock.Anything, mock.MatchedBy(func(r PendingRequest) bool { return r.Kind == "rescue" && r.Rescue != nil })).
		Return([]tools.QuestionAnswer{{Selected: []string{"openai/new"}}}, nil).Once()

	oldClient := providermocks.NewMockClient(t)
	oldClient.EXPECT().Name().Return("openai").Maybe()
	oldClient.EXPECT().Stream(mock.Anything, mock.Anything).Return(nil, errors.New("http 429: rate limit")).Once()
	old := core.NewAgent(oldClient, "old", "", nil)
	old.MaxRetries = 0

	prompts := make(chan string, 1)
	newClient := providermocks.NewMockClient(t)
	newClient.EXPECT().Name().Return("openai").Maybe()
	newClient.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
		prompts <- lastUserText(req)
		return reply("ok"), nil
	}).Once()
	next := core.NewAgent(newClient, "new", "", nil)
	builder := newMockAgentBuilderLocal(t)
	builder.EXPECT().BuildAgentForRescue("openai", "new").Return(next, "openai", "new", nil).Once()
	require.NoError(t, c.call(ctx, func() { c.turn.agent = old; c.deps.AgentBuilder = builder }))

	require.NoError(t, c.Submit(ctx, Input{Text: "hello"}))
	require.Equal(t, "hello", recv(t, ctx, (<-chan string)(prompts)))
	waitIdle(t, c, ctx)
	var current *core.Agent
	require.NoError(t, c.call(ctx, func() { current = c.turn.agent }))
	require.Same(t, next, current)
	require.Equal(t, "new", c.Info().Model)
}

func TestNonRescueTurnErrorDoesNotRescue(t *testing.T) {
	require.True(t, rescueEligible(errors.New("HTTP 503 service unavailable")))
	require.False(t, rescueEligible(errors.New("http 400 bad request")))
	require.False(t, rescueEligible(nil))
}
