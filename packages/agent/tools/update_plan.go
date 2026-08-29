package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

const (
	UpdatePlanToolName = "update_plan"
	updatePlanSchema   = `{"type":"object","properties":{"explanation":{"type":"string","description":"Optional explanation for this plan update."},"plan":{"type":"array","description":"The list of steps.","items":{"type":"object","properties":{"step":{"type":"string","description":"Task step text."},"status":{"type":"string","enum":["pending","in_progress","completed"],"description":"Step status."}},"required":["step","status"],"additionalProperties":false}}},"required":["plan"],"additionalProperties":false}`
)

// UpdatePlanTool lets the model maintain a turn-scoped task checklist.
type UpdatePlanTool struct{}

type updatePlanArgs struct {
	Explanation *string           `json:"explanation"`
	Plan        *[]updatePlanStep `json:"plan"`
}

type updatePlanStep struct {
	Step   *string `json:"step"`
	Status *string `json:"status"`
}

func (t *UpdatePlanTool) Name() string { return UpdatePlanToolName }

func (t *UpdatePlanTool) Description() string {
	return "Track steps and progress for complex, ambiguous, or multi-phase work. Skip trivial tasks; keep the plan current with exactly one in_progress step until all steps are completed."
}

func (t *UpdatePlanTool) Schema() json.RawMessage { return json.RawMessage(updatePlanSchema) }

func (t *UpdatePlanTool) Execute(_ context.Context, raw json.RawMessage, _ func(string)) (core.ToolResult, error) {
	var args updatePlanArgs
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return updatePlanError(), nil
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF || args.Plan == nil {
		return updatePlanError(), nil
	}

	update := core.PlanUpdate{
		Explanation: args.Explanation,
		Plan:        make([]core.PlanStep, len(*args.Plan)),
	}
	for idx, step := range *args.Plan {
		if step.Step == nil || step.Status == nil {
			return updatePlanError(), nil
		}
		status := core.PlanStepStatus(*step.Status)
		switch status {
		case core.PlanPending, core.PlanInProgress, core.PlanCompleted:
		default:
			return updatePlanError(), nil
		}
		update.Plan[idx] = core.PlanStep{Step: *step.Step, Status: status}
	}

	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: "Plan updated"}},
		Details: update,
	}, nil
}

func updatePlanError() core.ToolResult {
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: "invalid arguments"}},
		IsError: true,
	}
}
