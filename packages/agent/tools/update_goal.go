package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

const (
	UpdateGoalToolName = "update_goal"
	updateGoalSchema   = `{"type":"object","properties":{"status":{"type":"string","enum":["active","complete","blocked"],"description":"Start a mission or set its next active goal, or mark the active goal complete or blocked."},"objective":{"type":"string","description":"Concrete objective for status active."},"mission_id":{"type":"string","description":"Optional identifier from the active-goal context. When supplied, it must match the current mission; omit it when starting or continuing the current mission."},"reason":{"type":"string","description":"Concise reason when the goal is blocked."}},"required":["status"],"additionalProperties":false}`
)

// UpdateGoalTool lets the main agent start a mission, set its next concrete
// active goal, or settle the current goal. Host-side persistence validates
// transitions within an existing session mission. A supplied mission ID is
// checked against the current mission; an omitted ID targets that mission.
type UpdateGoalTool struct{}

type updateGoalArgs struct {
	Status    string `json:"status"`
	Objective string `json:"objective,omitempty"`
	MissionID string `json:"mission_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// GoalUpdate is trusted metadata returned only by UpdateGoalTool.
type GoalUpdate struct {
	Status    core.GoalStatus
	Objective string
	MissionID string
	Reason    string
}

func (t *UpdateGoalTool) Name() string { return UpdateGoalToolName }

func (t *UpdateGoalTool) Description() string {
	return "Start your own mission when none is active, set the next concrete goal within an active mission, or mark the active goal complete or blocked. Do not broaden an existing mission."
}

func (t *UpdateGoalTool) Schema() json.RawMessage { return json.RawMessage(updateGoalSchema) }

func (t *UpdateGoalTool) Execute(_ context.Context, raw json.RawMessage, _ func(string)) (core.ToolResult, error) {
	var args updateGoalArgs
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return goalToolError("invalid arguments"), nil
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return goalToolError("invalid arguments"), nil
	}
	args.Status = strings.TrimSpace(args.Status)
	args.Objective = strings.TrimSpace(args.Objective)
	args.MissionID = strings.TrimSpace(args.MissionID)
	args.Reason = strings.TrimSpace(args.Reason)

	var update GoalUpdate
	switch args.Status {
	case "active":
		if args.Objective == "" {
			return goalToolError("objective is required when setting a goal"), nil
		}
		update.Status = core.GoalActive
		update.Objective = args.Objective
		update.MissionID = args.MissionID
	case "complete":
		update.Status = core.GoalDone
	case "blocked":
		if args.Reason == "" {
			return goalToolError("reason is required when blocking a goal"), nil
		}
		update.Status = core.GoalBlocked
		update.Reason = args.Reason
	default:
		return goalToolError("status must be active, complete, or blocked"), nil
	}

	text := "autonomous goal marked " + string(update.Status)
	if update.Reason != "" {
		text += ": " + update.Reason
	}
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: text}},
		Details: update,
	}, nil
}

// GoalUpdateFromResult extracts a valid update_goal state transition.
func GoalUpdateFromResult(result core.ToolResult) (GoalUpdate, bool) {
	if result.IsError {
		return GoalUpdate{}, false
	}
	update, ok := result.Details.(GoalUpdate)
	if !ok {
		return GoalUpdate{}, false
	}
	if update.Status != core.GoalActive && update.Status != core.GoalDone && update.Status != core.GoalBlocked {
		return GoalUpdate{}, false
	}
	if update.Status == core.GoalActive && strings.TrimSpace(update.Objective) == "" {
		return GoalUpdate{}, false
	}
	if update.Status == core.GoalBlocked && strings.TrimSpace(update.Reason) == "" {
		return GoalUpdate{}, false
	}
	return update, true
}

func goalToolError(message string) core.ToolResult {
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: message}},
		IsError: true,
	}
}
