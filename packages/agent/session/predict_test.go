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

// predictAgentWithoutCalls fails the test on any provider request.
func predictAgentWithoutCalls(t *testing.T) *core.Agent {
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("openai").Maybe()
	ag := core.NewAgent(client, "model", "", nil)
	ag.AppendUserContext("hello", nil)
	return ag
}

func TestPredictNextPromptSkipsWhenNotQuiescent(t *testing.T) {
	cases := map[string]func(c *Controller){
		"queued": func(c *Controller) { c.turn.queue = []QueuedInput{{ID: "1", Input: Input{Text: "next"}}} },
		"pending": func(c *Controller) {
			c.confirm.pending = map[string]*pendingEntry{"r": {result: make(chan pendingResult, 1)}}
		},
		"no-agent":  func(c *Controller) {},
		"turn-busy": func(c *Controller) {},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			c, ctx := newTestController(t)
			turn := newMockturnSeam(t)
			turn.EXPECT().isBusy().Return(name == "turn-busy").Maybe()
			turn.EXPECT().turnOrigin().Return(InputOrigin("")).Maybe()
			turn.EXPECT().turnInfo().Return(turnInfoProjection{}).Maybe()
			if name == "no-agent" {
				turn.EXPECT().currentAgent().Return(nil).Once()
			} else {
				turn.EXPECT().currentAgent().Return(predictAgentWithoutCalls(t)).Maybe()
			}
			require.NoError(t, c.call(ctx, func() { c.turnSeam = turn; setup(c) }))
			text, err := c.PredictNextPrompt(ctx)
			require.NoError(t, err)
			require.Empty(t, text)
		})
	}
}

func TestPredictNextPromptHeadlessSkipsRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := NewController(ctx, Deps{Headless: true})
	t.Cleanup(func() { cancel(); <-c.Done() })
	text, err := c.PredictNextPrompt(ctx)
	require.NoError(t, err)
	require.Empty(t, text)
}

func TestPredictNextPromptIdleCallsAgent(t *testing.T) {
	c, ctx := newTestController(t)
	turn := lane9Turn(t, c, ctx)
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("openai").Maybe()
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, provider.Request) (<-chan provider.Event, error) {
		ch := make(chan provider.Event, 2)
		ch <- provider.EventTextDelta{Delta: "run the tests"}
		ch <- provider.EventDone{}
		close(ch)
		return ch, nil
	}).Once()
	ag := core.NewAgent(client, "model", "", nil)
	ag.AppendUserContext("hello", nil)
	turn.EXPECT().currentAgent().Return(ag).Once()
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	before := ag.MessageCount()
	text, err := c.PredictNextPrompt(ctx)
	require.NoError(t, err)
	require.Equal(t, "run the tests", text)
	require.Equal(t, before, ag.MessageCount())
	// A follow-up actor call drains the loop; no event may have been published.
	require.NoError(t, c.call(ctx, func() {}))
	select {
	case ev := <-events:
		t.Fatalf("unexpected event %v", ev.Kind)
	default:
	}
}

// blockingPredictAgent returns an agent whose first Stream signals started
// and blocks until its ctx ends; later Streams answer reply.
func blockingPredictAgent(t *testing.T, started chan<- struct{}, reply string) (*core.Agent, *providermocks.MockClient) {
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("openai").Maybe()
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
		started <- struct{}{}
		ch := make(chan provider.Event)
		go func() { <-ctx.Done(); close(ch) }()
		return ch, nil
	}).Once()
	if reply != "" {
		client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, provider.Request) (<-chan provider.Event, error) {
			ch := make(chan provider.Event, 2)
			ch <- provider.EventTextDelta{Delta: reply}
			ch <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: reply}}}}
			close(ch)
			return ch, nil
		}).Once()
	}
	ag := core.NewAgent(client, "model", "", nil)
	ag.AppendUserContext("hello", nil)
	return ag, client
}

type predictResult struct {
	text string
	err  error
}

func startPrediction(ctx context.Context, c *Controller) <-chan predictResult {
	out := make(chan predictResult, 1)
	go func() {
		text, err := c.PredictNextPrompt(ctx)
		out <- predictResult{text, err}
	}()
	return out
}

func waitResult(t *testing.T, ctx context.Context, ch <-chan predictResult) predictResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return predictResult{}
	}
}

