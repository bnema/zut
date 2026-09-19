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
	PlanToolName = "plan"
	planSchema   = `{
  "type":"object",
  "properties":{
    "action":{"type":"string","enum":["set","add","update","remove","clear","show"],"description":"Plan action to perform."},
    "explanation":{"type":"string","description":"Optional explanation for this plan change."},
    "steps":{"type":"array","description":"Steps for the set and add actions. set replaces the whole checklist and requires at least one step; add appends. Use clear to remove every step.","items":{"type":"object","properties":{"step":{"type":"string","description":"Task step text."},"status":{"type":"string","enum":["pending","in_progress","completed"],"description":"Step status, pending when omitted."}},"required":["step"],"additionalProperties":false}},
    "index":{"type":"integer","minimum":1,"description":"1-based step position for the update and remove actions."},
    "status":{"type":"string","enum":["pending","in_progress","completed"],"description":"New status for the update action."},
    "step":{"type":"string","description":"New step text for the update action."}
  },
  "required":["action"],
  "additionalProperties":false
}`
)

// PlanTool lets the model maintain a persisted, session-scoped task checklist
// through one CRUD surface. It only parses arguments: core owns the plan state,
// so validation against the current checklist and the model-visible result text
// happen after execution, where that state is available.
type PlanTool struct{}

var _ core.ToolArgumentRewritePolicy = (*PlanTool)(nil)

type planArgs struct {
	Action      *string       `json:"action"`
	Explanation *string       `json:"explanation"`
	Steps       []planArgStep `json:"steps"`
	Index       int           `json:"index"`
	Status      string        `json:"status"`
	Step        *string       `json:"step"`
}

type planArgStep struct {
	Step   *string `json:"step"`
	Status *string `json:"status"`
}

func (t *PlanTool) Name() string { return PlanToolName }

// AllowArgumentRewrite keeps the displayed checklist identical to the plan the
// model submitted. Guards may still allow or refuse the call.
func (t *PlanTool) AllowArgumentRewrite() bool { return false }

func (t *PlanTool) Description() string {
	return "Maintain a persisted task checklist. Actions: set (replace every step), add, update one step, remove one step, clear, show the current checklist. At most one step may be in_progress at a time; prefer add/update/remove deltas over re-sending the whole list with set."
}

func (t *PlanTool) Schema() json.RawMessage { return json.RawMessage(planSchema) }

func (t *PlanTool) Execute(_ context.Context, raw json.RawMessage, _ func(string)) (core.ToolResult, error) {
	var args planArgs
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return planError(), nil
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return planError(), nil
	}
	if args.Action == nil || *args.Action == "" {
		return planError(), nil
	}

	op := core.PlanOperation{
		Action:      *args.Action,
		Explanation: args.Explanation,
		Index:       args.Index,
		// Status and its values are validated by core, which rejects an unknown
		// status with an actionable message. The tool only carries the value.
		Status: core.PlanStepStatus(args.Status),
		Text:   args.Step,
	}
	for _, step := range args.Steps {
		if step.Step == nil {
			return planError(), nil
		}
		var status core.PlanStepStatus
		if step.Status != nil {
			status = core.PlanStepStatus(*step.Status)
		}
		op.Steps = append(op.Steps, core.PlanStep{Step: *step.Step, Status: status})
	}

	return core.ToolResult{
		// Placeholder content: core replaces it with the summary or listing
		// produced against the current plan.
		Content: []provider.Content{provider.TextBlock{Text: "Plan updated"}},
		Details: op,
	}, nil
}

func planError() core.ToolResult {
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: "invalid arguments"}},
		IsError: true,
	}
}
