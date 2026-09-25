package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
)

// SubagentInterruptTool cancels only a sub-agent's running turn. Unlike stop,
// the child stays alive with its transcript, so a later resume continues with
// full context.
type SubagentInterruptTool struct {
	ResidentManager *subagents.ResidentManager
	Enabled         func() bool
}

type subagentInterruptArgs struct {
	AgentID string `json:"agent_id"`
}

// Name returns the shared facade name: this type is an internal
// implementation and is never registered on its own.
func (t *SubagentInterruptTool) Name() string { return SubagentToolName }

func (t *SubagentInterruptTool) Execute(ctx context.Context, raw json.RawMessage, _ func(string)) (core.ToolResult, error) {
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
	var args subagentInterruptArgs
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
	interrupted, err := t.ResidentManager.Interrupt(ctx, snapshot.ID)
	if err != nil {
		return protocolToolError(prefix + ": " + err.Error())
	}
	if !interrupted {
		return protocolToolError(fmt.Sprintf("%s: agent %q has no running turn to interrupt", prefix, snapshot.ID))
	}
	if updated, ok := t.ResidentManager.SnapshotFor(snapshot.ID); ok {
		snapshot = updated
	}
	return renderResidentAction("interrupt_requested", publicResidentStatus(snapshot))
}
