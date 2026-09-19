package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/google/uuid"
)

// SubagentResumeTool gives a sub-agent a follow-up turn while preserving its
// existing session context. Every explicit follow-up is accepted durably and
// runs FIFO after any active resident turn. An explicit bounded wait may return
// that turn's completion or expire while the child stays active.
type SubagentResumeTool struct {
	ResidentManager *subagents.ResidentManager
	Enabled         func() bool
}

type subagentResumeArgs struct {
	AgentID string `json:"agent_id"`
	Prompt  string `json:"prompt"`
	Wait    *int   `json:"wait,omitempty"`
}

// Name returns the shared facade name: this type is an internal
// implementation and is never registered on its own.
func (t *SubagentResumeTool) Name() string { return SubagentToolName }

func (t *SubagentResumeTool) Execute(ctx context.Context, raw json.RawMessage, _ func(string)) (core.ToolResult, error) {
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

	var args subagentResumeArgs
	if err := decodeSubagentArgs(raw, &args); err != nil {
		return core.ToolResult{}, err
	}
	id := strings.TrimSpace(args.AgentID)
	if id == "" {
		return protocolToolError(prefix + ": agent_id is required")
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return protocolToolError(prefix + ": prompt is required")
	}
	if args.Wait != nil && (*args.Wait < 1 || *args.Wait > maxSubagentWaitSeconds) {
		return protocolToolError(fmt.Sprintf("%s: wait must be between 1 and %d seconds", prefix, maxSubagentWaitSeconds))
	}
	snapshot, ok := findResidentStatusSnapshot(t.ResidentManager.Snapshot(), id)
	if !ok {
		return protocolToolError(fmt.Sprintf("%s: no such agent %q", prefix, id))
	}
	// The watch is registered before the follow-up is accepted, so a turn that
	// finishes immediately cannot complete before this call subscribes.
	turnID := uuid.NewString()
	var completionResult <-chan subagents.ResidentCompletion
	cancelWait := func() {}
	if args.Wait != nil {
		completionResult, cancelWait = t.ResidentManager.WatchCompletion(snapshot.ID, turnID)
	}
	defer cancelWait()
	if err := t.ResidentManager.ResumeWithTurn(ctx, snapshot.ID, args.Prompt, turnID); err != nil {
		return protocolToolError(prefix + ": " + err.Error())
	}
	var outcome *subagentWaitOutcome
	if args.Wait != nil {
		completion, timedOut, err := waitForResidentCompletion(ctx, completionResult, *args.Wait, prefix)
		if err != nil {
			return core.ToolResult{}, err
		}
		outcome = &subagentWaitOutcome{Seconds: *args.Wait, TimedOut: timedOut}
		if !timedOut {
			result := completion.Completion()
			outcome.Status = result.Status
			outcome.Error = result.Error
			outcome.Summary = result.Summary
		}
	}
	if updated, ok := t.ResidentManager.SnapshotFor(snapshot.ID); ok {
		snapshot = updated
	}
	return renderResidentActionWait("resumed", publicResidentStatus(snapshot), outcome)
}
