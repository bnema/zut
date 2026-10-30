package modes

import (
	"context"
	"strings"

	"github.com/bnema/zut/packages/agent/session"
)

// These tags remain presentation filters for historical saved transcripts.
const goalContinueMetaKey = "zut_goal_continue"
const goalInterruptReassessMetaKey = "zut_goal_interrupt_reassess"

// portGoalCommand maps the /goal sub-forms onto the daemon's GoalCommand.
func (i *Interactive) portGoalCommand(cmd string, parts []string) {
	goal := session.GoalCommand{Op: "status"}
	if len(parts) > 1 {
		arg := strings.ToLower(parts[1])
		switch {
		case len(parts) == 2 && (arg == "pause" || arg == "resume" || arg == "clear" || arg == "history" || arg == "status"):
			goal.Op = arg
		default:
			goal = session.GoalCommand{Op: "set", Objective: strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cmd), parts[0]))}
		}
	}
	i.portSettingsCall("goal", func(ctx context.Context, port session.Port) error {
		return port.SetGoal(ctx, goal)
	})
}
