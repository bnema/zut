package session

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/google/uuid"
)

type goalRun struct {
	id, goalID string
	tokens     uint64
	hadTool    bool
}
type goalState struct {
	controller                        *Controller
	run                               *goalRun
	interruptedID, interruptedMission string
	armed, returning                  bool
	plan                              map[string]core.PlanUpdate
}

func cloneGoal(g *core.SessionGoal) *core.SessionGoal {
	if g == nil {
		return nil
	}
	v := *g
	if g.TokenBudget != nil {
		b := *g.TokenBudget
		v.TokenBudget = &b
	}
	return &v
}
func clonePlan(p map[string]core.PlanUpdate) map[string]core.PlanUpdate {
	out := make(map[string]core.PlanUpdate, len(p))
	for k, v := range p {
		v.Plan = append([]core.PlanStep(nil), v.Plan...)
		if v.Explanation != nil {
			text := *v.Explanation
			v.Explanation = &text
		}
		out[k] = v
	}
	return out
}
func (s *goalState) snapshotInto(out *Snapshot) {
	out.Plan = clonePlan(s.plan)
	if s.controller == nil || s.controller.deps.SessionStore == nil {
		return
	}
	store := s.controller.deps.SessionStore
	for _, g := range store.CurrentGoalHistory() {
		out.GoalHistory = append(out.GoalHistory, *cloneGoal(&g))
	}
	if g := store.CurrentGoal(); g != nil {
		out.Info.Goal = &GoalSummary{Objective: g.Objective, Status: string(g.Status)}
	}
}
func (c *Controller) currentGoal() *core.SessionGoal {
	c.goal.controller = c
	if c.deps.SessionStore == nil {
		return nil
	}
	return cloneGoal(c.deps.SessionStore.CurrentGoal())
}
func (c *Controller) saveGoal(g *core.SessionGoal, runtime bool) error {
	var err error
	if runtime {
		err = c.deps.SessionStore.PersistGoalRuntime(cloneGoal(g))
	} else {
		err = c.deps.SessionStore.PersistGoal(cloneGoal(g))
	}
	if err != nil {
		return err
	}
	status := ""
	if g != nil {
		status = string(g.Status)
	}
	c.publish(EvGoal, GoalEvent{Goal: cloneGoal(g), Status: status})
	return nil
}