func waitStarted(t *testing.T, ctx context.Context, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestPredictNextPromptCloseReturnsErrClosed(t *testing.T) {
	c, ctx := newTestController(t)
	turn := lane9Turn(t, c, ctx)
	started := make(chan struct{}, 1)
	ag, _ := blockingPredictAgent(t, started, "")
	turn.EXPECT().currentAgent().Return(ag).Once()
	res := startPrediction(ctx, c)
	waitStarted(t, ctx, started)
	require.NoError(t, c.Close(ctx, "test"))
	r := waitResult(t, ctx, res)
	require.ErrorIs(t, r.err, ErrClosed)
	require.Empty(t, r.text)
}

func TestPredictNextPromptCallerCancel(t *testing.T) {
	c, ctx := newTestController(t)
	turn := lane9Turn(t, c, ctx)
	started := make(chan struct{}, 1)
	ag, _ := blockingPredictAgent(t, started, "")
	turn.EXPECT().currentAgent().Return(ag).Once()
	pctx, cancel := context.WithCancel(ctx)
	res := startPrediction(pctx, c)
	waitStarted(t, ctx, started)
	cancel()
	r := waitResult(t, ctx, res)
	require.ErrorIs(t, r.err, context.Canceled)
	require.False(t, errors.Is(r.err, ErrClosed))
}

func TestPredictNextPromptSecondCancelsFirst(t *testing.T) {
	c, ctx := newTestController(t)
	turn := lane9Turn(t, c, ctx)
	started := make(chan struct{}, 1)
	ag, _ := blockingPredictAgent(t, started, "second guess")
	turn.EXPECT().currentAgent().Return(ag).Twice()
	first := startPrediction(ctx, c)
	waitStarted(t, ctx, started)
	text, err := c.PredictNextPrompt(ctx)
	require.NoError(t, err)
	require.Equal(t, "second guess", text)
	r := waitResult(t, ctx, first)
	require.ErrorIs(t, r.err, context.Canceled)
}

func TestPredictNextPromptTurnStartCancels(t *testing.T) {
	c, ctx := newTestController(t)
	started := make(chan struct{}, 1)
	ag, _ := blockingPredictAgent(t, started, "turn reply")
	// A preset title keeps title generation off the provider mock.
	require.NoError(t, c.call(ctx, func() { c.turn.agent, c.settings.title = ag, "t" }))
	before := ag.MessageCount()
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	res := startPrediction(ctx, c)
	waitStarted(t, ctx, started)
	require.NoError(t, c.Submit(ctx, Input{Text: "go"}))
	r := waitResult(t, ctx, res)
	require.ErrorIs(t, r.err, context.Canceled)
	for idle := false; !idle; {
		select {
		case ev := <-events:
			if ev.Kind == EvInfo && ev.Data.(InfoEvent).Info.Run == RunStateIdle && ev.Data.(InfoEvent).Info.Activity.Kind == "idle" {
				var busy bool
				require.NoError(t, c.call(ctx, func() { busy = c.turn.busy }))
				idle = !busy
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	msgs := ag.Messages()
	require.Len(t, msgs, before+2)
	require.Equal(t, "turn reply", msgs[len(msgs)-1].Content[0].(provider.TextBlock).Text)
}

func TestPredictNextPromptAgentSwapCancels(t *testing.T) {
	c, ctx := newTestController(t)
	started := make(chan struct{}, 1)
	ag, _ := blockingPredictAgent(t, started, "")
	require.NoError(t, c.call(ctx, func() { c.turn.agent = ag }))
	res := startPrediction(ctx, c)
	waitStarted(t, ctx, started)
	var swapErr error
	require.NoError(t, c.call(ctx, func() { swapErr = c.setAgent(core.NewAgent(nil, "other", "", nil)) }))
	require.NoError(t, swapErr)
	r := waitResult(t, ctx, res)
	require.ErrorIs(t, r.err, context.Canceled)
}

func TestPredictNextPromptCompactionCancels(t *testing.T) {
	c, ctx := newTestController(t)
	turn := lane9Turn(t, c, ctx)
	turn.EXPECT().drainQueued().Return().Maybe()
	started := make(chan struct{}, 1)
	ag, _ := blockingPredictAgent(t, started, "summary")
	turn.EXPECT().currentAgent().Return(ag).Maybe()
	res := startPrediction(ctx, c)
	waitStarted(t, ctx, started)
	var compactErr error
	require.NoError(t, c.call(ctx, func() { compactErr = c.beginCompact(nil) }))
	require.NoError(t, compactErr)
	r := waitResult(t, ctx, res)
	require.ErrorIs(t, r.err, context.Canceled)
	var done chan struct{}
	require.NoError(t, c.call(ctx, func() { done = c.compactDone }))
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
