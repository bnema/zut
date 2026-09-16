package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bnema/zut/packages/core"
)

// WorktreeTool exposes persistent worktree creation and lifecycle management
// through one public tool while keeping the implementations focused internally.
type WorktreeTool struct {
	Create *CreateWorktreeTool
	Manage *ManageWorktreesTool
}

const worktreeSchema = `{
  "type":"object",
  "properties":{
    "action":{"type":"string","enum":["create","list","cleanup"],"description":"Create a branch worktree, list repository worktrees, or remove explicitly selected safe worktrees."},
    "branch":{"type":"string","description":"New branch name. Required for create."},
    "paths":{"type":"array","items":{"type":"string"},"description":"Exact registered worktree paths. Required for cleanup."}
  },
  "required":["action"]
}`

func (t *WorktreeTool) Name() string { return "worktree" }
func (t *WorktreeTool) Description() string {
	return "Create, list, and safely clean up Git worktrees."
}
func (t *WorktreeTool) Schema() json.RawMessage { return json.RawMessage(worktreeSchema) }

func (t *WorktreeTool) Preview(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	action, delegated, err := parseWorktreeAction(raw)
	if err != nil {
		return core.ToolResult{}, err
	}
	switch action {
	case "create":
		if t.Create == nil {
			return core.ToolResult{}, errors.New("worktree: create action is unavailable")
		}
		result, err := t.Create.Preview(ctx, delegated)
		return result, worktreeError(err)
	case "list", "cleanup":
		if t.Manage == nil {
			return core.ToolResult{}, errors.New("worktree: management actions are unavailable")
		}
		result, err := t.Manage.Preview(ctx, delegated)
		return result, worktreeError(err)
	default:
		return core.ToolResult{}, errors.New("worktree: unknown action")
	}
}

func (t *WorktreeTool) Execute(ctx context.Context, raw json.RawMessage, progress func(string)) (core.ToolResult, error) {
	action, delegated, err := parseWorktreeAction(raw)
	if err != nil {
		return core.ToolResult{}, err
	}
	switch action {
	case "create":
		if t.Create == nil {
			return core.ToolResult{}, errors.New("worktree: create action is unavailable")
		}
		result, err := t.Create.Execute(ctx, delegated, progress)
		return result, worktreeError(err)
	case "list", "cleanup":
		if t.Manage == nil {
			return core.ToolResult{}, errors.New("worktree: management actions are unavailable")
		}
		result, err := t.Manage.Execute(ctx, delegated, progress)
		return result, worktreeError(err)
	default:
		return core.ToolResult{}, errors.New("worktree: unknown action")
	}
}

func parseWorktreeAction(raw json.RawMessage) (string, json.RawMessage, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return "", nil, err
	}
	var action string
	if err := json.Unmarshal(values["action"], &action); err != nil || action == "" {
		return "", nil, errors.New("worktree: action is required")
	}
	delete(values, "action")
	if action == "list" || action == "cleanup" {
		encoded, err := json.Marshal(map[string]any{"action": action, "paths": rawField(values, "paths")})
		if err != nil {
			return "", nil, err
		}
		if action == "list" {
			encoded = json.RawMessage(`{"action":"list"}`)
		}
		return action, encoded, nil
	}
	encoded, err := json.Marshal(map[string]any{"branch": rawField(values, "branch")})
	return action, encoded, err
}

func worktreeError(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	for _, prefix := range []string{"create_worktree: ", "manage_worktrees: "} {
		if strings.HasPrefix(message, prefix) {
			message = strings.TrimPrefix(message, prefix)
			break
		}
	}
	return fmt.Errorf("worktree: %s", message)
}

func rawField(values map[string]json.RawMessage, key string) any {
	value, ok := values[key]
	if !ok {
		return nil
	}
	var decoded any
	if json.Unmarshal(value, &decoded) != nil {
		return nil
	}
	return decoded
}
