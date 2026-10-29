package session

import (
	"context"
	"testing"

	"github.com/bnema/zut/packages/agent/scheduler"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// GoalTestController keeps seam injection in the owning package while external
// tests use the exported, generated dependency mocks without an import cycle.
func GoalTestController(t *testing.T, store SessionStore, starts int) *Controller {
	c, ctx := newTestController(t)
	turn := newMockturnSeam(t)
	turn.EXPECT().isBusy().Return(false)
	turn.EXPECT().turnOrigin().Return(InputOriginUser)
	turn.EXPECT().turnInfo().Return(turnInfoProjection{})
	if starts > 0 {
		turn.EXPECT().startTurn(mock.MatchedBy(func(in Input) bool { return in.Origin == InputOriginGoal && in.Text != "" })).Return(nil).Times(starts)
	}
	require.NoError(t, c.call(ctx, func() { c.turnSeam = turn; c.deps.SessionStore = store; c.goal.controller = c; c.updateInfo() }))
	return c
}

func GoalTestFinish(t *testing.T, c *Controller, tokens int, tool bool, end core.EvTurnEnd) {
	require.NoError(t, c.call(context.Background(), func() {
		c.goal.observe(core.EvUsage{Usage: provider.Usage{InputTokens: tokens}})
		if tool {
			c.goal.observe(core.EvToolCall{Name: "read"})
		}
		c.goalAfterTurn(end)
	}))
}
func GoalTestInterrupt(t *testing.T, c *Controller) string {
	var text string
	require.NoError(t, c.call(context.Background(), func() {
		c.goalOnAbort()
		c.goalAfterTurn(core.EvTurnEnd{Err: context.Canceled})
		in := Input{Origin: InputOriginUser, Text: "latest question"}
		c.goalBeforeTurn(&in)
		text = in.Text
	}))
	return text
}
func TestGoalPlanEvents(t *testing.T) {
	c, ctx := newTestController(t)
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	require.NoError(t, c.call(ctx, func() {
		c.goal.controller = c
		c.goal.observe(core.EvPlanUpdate{CallID: "p", Update: core.PlanUpdate{Plan: []core.PlanStep{{Step: "work"}}}})
	}))
	ev := <-events
	require.Equal(t, EvPlan, ev.Kind)
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, "work", snap.Plan["p"].Plan[0].Step)
	snap.Plan["p"].Plan[0].Step = "mutated"
	snap, err = c.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, "work", snap.Plan["p"].Plan[0].Step)
}
func TestScheduledInputQueuesWithoutStarting(t *testing.T) {
	c, ctx := newTestController(t)
	turn := newMockturnSeam(t)
	turn.EXPECT().enqueue(Input{Origin: InputOriginScheduler, Mode: SubmitModeQueue, Text: "scheduled"}).Return().Once()
	require.NoError(t, c.call(ctx, func() { c.turnSeam = turn; c.deliverScheduled(scheduler.Task{Message: "scheduled"}) }))
}

// GoalTestScheduleCount reads the scheduler projection on the actor.
func GoalTestScheduleCount(t *testing.T, host ScheduleHost) int {
	c, ctx := newTestController(t)
	var count int
	require.NoError(t, c.call(ctx, func() { c.deps.Scheduler = host; count = c.scheduleCount() }))
	return count
}
