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
		"queued":   func(c *Controller) { c.turn.queue = []QueuedInput{{ID: "1", Input: Input{Text: "next"}}} },
		"rescuing": func(c *Controller) { c.turn.rescuing = true },
		"compact":  func(c *Controller) { c.compact.busy = true },
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
