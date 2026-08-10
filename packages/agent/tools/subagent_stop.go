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
// sub-agent. It uses the supervisor's graceful shutdown and force-stop
// lifecycle rather than directly manipulating the worker process.
type SubagentStopTool struct {
	Supervisor *subagents.Supervisor
	Enabled    func() bool

	// OnStopRequested registers non-blocking completion tracking before the
	// shutdown request is sent. The interactive host uses it to deliver the
	// terminal result even for workers restored by Reload.
	OnStopRequested func(agent *subagents.Agent)
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
      "description": "Worker id or unique id prefix for the stuck background sub-agent to terminate."
    }
  },
  "required": ["agent_id"]
}`

func (t *SubagentStopTool) Name() string { return SubagentStopToolName }

func (t *SubagentStopTool) Description() string {
	return "Request termination of a stuck background sub-agent. It gracefully requests shutdown, then cancels or force-stops the worker when necessary."
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
	if t.Supervisor == nil {
		return protocolToolError(prefix + ": subagent supervisor not available in this mode")
	}
	if t.Enabled == nil || !t.Enabled() {
		return protocolToolError(prefix + ": subagent management is unavailable in this mode")
	}

	var args subagentStopArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return core.ToolResult{}, fmt.Errorf("invalid args: %w", err)
	}
	id := strings.TrimSpace(args.AgentID)
	if id == "" {
		return protocolToolError(prefix + ": agent_id is required")
	}
	snapshot, ok := findSubagentStatusSnapshot(t.Supervisor.SnapshotAll(), id)
	if !ok {
		return protocolToolError(fmt.Sprintf("%s: no such agent %q", prefix, id))
	}
	if !subagentCanStop(snapshot) {
		return protocolToolError(fmt.Sprintf("%s: agent %s has no live worker to stop", prefix, snapshot.ID))
	}
	if t.OnStopRequested != nil {
		if agent := t.Supervisor.Get(snapshot.ID); agent != nil {
			t.OnStopRequested(agent)
		}
	}
	if err := t.Supervisor.StopContext(ctx, snapshot.ID); err != nil {
		return core.ToolResult{}, fmt.Errorf("%s: %w", prefix, err)
	}

	snapshot, ok = findSubagentStatusSnapshot(t.Supervisor.SnapshotAll(), snapshot.ID)
	if !ok {
		return core.ToolResult{}, fmt.Errorf("%s: terminated agent disappeared from supervisor", prefix)
	}
	return renderSubagentAction("stop_requested", snapshot)
}

func subagentCanStop(snapshot subagents.AgentSnapshot) bool {
	return snapshot.Status == subagents.StatusPending ||
		snapshot.Status == subagents.StatusRunning ||
		(snapshot.Status == subagents.StatusDetached && snapshot.ProcessState == subagents.ProcessAlive)
}

func renderSubagentAction(action string, snapshot subagents.AgentSnapshot) (core.ToolResult, error) {
	response := subagentActionResponse{
		Action: action,
		Agent:  publicSubagentStatus(snapshot),
	}
	data, err := json.Marshal(response)
	if err != nil {
		return core.ToolResult{}, fmt.Errorf("subagent action: encode response: %w", err)
	}
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: string(data)}},
		Details: response,
	}, nil
}
