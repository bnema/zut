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
	updateGoalSchema   = `{"type":"object","properties":{"status":{"type":"string","enum":["complete","blocked"],"description":"Mark the active autonomous goal complete or blocked."},"reason":{"type":"string","description":"Concise reason when the goal is blocked."}},"required":["status"],"additionalProperties":false}`
)

// UpdateGoalTool lets the model stop an explicitly user-started autonomous
// goal. Starting, pausing, and resuming remain user-controlled.
type UpdateGoalTool struct{}

type updateGoalArgs struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// GoalUpdate is trusted metadata returned only by UpdateGoalTool.
type GoalUpdate struct {
	Status core.GoalStatus
	Reason string
}

func (t *UpdateGoalTool) Name() string { return UpdateGoalToolName }

func (t *UpdateGoalTool) Description() string {
	return "Mark the active autonomous goal complete or blocked. Only use this for a goal explicitly started by the user with /goal."
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
	args.Reason = strings.TrimSpace(args.Reason)

	var update GoalUpdate
	switch args.Status {
	case "complete":
		update.Status = core.GoalDone
	case "blocked":
		if args.Reason == "" {
			return goalToolError("reason is required when blocking a goal"), nil
		}
		update.Status = core.GoalBlocked
		update.Reason = args.Reason
	default:
		return goalToolError("status must be complete or blocked"), nil
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
	if update.Status != core.GoalDone && update.Status != core.GoalBlocked {
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
