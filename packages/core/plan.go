package core

import (
	"fmt"
	"strings"
	"time"
)

// PlanOperation is one parsed plan tool call. The tool decodes the arguments;
// core validates the operation against the current plan, materializes the
// resulting list, and only then commits it. Fields that do not apply to the
// selected action are ignored.
type PlanOperation struct {
	Action      string
	Explanation *string
	Steps       []PlanStep     // set, add
	Index       int            // update, remove (1-based)
	Text        *string        // update
	Status      PlanStepStatus // update
}

// SessionPlan is the agent-owned checklist persisted in session metadata.
// Steps is optional so older sessions remain compatible.
type SessionPlan struct {
	Steps []PlanStep `json:"steps,omitempty"`
	// Updated is stamped by Session.UpdatePlan when the plan is persisted.
	Updated time.Time `json:"updated"`
}

// SetPlan seeds the agent-owned plan. It copies the steps so the caller keeps
// ownership of its slice. Hosts seed state on resume and on /clear, where a nil
// agent is possible, so the guard is deliberate. Seeded steps are normalized, so
// a host cannot install state that the read action cannot report.
func (a *Agent) SetPlan(steps []PlanStep) {
	if a == nil {
		return
	}
	var seeded []PlanStep
	if normalized := normalizeSessionPlan(&SessionPlan{Steps: steps}); normalized != nil {
		seeded = clonePlanSteps(normalized.Steps)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.plan = seeded
}

// CurrentPlan returns a copy of the agent-owned plan. Mutating the result does
// not affect agent state.
func (a *Agent) CurrentPlan() []PlanStep {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return clonePlanSteps(a.plan)
}

// previewPlanOperation validates op against the current plan and returns the
// prospective update without mutating agent state. Callers commit a validated
// operation with commitPlanUpdate once persistence succeeds.
func (a *Agent) previewPlanOperation(op PlanOperation) (PlanUpdate, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.previewPlanOperationLocked(op)
}

// commitPlanUpdate stores the plan that was just persisted. Callers pass the
// value returned by previewPlanOperation after CommitToolResult succeeded, so
// agent state always matches what the session file holds.
//
// Preview and commit are separate lock windows. When a concurrent SetPlan lands
// between them, the persisted update wins: the session file now records that
// list, so state must agree with the file rather than with the intervening call.
func (a *Agent) commitPlanUpdate(update PlanUpdate) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.plan = clonePlanSteps(update.Plan)
}

func (a *Agent) previewPlanOperationLocked(op PlanOperation) (PlanUpdate, error) {
	switch op.Action {
	case "set", "add", "update", "remove", "clear", "show":
	case "":
		return PlanUpdate{}, fmt.Errorf("plan: action is required")
	default:
		return PlanUpdate{}, fmt.Errorf("plan: unknown action %q", op.Action)
	}

	plan := clonePlanSteps(a.plan)
	switch op.Action {
	case "set":
		// A replace must say what it replaces: an omitted or empty step list used
		// to wipe the checklist silently, which is too destructive a default for a
		// model that only meant to attach an explanation. Removing every step is an
		// explicit intent, so it has its own action.
		if len(op.Steps) == 0 {
			return PlanUpdate{}, fmt.Errorf("plan: steps are required for set (use clear to remove every step)")
		}
		plan = defaultPlanStepStatuses(clonePlanSteps(op.Steps))
	case "add":
		if len(op.Steps) == 0 {
			return PlanUpdate{}, fmt.Errorf("plan: steps are required for add")
		}
		plan = append(plan, defaultPlanStepStatuses(clonePlanSteps(op.Steps))...)
	case "update":
		if op.Index < 1 || op.Index > len(plan) {
			return PlanUpdate{}, fmt.Errorf("plan: index %d out of range (plan has %d steps)", op.Index, len(plan))
		}
		if op.Status == "" && op.Text == nil {
			return PlanUpdate{}, fmt.Errorf("plan: update requires status or step")
		}
		step := plan[op.Index-1]
		if op.Text != nil {
			step.Step = *op.Text
		}
		if op.Status != "" {
			step.Status = op.Status
		}
		plan[op.Index-1] = step
	case "remove":
		if op.Index < 1 || op.Index > len(plan) {
			return PlanUpdate{}, fmt.Errorf("plan: index %d out of range (plan has %d steps)", op.Index, len(plan))
		}
		plan = append(plan[:op.Index-1], plan[op.Index:]...)
		if len(plan) == 0 {
			plan = nil
		}
	case "clear":
		plan = nil
	case "show":
		// show is a read: report the current plan without validating it. Seeding
		// normalizes, but a read must not fail on state a host installed directly.
		return PlanUpdate{Explanation: op.Explanation, Plan: plan}, nil
	}

	if err := validatePlanSteps(plan); err != nil {
		return PlanUpdate{}, err
	}
	return PlanUpdate{Explanation: op.Explanation, Plan: plan}, nil
}