// SetGoal persists an explicit goal lifecycle command before scheduling work.
func (c *Controller) SetGoal(ctx context.Context, command GoalCommand) error {
	var result error
	err := c.call(ctx, func() { result = c.setGoal(command) })
	if err != nil {
		return err
	}
	return result
}
func (c *Controller) setGoal(command GoalCommand) error {
	c.goal.controller = c
	if c.deps.SessionStore == nil {
		return errors.New("goal: session persistence is unavailable")
	}
	g := c.currentGoal()
	wake := false
	switch command.Op {
	case "set":
		objective := strings.TrimSpace(command.Objective)
		if objective == "" {
			return errors.New("goal: objective is required")
		}
		g = &core.SessionGoal{Objective: objective, Status: core.GoalActive, Owner: core.GoalOwnerUser}
		if b := c.deps.GoalMaxTokenBudget; b != nil && *b > 0 {
			v := *b
			g.TokenBudget = &v
		}
		wake = true
	case "clear":
		g = nil
	case "pause", "resume", "complete":
		if g == nil {
			return errors.New("goal: no goal")
		}
		g.Reason = ""
		switch command.Op {
		case "pause":
			g.Status = core.GoalPaused
		case "resume":
			g.Status = core.GoalActive
			wake = true
		case "complete":
			g.Status = core.GoalDone
		}
	default:
		return fmt.Errorf("goal: unknown operation %q", command.Op)
	}
	if err := c.saveGoal(g, false); err != nil {
		return err
	}
	c.goal.armed = false
	c.goal.returning = false
	if wake {
		return c.continueGoal()
	}
	return nil
}
func (c *Controller) continueGoal() error {
	g := c.currentGoal()
	if g == nil || g.Status != core.GoalActive || strings.TrimSpace(g.Objective) == "" || c.goal.armed || c.goal.run != nil || c.turnSeam.isBusy() || c.residentSeam.residentsRunning() {
		return nil
	}
	if g.TokenBudget != nil && g.TokensUsed >= *g.TokenBudget {
		g.Status = core.GoalBudgetLimited
		g.Reason = "configured token budget exhausted"
		g.ContinuationID = ""
		return c.saveGoal(g, false)
	}
	// A lease from a previous daemon has no associated running turn. Replacing
	// it here recovers the active goal without starting work during restore.
	run := &goalRun{id: uuid.NewString(), goalID: g.ID}
	g.ContinuationID = run.id
	if err := c.saveGoal(g, true); err != nil {
		return err
	}
	c.goal.run = run
	text := g.Objective + "\n\nContinue working autonomously toward this active goal. Do not stop at a progress update. Continue until the goal is complete or blocked, then call update_goal. If the current objective has become invalid and a replacement is necessary, you may supersede it only with a concrete objective inside the same mission; never use superseded to avoid difficult work or broaden scope."
	if g.ConsecutiveNoProgressTurns > 0 {
		text += " You ended the prior continuation without taking a concrete action. Inspect the current state and take the next action now; do not merely say that you will continue."
	}
	if g.ID != "" {
		text += " When superseding this goal, use goal_id " + g.ID + "."
	}
	if g.MissionID != "" {
		text += " When setting a next goal, use mission_id " + g.MissionID + " and keep it within the same user mission."
	}
	if err := c.turnSeam.startTurn(Input{Origin: InputOriginGoal, Text: text}); err != nil {
		c.goal.run = nil
		g.ContinuationID = ""
		if cleanup := c.saveGoal(g, true); cleanup != nil {
			return fmt.Errorf("%w (clear lease: %v)", err, cleanup)
		}
		return err
	}
	return nil
}
func (c *Controller) goalAfterTurn(end core.EvTurnEnd) {
	c.goal.controller = c
	run := c.goal.run
	c.goal.run = nil
	if run != nil {
		g := c.currentGoal()
		if g != nil && g.ID == run.goalID {
			g.ContinuationID = ""
			g.TokensUsed += run.tokens
			runtime := true
			if g.Status == core.GoalActive {
				if g.TokenBudget != nil && g.TokensUsed >= *g.TokenBudget {
					g.Status = core.GoalBudgetLimited
					g.Reason = "configured token budget exhausted"
					runtime = false
				} else if end.Err == nil && end.Stop != provider.StopLength && end.Stop != provider.StopAborted {
					if run.hadTool {
						g.ConsecutiveNoProgressTurns = 0
					} else {
						g.ConsecutiveNoProgressTurns++
					}
					if g.ConsecutiveNoProgressTurns >= 2 {
						g.Status = core.GoalStalled
						g.Reason = "agent ended consecutive continuations without a concrete action"
						runtime = false
					}
				}
			}
			if err := c.saveGoal(g, runtime); err != nil {
				c.goalError(err)
				return
			}
		}
	}
	if end.Err != nil || end.Stop == provider.StopAborted {
		return
	}
	c.goal.returning = false
	if err := c.continueGoal(); err != nil {
		c.goalError(err)
	}
}
func (c *Controller) goalError(err error) {
	c.publish(EvNote, NoteEvent{Note: Note{Level: "error", Text: err.Error()}})
}
func (c *Controller) goalIdle() bool {
	g := c.currentGoal()
	return g == nil || g.Status != core.GoalActive
}
func (s *goalState) observe(ev core.AgentEvent) {
	switch e := ev.(type) {
	case core.EvUsage:
		if s.run != nil {
			for _, n := range []int{e.Usage.InputTokens, e.Usage.OutputTokens, e.Usage.CacheReadTokens, e.Usage.CacheWriteTokens} {
				if n > 0 {
					s.run.tokens += uint64(n)
				}
			}
		}
	case core.EvToolCall:
		if s.run != nil && e.Name != "update_goal" {
			s.run.hadTool = true
		}
	case core.EvPlanUpdate:
		if s.plan == nil {
			s.plan = make(map[string]core.PlanUpdate)
		}
		s.plan[e.CallID] = clonePlan(map[string]core.PlanUpdate{e.CallID: e.Update})[e.CallID]
		if s.controller != nil {
			s.controller.publish(EvPlan, PlanEvent{Plan: clonePlan(s.plan)})
		}
	}
}

// goalOnAbort leaves the persisted goal active, but blocks autonomous wakes
// until the next ordinary user prompt has handled the interruption.
func (c *Controller) goalOnAbort() {
	g := c.currentGoal()
	if g == nil || g.Status != core.GoalActive {
		return
	}
	c.goal.interruptedID = g.ID
	c.goal.interruptedMission = g.MissionID
	c.goal.armed = true
	c.goal.returning = false
}

// goalBeforeTurn appends a hidden reassessment message before the next user
// prompt without changing the visible user input.
func (c *Controller) goalBeforeTurn(in *Input) {
	c.goal.controller = c
	if in == nil || in.Origin != InputOriginUser || !c.goal.armed {
		return
	}
	g := c.currentGoal()
	if g == nil || g.Status != core.GoalActive || strings.TrimSpace(g.Objective) == "" || g.ID != c.goal.interruptedID || g.MissionID != c.goal.interruptedMission {
		c.goal.armed = false
		return
	}
	c.goal.armed = false
	c.goal.returning = true
	text := "The previous turn was interrupted by the user; the autonomous goal below remains active. Handle the latest user message first. Classify it as changing, settling, blocking, or incidental to that goal. If it is incidental, answer it briefly and then continue the goal autonomously without waiting for the user to say continue. Call update_goal only for a genuine complete, blocked, or superseded transition.\n\nActive goal: " + g.Objective
	if g.ID != "" {
		text += " When superseding this goal, use goal_id " + g.ID + "."
	}
	if g.MissionID != "" {
		text += " When setting a next goal, use mission_id " + g.MissionID + " and keep it within the same user mission."
	}
	in.Hidden = append(in.Hidden, provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: text}},
		Meta:    map[string]string{"zut_goal_interrupt_reassess": "true"},
	})
}
