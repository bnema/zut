package modes

import (
	"context"
	"fmt"
	"strings"

	agenttools "github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

const goalContinueMetaKey = "zut_goal_continue"

func (i *Interactive) goalUpdatesAvailable() bool {
	ag := i.Agent()
	if ag == nil {
		return false
	}
	_, ok := ag.ToolsSnapshot()[agenttools.UpdateGoalToolName]
	return ok
}

func (i *Interactive) goalContinuationMessage() (provider.Message, bool) {
	if i.cfg.CurrentGoal == nil || !i.goalUpdatesAvailable() {
		return provider.Message{}, false
	}
	goal := i.cfg.CurrentGoal()
	if goal == nil || goal.Status != core.GoalActive || strings.TrimSpace(goal.Objective) == "" {
		return provider.Message{}, false
	}
	text := goal.Objective +
		"\n\nContinue working autonomously toward this active goal. Do not stop at a progress update. Continue until the goal is complete or blocked, then call update_goal."
	if goal.MissionID != "" {
		text += " When setting a next goal, use mission_id " + goal.MissionID + " and keep it within the same user mission."
	}
	return provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: text}},
		Meta:    map[string]string{goalContinueMetaKey: "true"},
	}, true
}

func (i *Interactive) goalIdleLocked() bool {
	return !i.busy && i.agent != nil && len(i.queued) == 0 && i.agent.QueuedMessageCount() == 0
}

func (i *Interactive) requestGoalContinuationIfIdle(parent context.Context) bool {
	message, ok := i.goalContinuationMessage()
	if !ok || i.coordinatorHasPendingWorkers() {
		return false
	}
	i.mu.Lock()
	if !i.goalIdleLocked() {
		i.mu.Unlock()
		return false
	}
	i.busy = true
	i.mu.Unlock()
	i.startGoalContinuation(parent, message)
	return true
}

func (i *Interactive) startGoalContinuation(parent context.Context, message provider.Message) {
	goalMessage, goalActive := i.goalContinuationMessage()
	i.mu.Lock()
	ag := i.agent
	if ag == nil {
		i.busy = false
		pendingIdleWork := i.takePendingIdleWorkLocked()
		i.mu.Unlock()
		runPendingIdleWork(pendingIdleWork)
		i.invalidate()
		return
	}
	if len(i.queued) > 0 {
		next := i.queued[0]
		i.queued = i.queued[1:]
		i.mu.Unlock()
		i.startTurn(parent, next)
		return
	}
	if ag.QueuedMessageCount() > 0 {
		i.mu.Unlock()
		i.startTurnRequest(parent, "", nil, true, false)
		return
	}
	if !goalActive || userMessageText(goalMessage) != userMessageText(message) {
		i.busy = false
		pendingIdleWork := i.takePendingIdleWorkLocked()
		i.mu.Unlock()
		runPendingIdleWork(pendingIdleWork)
		// The objective may have been replaced while the prior turn was
		// finishing. Re-evaluate it now instead of waiting for another event;
		// the idle gate still gives queued user work priority.
		i.requestGoalContinuationIfIdle(parent)
		i.invalidate()
		return
	}
	i.mu.Unlock()
	ag.AppendUserContext(userMessageText(message), message.Meta)

	// Persistence callbacks run synchronously from AppendUserContext, so never
	// hold the TUI mutex across it. Re-check user work that may have arrived
	// while the hidden continuation was being persisted.
	i.mu.Lock()
	if len(i.queued) > 0 {
		next := i.queued[0]
		i.queued = i.queued[1:]
		i.mu.Unlock()
		i.startTurn(parent, next)
		return
	}
	i.mu.Unlock()
	i.startTurnRequest(parent, "", nil, true, false)
}

func copySessionGoal(goal *core.SessionGoal) *core.SessionGoal {
	if goal == nil {
		return nil
	}
	copyGoal := *goal
	return &copyGoal
}

func (i *Interactive) updateActiveGoal(status core.GoalStatus, reason string) {
	if i.cfg.CurrentGoal == nil || i.cfg.PersistGoal == nil {
		return
	}
	goal := copySessionGoal(i.cfg.CurrentGoal())
	if goal == nil || goal.Status != core.GoalActive {
		return
	}
	goal.Status = status
	goal.Reason = reason
	if err := i.cfg.PersistGoal(goal); err != nil {
		i.ReportError(fmt.Errorf("persist goal: %w", err))
		return
	}
	i.setGoalStatus(goal)
}

func (i *Interactive) runGoalCommand(ctx context.Context, cmd string, parts []string) {
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
	i.setGoalStatus(current)
	i.setGoalCommandStatus(fmt.Sprintf("autonomous goal %s: %s", current.Status, current.Objective))
	if startTitle {
		i.maybeStartSessionTitle(ctx, current.Objective)
	}
	if startIfIdle {
		i.requestGoalContinuationIfIdle(ctx)
	}
}

// RefreshGoal reloads the status badge after the CLI commits a session switch.
func (i *Interactive) RefreshGoal() {
	if i.cfg.CurrentGoal == nil {
		i.setGoalStatus(nil)
		return
	}
	i.setGoalStatus(i.cfg.CurrentGoal())
}

func (i *Interactive) setGoalStatus(goal *core.SessionGoal) {
	status := core.GoalStatus("")
	if goal != nil {
		status = goal.Status
	}
	i.mu.Lock()
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
