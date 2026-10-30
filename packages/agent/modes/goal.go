package modes

import (
	"context"
	"fmt"
	"strings"

	"github.com/bnema/zut/packages/agent/session"
	agenttools "github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

const goalContinueMetaKey = "zut_goal_continue"

// goalInterruptReassessMetaKey marks the hidden post-interruption
// instruction appended before the next ordinary user prompt. It is filtered
// from the rendered transcript like goalContinueMetaKey.
const goalInterruptReassessMetaKey = "zut_goal_interrupt_reassess"

// interruptedGoalState is transient controller state recording that Esc
// cancelled the running turn of an active goal. nextPromptArmed means the
// immediately following ordinary user prompt must carry the hidden
// reassessment instruction.
type interruptedGoalState struct {
	goalID          string
	missionID       string
	nextPromptArmed bool
}

func (i *Interactive) goalUpdatesAvailable() bool {
	ag := i.Agent()
	if ag == nil {
		return false
	}
	_, ok := ag.ToolsSnapshot()[agenttools.UpdateGoalToolName]
	return ok
}

// armInterruptedGoalReturn records a transient return intent when Esc cancels
// the running turn of an active goal. The persisted goal stays active; only
// the next ordinary user prompt carries the hidden reassessment instruction
// and may resume the same goal afterwards.
func (i *Interactive) armInterruptedGoalReturn() {
	if i.cfg.CurrentGoal == nil {
		return
	}
	goal := i.cfg.CurrentGoal()
	if goal == nil || goal.Status != core.GoalActive || strings.TrimSpace(goal.Objective) == "" {
		return
	}
	i.mu.Lock()
	i.interruptedGoalReturn = &interruptedGoalState{
		goalID:          goal.ID,
		missionID:       goal.MissionID,
		nextPromptArmed: true,
	}
	i.mu.Unlock()
}

// clearInterruptedGoalReturn drops transient interruption state without
// touching the persisted goal.
func (i *Interactive) clearInterruptedGoalReturn() {
	i.mu.Lock()
	i.interruptedGoalReturn = nil
	i.mu.Unlock()
}

func copySessionGoal(goal *core.SessionGoal) *core.SessionGoal {
	if goal == nil {
		return nil
	}
	copyGoal := *goal
	return &copyGoal
}

func (i *Interactive) runGoalCommand(ctx context.Context, cmd string, parts []string) {
	if i.portMode.Load() {
		i.portGoalCommand(cmd, parts)
		return
	}
	if i.cfg.CurrentGoal == nil || i.cfg.PersistGoal == nil {
		i.setGoalCommandError("goal: session persistence is unavailable")
		return
	}

	current := copySessionGoal(i.cfg.CurrentGoal())
	if len(parts) == 2 && strings.EqualFold(parts[1], "history") {
		if i.cfg.CurrentGoalHistory == nil {
			i.setGoalCommandError("goal: history is unavailable")
			return
		}
		history := i.cfg.CurrentGoalHistory()
		if len(history) == 0 {
			i.setGoalCommandStatus("no goal history")
			return
		}
		entries := make([]string, 0, len(history))
		for _, goal := range history {
			owner := goal.Owner
			if owner == "" {
				owner = core.GoalOwnerUser
			}
			entry := fmt.Sprintf("%s (%s): %s", goal.Status, owner, goal.Objective)
			if goal.Reason != "" {
				entry += " (" + goal.Reason + ")"
			}
			entries = append(entries, entry)
		}
		i.setGoalCommandStatus("goal history: " + strings.Join(entries, " → "))
		return
	}
	if len(parts) == 1 {
		if current == nil {
			i.setGoalCommandStatus("no autonomous goal")
			return
		}
		owner := current.Owner
		if owner == "" {
			owner = core.GoalOwnerUser
		}
		status := fmt.Sprintf("goal %s (%s): %s", current.Status, owner, current.Objective)
		if current.TokenBudget != nil {
			status += fmt.Sprintf(" (%d / %d tokens)", current.TokensUsed, *current.TokenBudget)
		} else if current.TokensUsed > 0 {
			status += fmt.Sprintf(" (%d tokens)", current.TokensUsed)
		}
		if current.Reason != "" {
			status += " (" + current.Reason + ")"
		}
		i.setGoalCommandStatus(status)
		return
	}

	startIfIdle := false
	startTitle := false
	arg := ""
	if len(parts) == 2 {
		arg = strings.ToLower(parts[1])
	}
	switch arg {
	case "pause":
		if current == nil {
			i.setGoalCommandError("goal: no goal to pause")
			return
		}
		current.Status = core.GoalPaused
		current.Reason = ""
		// An explicit pause is durable: drop any armed Esc return so the
		// next prompt cannot resume the goal. /goal resume restarts it.
		i.clearInterruptedGoalReturn()
	case "resume":
		if current == nil {
			i.setGoalCommandError("goal: no goal to resume")
			return
		}
		current.Status = core.GoalActive
		current.Reason = ""
		startIfIdle = true
	case "clear":
		if err := i.cfg.PersistGoal(nil); err != nil {
			i.setGoalCommandError("goal: " + err.Error())
			return
		}
		i.clearInterruptedGoalReturn()
		i.setGoalStatus(nil)
		i.setGoalCommandStatus("autonomous goal cleared")
		return
	default:
		objective := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cmd), parts[0]))
		if objective == "" {
			i.setGoalCommandError("usage: /goal <objective>|pause|resume|clear|history")
			return
		}
		current = &core.SessionGoal{Objective: objective, Status: core.GoalActive, Owner: core.GoalOwnerUser}
		if i.cfg.GoalMaxTokenBudget != nil {
			budget := *i.cfg.GoalMaxTokenBudget
			if budget > 0 {
				current.TokenBudget = &budget
			}
		}
		startIfIdle = true
		startTitle = true
	}

	if startIfIdle && !i.goalUpdatesAvailable() {
		i.setGoalCommandError("goal: update_goal is unavailable; enable the default tools")
		return
	}
	if err := i.cfg.PersistGoal(current); err != nil {
		i.setGoalCommandError("goal: " + err.Error())
		return
	}
	// An explicit resume or replacement supersedes Esc-armed reassessment:
	// the command owns the next wake, not the pending ordinary prompt.
	if startIfIdle {
		i.clearInterruptedGoalReturn()
		if i.agent != nil {
			i.agent.ResetRepetitionGuard()
		}
	}
	i.setGoalStatus(current)
	i.setGoalCommandStatus(fmt.Sprintf("autonomous goal %s: %s", current.Status, current.Objective))
	if startTitle {
		i.maybeStartSessionTitle(ctx, current.Objective)
	}
}

func (i *Interactive) setGoalStatus(goal *core.SessionGoal) {
	i.setGoalStatusWithAlert(goal, true)
}

func (i *Interactive) setGoalStatusWithAlert(goal *core.SessionGoal, notify bool) {
	status := core.GoalStatus("")
	if goal != nil {
		status = goal.Status
	}
	i.mu.Lock()
	if notify {
		i.alertGoalTransitionLocked(i.goalStatus, status)
	}
	i.goalStatus = status
	i.mu.Unlock()
	i.invalidate()
}

func (i *Interactive) setGoalCommandStatus(message string) {
	i.mu.Lock()
	i.statusOK = message
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
}

func (i *Interactive) setGoalCommandError(message string) {
	i.mu.Lock()
	i.statusOK = ""
	i.statusErr = message
	i.mu.Unlock()
	i.invalidate()
}

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
