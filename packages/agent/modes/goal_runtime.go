package modes

import (
	"fmt"
	"strings"

	agenttools "github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/google/uuid"
)

// A second empty continuation is enough to distinguish a model that briefly
// paused to summarize from one that is only acknowledging the continuation
// prompt. This is a progress invariant, not a resource limit: productive
// autonomous goals remain unbounded unless the user configures a budget.
const maxConsecutiveGoalNoProgressTurns = 2

type goalContinuationRun struct {
	id      string
	goalID  string
	tokens  uint64
	hadTool bool
}

func goalTokenCount(usage provider.Usage) uint64 {
	// OutputTokens already includes provider-reported reasoning tokens. Cache
	// usage is separately billable and therefore included in the goal total.
	counts := []int{usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheWriteTokens}
	var total uint64
	for _, count := range counts {
		if count > 0 {
			total += uint64(count)
		}
	}
	return total
}

func (i *Interactive) limitGoalBeforeRun(goal *core.SessionGoal) bool {
	if goal == nil || goal.Status != core.GoalActive || goal.TokenBudget == nil || goal.TokensUsed < *goal.TokenBudget {
		return false
	}
	goal = copySessionGoal(goal)
	goal.Status = core.GoalBudgetLimited
	goal.Reason = "configured token budget exhausted"
	goal.ContinuationID = ""
	if i.cfg.PersistGoal == nil {
		i.ReportError(fmt.Errorf("goal persistence is unavailable"))
		return true
	}
	if err := i.cfg.PersistGoal(goal); err != nil {
		i.ReportError(fmt.Errorf("persist goal budget limit: %w", err))
		return true
	}
	i.setGoalStatus(goal)
	return true
}

func (i *Interactive) startGoalRun(goal *core.SessionGoal) (*goalContinuationRun, error) {
	if goal == nil || goal.Status != core.GoalActive || strings.TrimSpace(goal.Objective) == "" {
		return nil, nil
	}
	i.mu.Lock()
	if i.goalRun != nil {
		i.mu.Unlock()
		return nil, nil
	}
	run := &goalContinuationRun{id: uuid.NewString(), goalID: goal.ID}
	i.goalRun = run
	i.mu.Unlock()

	goal = copySessionGoal(goal)
	goal.ContinuationID = run.id
	if i.cfg.PersistGoalRuntime == nil {
		return run, nil
	}
	if err := i.cfg.PersistGoalRuntime(goal); err != nil {
		i.clearGoalRun(run.id)
		return nil, fmt.Errorf("persist goal continuation: %w", err)
	}
	return run, nil
}

func (i *Interactive) clearGoalRun(runID string) {
	i.mu.Lock()
	if i.goalRun != nil && i.goalRun.id == runID {
		i.goalRun = nil
	}
	i.mu.Unlock()
}

func (i *Interactive) abandonGoalRun(run *goalContinuationRun) {
	if run == nil {
		return
	}
	i.clearGoalRun(run.id)
	if i.cfg.CurrentGoal == nil || i.cfg.PersistGoalRuntime == nil {
		return
	}
	goal := copySessionGoal(i.cfg.CurrentGoal())
	if goal == nil || goal.ID != run.goalID || goal.ContinuationID != run.id {
		return
	}
	goal.ContinuationID = ""
	if err := i.cfg.PersistGoalRuntime(goal); err != nil {
		i.ReportError(fmt.Errorf("clear goal continuation: %w", err))
	}
}

func (i *Interactive) observeGoalRun(ev core.AgentEvent) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.goalRun == nil {
		return
	}
	switch e := ev.(type) {
	case core.EvUsage:
		i.goalRun.tokens += goalTokenCount(e.Usage)
	case core.EvToolCall:
		if e.Name != agenttools.UpdateGoalToolName {
			i.goalRun.hadTool = true
		}
	}
}

// finishGoalRun records accounting exactly once and returns whether the
// controller should attempt another autonomous continuation. A stale run is
// discarded without changing a newer goal.
func (i *Interactive) finishGoalRun() bool {
	i.mu.Lock()
	run := i.goalRun
	i.goalRun = nil
	i.mu.Unlock()
	if run == nil || i.cfg.CurrentGoal == nil {
		return false
	}
	goal := copySessionGoal(i.cfg.CurrentGoal())
	if goal == nil || goal.ID != run.goalID {
		return false
	}
	goal.ContinuationID = ""
	goal.TokensUsed += run.tokens

	persistRuntime := func() bool {
		if i.cfg.PersistGoalRuntime == nil {
			return true
		}
		if err := i.cfg.PersistGoalRuntime(goal); err != nil {
			i.ReportError(fmt.Errorf("persist goal runtime: %w", err))
			return false
		}
		i.setGoalStatus(goal)
		return true
	}
	if goal.Status != core.GoalActive {
		persistRuntime()
		return false
	}
	if goal.TokenBudget != nil && goal.TokensUsed >= *goal.TokenBudget {
		goal.Status = core.GoalBudgetLimited
		goal.Reason = "configured token budget exhausted"
		if i.cfg.PersistGoal == nil {
			i.ReportError(fmt.Errorf("goal persistence is unavailable"))
			return false
		}
		if err := i.cfg.PersistGoal(goal); err != nil {
			i.ReportError(fmt.Errorf("persist goal budget limit: %w", err))
			return false
		}
		i.setGoalStatus(goal)
		return false
	}
	if run.hadTool {
		goal.ConsecutiveNoProgressTurns = 0
	} else {
		goal.ConsecutiveNoProgressTurns++
		if goal.ConsecutiveNoProgressTurns >= maxConsecutiveGoalNoProgressTurns {
			goal.Status = core.GoalStalled
			goal.Reason = "agent ended consecutive continuations without a concrete action"
			if i.cfg.PersistGoal == nil {
				i.ReportError(fmt.Errorf("goal persistence is unavailable"))
				return false
			}
			if err := i.cfg.PersistGoal(goal); err != nil {
				i.ReportError(fmt.Errorf("persist stalled goal: %w", err))
				return false
			}
			i.setGoalStatus(goal)
			return false
		}
	}
	return persistRuntime()
}

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
