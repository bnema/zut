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
	State           string                `json:"state"`
	ProcessState    string                `json:"process_state"`
	TurnState       string                `json:"turn_state"`
	LifetimeTurns   int                   `json:"lifetime_turns"`
	CurrentRunTurns int                   `json:"current_run_turns"`
	StartedAt       time.Time             `json:"started_at"`
	UpdatedAt       time.Time             `json:"updated_at"`
	FinishedAt      *time.Time            `json:"finished_at,omitempty"`
	TaskSummary     string                `json:"task_summary,omitempty"`
	Requirement     *statusRequirement    `json:"requirement,omitempty"`
	Result          *subagentStatusResult `json:"result,omitempty"`
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

func (t *SubagentStatusTool) Name() string { return "subagent_status" }

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

	// SnapshotAll is intentionally used for both list and single-worker
	// queries. Besides being non-blocking, it applies the supervisor's active
	// session visibility rules so an id lookup cannot bypass list scoping.
	snapshots := t.Supervisor.SnapshotAll()
	id := strings.TrimSpace(args.AgentID)
	if id == "" {
		entries := make([]subagentStatusEntry, 0, len(snapshots))
		for _, snapshot := range snapshots {
			entries = append(entries, publicSubagentStatus(snapshot))
		}
		return renderSubagentStatus(subagentStatusResponse{Agents: entries})
	}

	snapshot, ok := findSubagentStatusSnapshot(snapshots, id)
	if !ok {
		return protocolToolError(fmt.Sprintf("%s: no such agent %q", prefix, id))
	}
	entry := publicSubagentStatus(snapshot)
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

func publicSubagentStatus(snapshot subagents.AgentSnapshot) subagentStatusEntry {
	entry := subagentStatusEntry{
		ID:              snapshot.ID,
		State:           publicSubagentState(snapshot),
		ProcessState:    string(snapshot.ProcessState),
		TurnState:       string(snapshot.TurnState),
		LifetimeTurns:   snapshot.LifetimeTurns,
		CurrentRunTurns: snapshot.CurrentRunTurns,
		StartedAt:       snapshot.Started,
		UpdatedAt:       snapshot.UpdatedAt,
		TaskSummary:     summarizeSubagentTask(snapshot.Task),
	}
	if !snapshot.Finished.IsZero() {
		finished := snapshot.Finished
		entry.FinishedAt = &finished
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

func publicSubagentState(snapshot subagents.AgentSnapshot) string {
	switch snapshot.Status {
	case subagents.StatusPending:
		return "starting"
	case subagents.StatusRunning:
		return "running"
	case subagents.StatusDone:
		return "completed"
	case subagents.StatusFailed:
		return "failed"
	case subagents.StatusKilled:
		return "cancelled"
	case subagents.StatusDetached:
		return "detached"
	default:
		return "unknown"
	}
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
