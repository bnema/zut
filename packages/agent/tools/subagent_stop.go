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

// subagentWaitOutcome reports the result of an explicit bounded wait on one
// resident turn. An expired wait leaves the child active.
type subagentWaitOutcome struct {
	Seconds  int    `json:"seconds"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Status   string `json:"status,omitempty"`
	Error    string `json:"error,omitempty"`
	Summary  string `json:"summary,omitempty"`
}

type subagentActionResponse struct {
	Action string               `json:"action"`
	Agent  subagentStatusEntry  `json:"agent"`
	Wait   *subagentWaitOutcome `json:"wait,omitempty"`
}

// Name returns the shared facade name: this type is an internal
// implementation and is never registered on its own.
func (t *SubagentStopTool) Name() string { return SubagentToolName }

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
	return renderResidentActionWait(action, entry, nil)
}

// renderResidentActionWait renders an action response plus the outcome of an
// explicit bounded wait, if the caller requested one.
func renderResidentActionWait(action string, entry subagentStatusEntry, wait *subagentWaitOutcome) (core.ToolResult, error) {
	response := subagentActionResponse{Action: action, Agent: entry, Wait: wait}
	data, err := json.Marshal(response)
	if err != nil {
		return core.ToolResult{}, fmt.Errorf("subagent action: encode response: %w", err)
	}
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: string(data)}}, Details: response}, nil
}
