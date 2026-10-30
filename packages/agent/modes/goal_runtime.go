package modes

import (
	"fmt"

	"github.com/bnema/zut/packages/core"
)

// recoverGoalRun clears a lease left by a previous process. The new process
// has no provider turn associated with it, so retaining that lease would make
// the active goal permanently unschedulable.
func (i *Interactive) recoverGoalRun() {
	if i.cfg.CurrentGoal == nil || i.cfg.PersistGoalRuntime == nil {
		return
	}
	goal := copySessionGoal(i.cfg.CurrentGoal())
	if goal == nil || goal.Status != core.GoalActive || goal.ContinuationID == "" {
		return
	}
	goal.ContinuationID = ""
	if err := i.cfg.PersistGoalRuntime(goal); err != nil {
		i.ReportError(fmt.Errorf("recover goal continuation: %w", err))
	}
}
