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

// SubagentStatusTool reports live state for background sub-agents without
// waiting for a child to finish. An omitted agent_id lists resident children;
// an agent_id queries one child.
// The result deliberately contains metadata only: it does not expose
// transcripts, result output, credentials, provider settings, or filesystem
// paths.
type SubagentStatusTool struct {
	ResidentManager *subagents.ResidentManager
	Enabled         func() bool
}

type subagentStatusArgs struct {
	AgentID string `json:"agent_id,omitempty"`
}

type subagentStatusResponse struct {
	Agent  *subagentStatusEntry  `json:"agent,omitempty"`
	Agents []subagentStatusEntry `json:"agents"`
}

type subagentStatusEntry struct {
	ID             string                    `json:"agent_id"`
	State          subagents.ResidentState   `json:"state"`
	Profile        string                    `json:"profile,omitempty"`
	Provider       string                    `json:"provider"`
	Model          string                    `json:"model"`
	Workspace      subagents.WorkspaceMode   `json:"workspace_mode,omitempty"`
	Required       bool                      `json:"required,omitempty"`
	OwnedElsewhere bool                      `json:"owned_elsewhere,omitempty"`
	Budget         *subagents.BudgetSnapshot `json:"budget,omitempty"`
	BudgetSource   string                    `json:"budget_source,omitempty"`
}

const subagentStatusSchema = `{
  "type": "object",
  "properties": {
    "agent_id": {
      "type": "string",
		"description": "Optional child id or unique id prefix. Omit it to list all resident sub-agents."
    }
  }
}`

func (t *SubagentStatusTool) Name() string { return SubagentStatusToolName }

func (t *SubagentStatusTool) Description() string {
	return "Query live status for one background sub-agent or list all visible workers without waiting for completion."
}

func (t *SubagentStatusTool) Schema() json.RawMessage {
	return json.RawMessage(subagentStatusSchema)
}

func (t *SubagentStatusTool) Execute(ctx context.Context, raw json.RawMessage, _ func(string)) (core.ToolResult, error) {
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
		return protocolToolError(prefix + ": subagent status is unavailable in this mode")
	}

	var args subagentStatusArgs
	if err := decodeSubagentArgs(raw, &args); err != nil {
		return core.ToolResult{}, err
	}
	snapshots := t.ResidentManager.Snapshot()
	id := strings.TrimSpace(args.AgentID)
	if id == "" {
		entries := make([]subagentStatusEntry, 0, len(snapshots))
		for _, snapshot := range snapshots {
			entries = append(entries, publicResidentStatus(snapshot))
		}
		return renderSubagentStatus(subagentStatusResponse{Agents: entries})
	}
	snapshot, ok := findResidentStatusSnapshot(snapshots, id)
	if !ok {
		return protocolToolError(fmt.Sprintf("%s: no such agent %q", prefix, id))
	}
	entry := publicResidentStatus(snapshot)
	return renderSubagentStatus(subagentStatusResponse{Agent: &entry})
}

func findResidentStatusSnapshot(snapshots []subagents.ResidentSnapshot, id string) (subagents.ResidentSnapshot, bool) {
	for _, snapshot := range snapshots {
		if snapshot.ID == id {
			return snapshot, true
		}
	}
	var match subagents.ResidentSnapshot
	hits := 0
	for _, snapshot := range snapshots {
		if strings.HasPrefix(snapshot.ID, id) {
			match = snapshot
			hits++
		}
	}
	return match, hits == 1
}

func publicResidentStatus(snapshot subagents.ResidentSnapshot) subagentStatusEntry {
	entry := subagentStatusEntry{ID: snapshot.ID, State: snapshot.State, Profile: snapshot.Profile, Provider: snapshot.Provider, Model: snapshot.Model, Workspace: snapshot.WorkspaceMode, Required: snapshot.Required, OwnedElsewhere: snapshot.OwnedElsewhere, BudgetSource: snapshot.BudgetSource}
	if snapshot.Budget.Limit > 0 {
		budget := snapshot.Budget
		entry.Budget = &budget
	}
	return entry
}

func renderSubagentStatus(response subagentStatusResponse) (core.ToolResult, error) {
	data, err := json.Marshal(response)
	if err != nil {
		return core.ToolResult{}, fmt.Errorf("%s: encode status: %w", "subagent_status", err)
	}
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: string(data)}},
		Details: response,
	}, nil
}
