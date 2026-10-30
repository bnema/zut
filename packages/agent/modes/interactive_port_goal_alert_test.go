package modes

import (
	"context"
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestPortGoalTransitionAlerts(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		for _, status := range []core.GoalStatus{core.GoalBlocked, core.GoalPaused} {
			for _, infoFirst := range []bool{true, false} {
				term := &alertTestTerminal{}
				i := newQueuePortInteractive(t, sessionmocks.NewMockPort(t), session.Snapshot{Info: session.Info{ID: "s1", Goal: &session.GoalSummary{Status: string(core.GoalActive)}}}, InteractiveConfig{Terminal: term, TerminalAlertsEnabled: &enabled})
				goal := session.Event{Kind: session.EvGoal, Data: session.GoalEvent{Status: string(status)}}
				info := session.Event{Kind: session.EvInfo, Data: session.InfoEvent{Info: session.Info{ID: "s1", Goal: &session.GoalSummary{Status: string(status)}}}}
				events := []session.Event{goal, info, goal}
				if infoFirst {
					events = []session.Event{info, goal, info}
				}
				for _, ev := range events {
					i.handleSessionEvent(context.Background(), ev)
				}
				want := ""
				if enabled {
					want = "\a"
				}
				require.Equal(t, want, term.String(), "status=%s infoFirst=%v enabled=%v", status, infoFirst, enabled)
				require.Equal(t, status, i.goalStatus)
			}
		}
	}
}

func TestPortGoalCommandForms(t *testing.T) {
	for _, tt := range []struct {
		command string
		want    session.GoalCommand
	}{
		{"/goal", session.GoalCommand{Op: "status"}},
		{"/goal status", session.GoalCommand{Op: "status"}},
		{"/goal history", session.GoalCommand{Op: "history"}},
		{"/goal pause", session.GoalCommand{Op: "pause"}},
		{"/goal resume", session.GoalCommand{Op: "resume"}},
		{"/goal clear", session.GoalCommand{Op: "clear"}},
		{"/goal ship it", session.GoalCommand{Op: "set", Objective: "ship it"}},
	} {
		t.Run(tt.command, func(t *testing.T) {
			port := sessionmocks.NewMockPort(t)
			called := make(chan struct{})
			port.EXPECT().SetGoal(mock.Anything, tt.want).RunAndReturn(func(context.Context, session.GoalCommand) error { close(called); return nil }).Once()
			i := newQueuePortInteractive(t, port, session.Snapshot{}, InteractiveConfig{})
			i.runSlash(context.Background(), tt.command)
			waitSignal(t, called, "SetGoal")
		})
	}
}
