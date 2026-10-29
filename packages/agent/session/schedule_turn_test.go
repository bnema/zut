package session

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/stretchr/testify/require"
)

// scriptedClient answers each request from script in order, then with text.
type scriptedClient struct {
	calls  atomic.Int32
	script func(call int) provider.Message
}

func (c *scriptedClient) Name() string { return "scripted" }

func (c *scriptedClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	call := int(c.calls.Add(1))
	msg := c.script(call)
	stop := provider.StopEnd
	for _, content := range msg.Content {
		if _, ok := content.(provider.ToolCallBlock); ok {
			stop = provider.StopToolUse
		}
	}
	out := make(chan provider.Event, 2)
	out <- provider.EventStart{Provider: c.Name(), Model: req.Model}
	out <- provider.EventDone{Stop: stop, Message: msg}
	close(out)
	return out, nil
}

type repeatTool struct{}

func (repeatTool) Name() string            { return "repeat" }
func (repeatTool) Description() string     { return "repeats" }
func (repeatTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (repeatTool) Execute(context.Context, json.RawMessage, func(string)) (core.ToolResult, error) {
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "unchanged"}}}, nil
}

// repeatingScript repeats one identical tool call `repeats` times, then ends.
func repeatingScript(repeats int) func(int) provider.Message {
	return func(call int) provider.Message {
		if call > repeats {
			return provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "done"}}}
		}
		return provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{
			ID: fmt.Sprintf("call-%d", call), Name: "repeat", Arguments: json.RawMessage(`{}`),
		}}}
	}
}

func scheduleTurnController(t *testing.T, ag *core.Agent) (*Controller, context.Context) {
	t.Helper()
	c, ctx := newTestController(t)
	turnSeams(t, c, ctx)
	residents := newMockresidentSeam(t)
	residents.EXPECT().residentsRunStarted().Return().Maybe()
	residents.EXPECT().residentsRunEnded().Return().Maybe()
	residents.EXPECT().residentSummaries().Return(nil).Maybe()
	residents.EXPECT().residentsRunning().Return(false).Maybe()
	require.NoError(t, c.call(ctx, func() { c.residentSeam = residents; c.turn.agent = ag }))
	return c, ctx
}

// runToIdle submits in and waits for the turn to finish, returning its end.
func runToIdle(t *testing.T, c *Controller, ctx context.Context, in Input) core.EvTurnEnd {
	t.Helper()
	require.NoError(t, c.Submit(ctx, in))
	var end core.EvTurnEnd
	require.Eventually(t, func() bool {
		idle := false
		require.NoError(t, c.call(ctx, func() { idle = !c.turn.busy; end = c.turn.end }))
		return idle
	}, 10*time.Second, 5*time.Millisecond)
	return end
}

func repetitionWarned(ag *core.Agent) bool {
	for _, m := range ag.Messages() {
		if m.Meta[core.RepetitionGuardMetaKey] == "true" {
			return true
		}
	}
	return false
}

// A scheduled turn runs without the repetition guard, as the in-process
// scheduler did; the guard is back for the next user turn.
func TestScheduledTurnDisablesRepetitionGuard(t *testing.T) {
	const repeats = 10 // beyond the guard's stop threshold
	client := &scriptedClient{script: repeatingScript(repeats)}
	ag := core.NewAgent(client, "model", "", core.Registry{"repeat": repeatTool{}})
	c, ctx := scheduleTurnController(t, ag)

	end := runToIdle(t, c, ctx, Input{Origin: InputOriginScheduler, Mode: SubmitModeQueue, Text: "scheduled task"})
	require.NoError(t, end.Err)
	require.EqualValues(t, repeats+1, client.calls.Load(), "scheduled turn stopped early")
	require.False(t, repetitionWarned(ag), "scheduled turn injected a repetition warning")
	require.False(t, ag.DisableRepetitionGuard, "guard left disabled after the scheduled turn")

	// The same loop from the user is stopped by the guard.
	client.calls.Store(0)
	runToIdle(t, c, ctx, Input{Origin: InputOriginUser, Text: "do it again"})
	require.Less(t, client.calls.Load(), int32(repeats+1), "user turn ran without the guard")
	require.True(t, repetitionWarned(ag))
}

// A scheduled turn keeps an operator's --allow-repetition choice.
func TestScheduledTurnKeepsExplicitGuardOptOut(t *testing.T) {
	client := &scriptedClient{script: repeatingScript(1)}
	ag := core.NewAgent(client, "model", "", core.Registry{"repeat": repeatTool{}})
	ag.DisableRepetitionGuard = true
	c, ctx := scheduleTurnController(t, ag)
	runToIdle(t, c, ctx, Input{Origin: InputOriginScheduler, Mode: SubmitModeQueue, Text: "scheduled task"})
	require.True(t, ag.DisableRepetitionGuard)
}

// A plan change made by a scheduled turn goes through the agent's commit hook
// (the session store's persistence) and reaches the agent plan and snapshot.
func TestScheduledTurnPersistsPlanMutation(t *testing.T) {
	client := &scriptedClient{script: func(call int) provider.Message {
		if call == 1 {
			return provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{
				ID: "plan-call-1", Name: tools.PlanToolName, Arguments: json.RawMessage(`{"action":"set","steps":[{"step":"one","status":"pending"}]}`),
			}}}
		}
		return provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "done"}}}
	}}
	ag := core.NewAgent(client, "model", "", core.Registry{tools.PlanToolName: &tools.PlanTool{}})
	var mu sync.Mutex
	var committed []core.ToolResult
	ag.CommitToolResult = func(_ string, result core.ToolResult) error {
		mu.Lock()
		defer mu.Unlock()
		committed = append(committed, result)
		return nil
	}
	c, ctx := scheduleTurnController(t, ag)

	end := runToIdle(t, c, ctx, Input{Origin: InputOriginScheduler, Mode: SubmitModeQueue, Text: "scheduled task"})
	require.NoError(t, end.Err)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, committed, 1)
	require.NotNil(t, committed[0].PlanOperation, "plan mutation was not committed")
	require.Equal(t, []core.PlanStep{{Step: "one", Status: core.PlanPending}}, ag.CurrentPlan())
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Contains(t, snap.Plan, "plan-call-1")
}
