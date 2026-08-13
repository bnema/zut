package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// SubagentStopTool lets the manager request termination of a stuck background
// sub-agent through the resident manager's cancellation lifecycle.
type SubagentStopTool struct {
	ResidentManager *subagents.ResidentManager
	Enabled         func() bool
}

type subagentStopArgs struct {
	AgentID string `json:"agent_id"`
}

type subagentActionResponse struct {
	Action string              `json:"action"`
	Agent  subagentStatusEntry `json:"agent"`
}

const subagentStopSchema = `{
  "type": "object",
  "properties": {
    "agent_id": {
      "type": "string",
		"description": "Child id or unique id prefix for the stuck resident sub-agent to terminate."
    }
  },
  "required": ["agent_id"]
}`

func (t *SubagentStopTool) Name() string { return SubagentStopToolName }

func (t *SubagentStopTool) Description() string {
	return "Request termination of a stuck resident sub-agent."
}

func (t *SubagentStopTool) Schema() json.RawMessage {
	return json.RawMessage(subagentStopSchema)
}

func (t *SubagentStopTool) Execute(ctx context.Context, raw json.RawMessage, _ func(string)) (core.ToolResult, error) {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return core.ToolResult{}, ctx.Err()
		default:
		}
	}
	prefix := t.Name()
	if t.ResidentManager == nil {
		return protocolToolError(prefix + ": subagent runtime not available in this mode")
	}
	if t.Enabled == nil || !t.Enabled() {
		return protocolToolError(prefix + ": subagent management is unavailable in this mode")
	}

	var args subagentStopArgs
	if err := decodeSubagentArgs(raw, &args); err != nil {
		return core.ToolResult{}, err
	}
	id := strings.TrimSpace(args.AgentID)
	if id == "" {
		return protocolToolError(prefix + ": agent_id is required")
	}
	snapshot, ok := findResidentStatusSnapshot(t.ResidentManager.Snapshot(), id)
	if !ok {
		return protocolToolError(fmt.Sprintf("%s: no such agent %q", prefix, id))
	}
	if err := t.ResidentManager.Stop(ctx, snapshot.ID); err != nil {
		return core.ToolResult{}, fmt.Errorf("%s: %w", prefix, err)
	}
	if updated, ok := t.ResidentManager.SnapshotFor(snapshot.ID); ok {
		snapshot = updated
	}
	return renderResidentAction("stop_requested", publicResidentStatus(snapshot))
}

func renderResidentAction(action string, entry subagentStatusEntry) (core.ToolResult, error) {
	response := subagentActionResponse{Action: action, Agent: entry}
	data, err := json.Marshal(response)
	if err != nil {
		return core.ToolResult{}, fmt.Errorf("subagent action: encode response: %w", err)
	}
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: string(data)}}, Details: response}, nil
}