// validatePlanSteps rejects steps that cannot be rendered or persisted: empty
// text, an unknown status, or more than one in_progress step. There is
// deliberately no length cap.
func validatePlanSteps(plan []PlanStep) error {
	inProgress := 0
	for idx := range plan {
		if strings.TrimSpace(plan[idx].Step) == "" {
			return fmt.Errorf("plan: step text is required")
		}
		switch plan[idx].Status {
		case PlanPending, PlanCompleted:
		case PlanInProgress:
			inProgress++
		default:
			return fmt.Errorf("plan: unknown status %q", plan[idx].Status)
		}
	}
	if inProgress > 1 {
		return fmt.Errorf("plan: at most one step can be in_progress")
	}
	return nil
}

// defaultPlanStepStatuses applies the schema's optional step status: a step that
// omits it is pending. Without this, an omitted status would reach agent state
// as an invalid value and later be dropped by normalizeSessionPlan on reload.
func defaultPlanStepStatuses(steps []PlanStep) []PlanStep {
	for idx := range steps {
		if steps[idx].Status == "" {
			steps[idx].Status = PlanPending
		}
	}
	return steps
}

// planResultText renders the model-visible text for a validated plan
// operation. Mutations return a compact summary that never echoes the list;
// show returns the full checklist.
func planResultText(action string, update PlanUpdate) string {
	if action == "show" {
		return planShowListing(update.Plan)
	}
	return planMutationSummary(update.Plan)
}

// planMutationSummary summarizes the resulting plan without repeating its steps:
// the model already holds them in its own earlier messages.
func planMutationSummary(plan []PlanStep) string {
	if len(plan) == 0 {
		return "Plan cleared"
	}
	completed := 0
	inProgressIndex := 0
	for idx, step := range plan {
		switch step.Status {
		case PlanCompleted:
			completed++
		case PlanInProgress:
			inProgressIndex = idx + 1
		}
	}
	summary := fmt.Sprintf("Plan updated (%d/%d completed", completed, len(plan))
	if inProgressIndex > 0 {
		summary += fmt.Sprintf("; in progress: step %d", inProgressIndex)
	}
	return summary + ")"
}

// planShowListing renders the full checklist for the show action.
func planShowListing(plan []PlanStep) string {
	if len(plan) == 0 {
		return "Plan is empty"
	}
	var b strings.Builder
	for idx, step := range plan {
		if idx > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%d. [%s] %s", idx+1, planStepMarker(step.Status), step.Step)
	}
	return b.String()
}

// planStepMarker is the one-character checkbox shown for a step status.
func planStepMarker(status PlanStepStatus) string {
	switch status {
	case PlanCompleted:
		return "x"
	case PlanInProgress:
		return "~"
	default:
		return " "
	}
}

// clonePlanSteps returns an independent copy of steps, or nil when empty.
func clonePlanSteps(steps []PlanStep) []PlanStep {
	if len(steps) == 0 {
		return nil
	}
	clone := make([]PlanStep, len(steps))
	copy(clone, steps)
	return clone
}
