package session_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/bnema/zut/packages/agent/scheduler"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func goalStore(t *testing.T, initial *core.SessionGoal) (*sessionmocks.MockSessionStore, **core.SessionGoal) {
	store := sessionmocks.NewMockSessionStore(t)
	g := initial
	store.EXPECT().CurrentSessionPath().Return("")
	store.EXPECT().CurrentPlan().Return(nil)
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
	store.EXPECT().CurrentPlan().Return(nil)
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

func TestGoalTerminalErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status core.GoalStatus
	}{
		{"failure", errors.New("provider failed"), core.GoalBlocked},
		{"loop", fmt.Errorf("turn: %w", core.ErrRepetitiveLoop), core.GoalStalled},
		{"cancel", fmt.Errorf("turn: %w", context.Canceled), core.GoalActive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, g := goalStore(t, &core.SessionGoal{ID: "g", Objective: "work", Status: core.GoalActive})
			c := session.GoalTestController(t, store, 1)
			require.NoError(t, c.SetGoal(context.Background(), session.GoalCommand{Op: "resume"}))
			session.GoalTestFinish(t, c, 3, false, core.EvTurnEnd{Err: tc.err})
			require.Equal(t, tc.status, (*g).Status)
			require.Empty(t, (*g).ContinuationID)
			require.Equal(t, uint64(3), (*g).TokensUsed)
			if tc.status != core.GoalActive {
				require.Equal(t, tc.err.Error(), (*g).Reason)
			}
		})
	}
}
func TestGoalInitialProjectionAndRestoredPlan(t *testing.T) {
	store := sessionmocks.NewMockSessionStore(t)
	g := &core.SessionGoal{ID: "g", Objective: "restored", Status: core.GoalPaused}
	steps := []core.PlanStep{{Step: "old", Status: core.PlanCompleted}, {Step: "next", Status: core.PlanInProgress}}
	snapshots := map[string]core.PlanUpdate{"historical-call": {Plan: steps}}
	store.EXPECT().InitialPlanSnapshots().Return(snapshots).Once()
	store.EXPECT().CurrentGoal().Return(g)
	store.EXPECT().CurrentPlan().Return(steps)
	store.EXPECT().CurrentGoalHistory().Return([]core.SessionGoal{*g})
	store.EXPECT().CurrentSessionPath().Return("restored.jsonl")
	ctx, cancel := context.WithCancel(context.Background())
	c := session.NewController(ctx, session.Deps{SessionStore: store})
	defer func() { cancel(); <-c.Done() }()
	require.Equal(t, &session.GoalSummary{Objective: "restored", Status: "paused"}, c.Info().Goal)
	require.Equal(t, &session.PlanStepSummary{Index: 2, Total: 2, Text: "next"}, c.Info().PlanStep)
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, snapshots, snap.Plan)
	require.Equal(t, c.Info().Goal, snap.Info.Goal)
	require.Equal(t, uint64(0), snap.Seq)
}
func TestGoalToolResultsReloadWithoutPersisting(t *testing.T) {
	for _, supersede := range []bool{false, true} {
		t.Run(fmt.Sprint(supersede), func(t *testing.T) {
			store := sessionmocks.NewMockSessionStore(t)
			g := &core.SessionGoal{ID: "old", MissionID: "mission", Objective: "work", Status: core.GoalActive}
			store.EXPECT().CurrentGoal().RunAndReturn(func() *core.SessionGoal { return g })
			store.EXPECT().CurrentPlan().Return(nil)
			store.EXPECT().CurrentSessionPath().Return("")
			c := session.GoalTestController(t, store, 0)
			// Arm through the same cancellation path, but without a goal continuation.
			session.GoalTestInterrupt(t, c)
			events, err := c.Subscribe(context.Background(), 0)
			require.NoError(t, err)
			update := tools.GoalUpdate{Status: core.GoalDone}
			g = &core.SessionGoal{ID: "old", MissionID: "mission", Objective: "work", Status: core.GoalDone}
			if supersede {
				update = tools.GoalUpdate{Status: core.GoalSuperseded, GoalID: "old", Objective: "replacement"}
				g = &core.SessionGoal{ID: "new", MissionID: "mission", Objective: "replacement", Status: core.GoalActive}
			}
			session.GoalTestObserve(t, c, core.ToolResult{Details: update})
			ev := <-events
			require.Equal(t, session.EvGoal, ev.Kind)
			require.Equal(t, g, ev.Data.(session.GoalEvent).Goal)
			// No PersistGoal/PersistGoalRuntime expectations: observing must not write.
		})
	}
}
func TestGoalSavePublishesAssignedIdentity(t *testing.T) {
	store := sessionmocks.NewMockSessionStore(t)
	var g *core.SessionGoal
	store.EXPECT().CurrentGoal().RunAndReturn(func() *core.SessionGoal { return g })
	store.EXPECT().CurrentPlan().Return(nil)
	store.EXPECT().CurrentSessionPath().Return("")
	store.EXPECT().PersistGoal(mock.Anything).RunAndReturn(func(v *core.SessionGoal) error {
		v.ID = "assigned"
		v.MissionID = "mission"
		v.Ordinal = 7
		v.Status = core.GoalPaused
		g = v
		return nil
	}).Once()
	c := session.GoalTestController(t, store, 0)
	events, err := c.Subscribe(context.Background(), 0)
	require.NoError(t, err)
	require.NoError(t, c.SetGoal(context.Background(), session.GoalCommand{Op: "set", Objective: "work"}))
	ev := <-events
	got := ev.Data.(session.GoalEvent).Goal
	require.Equal(t, "assigned", got.ID)
	require.Equal(t, "mission", got.MissionID)
	require.Equal(t, 7, got.Ordinal)
}
func TestGoalReplacedMidTurnIsNotCharged(t *testing.T) {
	store, g := goalStore(t, &core.SessionGoal{ID: "old", MissionID: "m", Objective: "work", Status: core.GoalActive})
	// One start for the original continuation, one for the replacement goal.
	c := session.GoalTestController(t, store, 2)
	require.NoError(t, c.SetGoal(context.Background(), session.GoalCommand{Op: "resume"}))
	*g = &core.SessionGoal{ID: "new", MissionID: "m", Objective: "replacement", Status: core.GoalActive}
	session.GoalTestFinish(t, c, 5, true, core.EvTurnEnd{})
	require.Equal(t, "new", (*g).ID)
	require.Zero(t, (*g).TokensUsed)
}
