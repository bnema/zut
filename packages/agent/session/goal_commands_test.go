package session_test

import (
	"context"
	"testing"

	"github.com/bnema/zut/packages/agent/scheduler"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func goalStore(t *testing.T, initial *core.SessionGoal) (*sessionmocks.MockSessionStore, **core.SessionGoal) {
	store := sessionmocks.NewMockSessionStore(t)
	g := initial
	store.EXPECT().CurrentSessionPath().Return("")
	store.EXPECT().CurrentGoal().RunAndReturn(func() *core.SessionGoal { return g })
	store.EXPECT().PersistGoal(mock.Anything).RunAndReturn(func(v *core.SessionGoal) error { g = v; return nil }).Maybe()
	store.EXPECT().PersistGoalRuntime(mock.Anything).RunAndReturn(func(v *core.SessionGoal) error { g = v; return nil }).Maybe()
	return store, &g
}
func TestGoalLifecycleAndContinuation(t *testing.T) {
	store, g := goalStore(t, nil)
	c := session.GoalTestController(t, store, 3)
	ctx := context.Background()
	require.NoError(t, c.SetGoal(ctx, session.GoalCommand{Op: "set", Objective: "finish"}))
	require.NotEmpty(t, (*g).ContinuationID)
	session.GoalTestFinish(t, c, 10, true, core.EvTurnEnd{})
	require.Equal(t, uint64(10), (*g).TokensUsed)
	require.NoError(t, c.SetGoal(ctx, session.GoalCommand{Op: "pause"}))
	session.GoalTestFinish(t, c, 2, false, core.EvTurnEnd{})
	require.Equal(t, core.GoalPaused, (*g).Status)
	require.NoError(t, c.SetGoal(ctx, session.GoalCommand{Op: "resume"}))
	require.NoError(t, c.SetGoal(ctx, session.GoalCommand{Op: "clear"}))
	require.Nil(t, *g)
}
func TestGoalBudgetAndStall(t *testing.T) {
	t.Run("budget", func(t *testing.T) {
		budget := uint64(5)
		store, g := goalStore(t, &core.SessionGoal{ID: "g", Objective: "work", Status: core.GoalActive, TokenBudget: &budget})
		c := session.GoalTestController(t, store, 1)
		require.NoError(t, c.SetGoal(context.Background(), session.GoalCommand{Op: "resume"}))
		session.GoalTestFinish(t, c, 5, true, core.EvTurnEnd{})
		require.Equal(t, core.GoalBudgetLimited, (*g).Status)
		require.Empty(t, (*g).ContinuationID)
	})
	t.Run("stall", func(t *testing.T) {
		store, g := goalStore(t, &core.SessionGoal{ID: "g", Objective: "work", Status: core.GoalActive})
		c := session.GoalTestController(t, store, 2)
		require.NoError(t, c.SetGoal(context.Background(), session.GoalCommand{Op: "resume"}))
		session.GoalTestFinish(t, c, 0, false, core.EvTurnEnd{})
		session.GoalTestFinish(t, c, 0, false, core.EvTurnEnd{})
		require.Equal(t, core.GoalStalled, (*g).Status)
	})
}
func TestGoalInterruptedResume(t *testing.T) {
	store, g := goalStore(t, &core.SessionGoal{ID: "g", MissionID: "m", Objective: "work", Status: core.GoalActive})
	c := session.GoalTestController(t, store, 2)
	require.NoError(t, c.SetGoal(context.Background(), session.GoalCommand{Op: "resume"}))
	in := session.GoalTestInterrupt(t, c)
	require.Equal(t, "latest question", in.Text)
	require.Equal(t, []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "The previous turn was interrupted by the user; the autonomous goal below remains active. Handle the latest user message first. Classify it as changing, settling, blocking, or incidental to that goal. If it is incidental, answer it briefly and then continue the goal autonomously without waiting for the user to say continue. Call update_goal only for a genuine complete, blocked, or superseded transition.\n\nActive goal: work When superseding this goal, use goal_id g. When setting a next goal, use mission_id m and keep it within the same user mission."}}, Meta: map[string]string{"zut_goal_interrupt_reassess": "true"}}}, in.Hidden)
	require.Equal(t, core.GoalActive, (*g).Status)
	require.Zero(t, (*g).ConsecutiveNoProgressTurns)
	session.GoalTestFinish(t, c, 0, false, core.EvTurnEnd{})
}

func TestGoalPersistenceFailureDoesNotStart(t *testing.T) {
	store := sessionmocks.NewMockSessionStore(t)
	store.EXPECT().CurrentGoal().Return(nil)
	store.EXPECT().CurrentSessionPath().Return("")
	store.EXPECT().PersistGoal(mock.Anything).Return(context.Canceled)
	c := session.GoalTestController(t, store, 0)
	require.ErrorIs(t, c.SetGoal(context.Background(), session.GoalCommand{Op: "set", Objective: "work"}), context.Canceled)
}
func TestGoalBudgetAlreadyExhausted(t *testing.T) {
	budget := uint64(4)
	store, g := goalStore(t, &core.SessionGoal{ID: "g", Objective: "work", Status: core.GoalActive, TokenBudget: &budget, TokensUsed: 4, ContinuationID: "old-daemon"})
	c := session.GoalTestController(t, store, 0)
	require.NoError(t, c.SetGoal(context.Background(), session.GoalCommand{Op: "resume"}))
	require.Equal(t, core.GoalBudgetLimited, (*g).Status)
	require.Empty(t, (*g).ContinuationID)
}

func TestGoalComplete(t *testing.T) {
	store, g := goalStore(t, &core.SessionGoal{Objective: "work", Status: core.GoalActive})
	c := session.GoalTestController(t, store, 0)
	require.NoError(t, c.SetGoal(context.Background(), session.GoalCommand{Op: "complete"}))
	require.Equal(t, core.GoalDone, (*g).Status)
}
func TestScheduleCount(t *testing.T) {
	host := sessionmocks.NewMockScheduleHost(t)
	host.EXPECT().List().Return([]scheduler.Task{{ID: "one"}, {ID: "two"}})
	require.Equal(t, 2, session.GoalTestScheduleCount(t, host))
}
