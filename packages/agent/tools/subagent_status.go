package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// SubagentStatusTool reports live state for background sub-agents without
// waiting for a worker to finish. An omitted agent_id lists the workers
// visible to the current supervisor session; an agent_id queries one worker.
// The result deliberately contains metadata only: it does not expose
// transcripts, result output, credentials, provider settings, or filesystem
// paths.
type SubagentStatusTool struct {
	Supervisor *subagents.Supervisor
	Enabled    func() bool
}

type subagentStatusArgs struct {
	AgentID string `json:"agent_id,omitempty"`
}

type subagentStatusResponse struct {
	Agent  *subagentStatusEntry  `json:"agent,omitempty"`
	Agents []subagentStatusEntry `json:"agents"`
}

type subagentStatusEntry struct {
	ID              string                `json:"agent_id"`
	Operation       *statusOperation      `json:"operation,omitempty"`
	Terminal        string                `json:"terminal,omitempty"`
	LastEvent       *statusLastEvent      `json:"last_event,omitempty"`
	StartedAt       time.Time             `json:"started_at"`
	LifetimeTurns   int                   `json:"lifetime_turns"`
	CurrentRunTurns int                   `json:"current_run_turns"`
	TaskSummary     string                `json:"task_summary,omitempty"`
	Requirement     *statusRequirement    `json:"requirement,omitempty"`
	Result          *subagentStatusResult `json:"result,omitempty"`
}

type statusOperation struct {
	Type      string    `json:"type"`
	StartedAt time.Time `json:"started_at"`
}

type statusLastEvent struct {
	Type string    `json:"type"`
	At   time.Time `json:"at"`
}

type statusRequirement struct {
	State      string `json:"state"`
	TargetTurn int    `json:"target_turn"`
	Unmet      bool   `json:"unmet"`
	ErrorCode  string `json:"error_code,omitempty"`
}

type subagentStatusResult struct {
	State     string `json:"state"`
	Available bool   `json:"available"`
	Ref       string `json:"ref,omitempty"`
}

const subagentStatusSchema = `{
  "type": "object",
  "properties": {
    "agent_id": {
      "type": "string",
      "description": "Optional worker id or unique id prefix. Omit it to list all visible background sub-agents."
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
	if t.Supervisor == nil {
		return protocolToolError(prefix + ": subagent supervisor not available in this mode")
	}
	if t.Enabled == nil || !t.Enabled() {
		return protocolToolError(prefix + ": subagent status is unavailable in this mode")
	}

	var args subagentStatusArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return core.ToolResult{}, fmt.Errorf("invalid args: %w", err)
	}

	// SnapshotAll provides identity, session scope, and durable result facts.
	// The execution indication itself comes solely from the trace projection.
	snapshots := t.Supervisor.SnapshotAll()
	views := t.Supervisor.TraceViews()
	id := strings.TrimSpace(args.AgentID)
	if id == "" {
		entries := make([]subagentStatusEntry, 0, len(snapshots))
		for _, snapshot := range snapshots {
			entries = append(entries, publicSubagentStatus(snapshot, views[snapshot.ID]))
		}
		return renderSubagentStatus(subagentStatusResponse{Agents: entries})
	}

	snapshot, ok := findSubagentStatusSnapshot(snapshots, id)
	if !ok {
		return protocolToolError(fmt.Sprintf("%s: no such agent %q", prefix, id))
	}
	entry := publicSubagentStatus(snapshot, views[snapshot.ID])
	return renderSubagentStatus(subagentStatusResponse{Agent: &entry})
}

func findSubagentStatusSnapshot(snapshots []subagents.AgentSnapshot, id string) (subagents.AgentSnapshot, bool) {
	for _, snapshot := range snapshots {
		if snapshot.ID == id {
			return snapshot, true
		}
	}
	var match subagents.AgentSnapshot
	hits := 0
	for _, snapshot := range snapshots {
		if strings.HasPrefix(snapshot.ID, id) {
			match = snapshot
			hits++
		}
	}
	return match, hits == 1
}

func publicSubagentStatus(snapshot subagents.AgentSnapshot, traceViews ...subagents.AgentTraceView) subagentStatusEntry {
	var view subagents.AgentTraceView
	if len(traceViews) != 0 {
		view = traceViews[0]
	}
	entry := subagentStatusEntry{
		ID:              snapshot.ID,
		Terminal:        view.Terminal,
		StartedAt:       snapshot.Started,
		LifetimeTurns:   snapshot.LifetimeTurns,
		CurrentRunTurns: snapshot.CurrentRunTurns,
		TaskSummary:     summarizeSubagentTask(snapshot.Task),
	}
	if len(view.OpenOperations) != 0 {
		operation := view.OpenOperations[0]
		entry.Operation = &statusOperation{Type: operation.Type, StartedAt: operation.StartedAt}
	}
	if view.LastEvent.Type != "" {
		entry.LastEvent = &statusLastEvent{Type: view.LastEvent.Type, At: view.LastEvent.Timestamp}
	}
	if snapshot.Requirement.Required {
		entry.Requirement = &statusRequirement{
			State:      string(snapshot.Requirement.State),
			TargetTurn: snapshot.Requirement.TargetTurn,
			Unmet:      snapshot.Requirement.Unmet(),
			ErrorCode:  snapshot.Requirement.ErrorCode,
		}
	}
	if snapshot.Result != nil {
		entry.Result = &subagentStatusResult{
			State:     publicResultState(snapshot.Result.Status),
			Available: true,
			Ref:       snapshot.ResultRef,
		}
	}
	return entry
}

func publicResultState(status subagents.TurnStatus) string {
	switch status {
	case subagents.ResultSucceeded:
		return "completed"
	case subagents.ResultFailed:
		return "failed"
	case subagents.ResultCanceled:
		return "cancelled"
	default:
		return "unknown"
	}
}

// summarizeSubagentTask exposes only the first logical line and a small bound.
// This keeps the status surface useful for identifying a worker without
// returning the full prompt or any following context.
func summarizeSubagentTask(task string) string {
	firstLine := strings.TrimSpace(strings.SplitN(task, "\n", 2)[0])
	firstLine = strings.Join(strings.Fields(firstLine), " ")
	return truncateSubagentStatus(firstLine, 200)
}

func truncateSubagentStatus(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-3]) + "..."
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
