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

// SubagentResumeTool sends a sub-agent a follow-up while preserving its session
// context. In steer mode, the default, a follow-up to a running child joins its
// current turn at the next model-call boundary; otherwise it is accepted
// durably as a new turn that runs FIFO after any active one. An explicit
// bounded wait may return the answering turn's completion or expire while the
// child stays active.
type SubagentResumeTool struct {
	ResidentManager *subagents.ResidentManager
	Enabled         func() bool
}

type subagentResumeArgs struct {
	AgentID string `json:"agent_id"`
	Prompt  string `json:"prompt"`
	Mode    string `json:"mode,omitempty"`
	Wait    *int   `json:"wait,omitempty"`
}

// subagentResumeResponse reports how a follow-up was delivered.
type subagentResumeResponse struct {
	Action   string               `json:"action"`
	Delivery string               `json:"delivery"`
	Agent    subagentStatusEntry  `json:"agent"`
	Wait     *subagentWaitOutcome `json:"wait,omitempty"`
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
	mode := subagents.ResumeMode(strings.TrimSpace(args.Mode))
	if mode == "" {
		mode = subagents.ResumeSteer
	}
	if mode != subagents.ResumeSteer && mode != subagents.ResumeQueue {
		return protocolToolError(prefix + ": mode must be steer or queue")
	}
	if args.Wait != nil && (*args.Wait < 1 || *args.Wait > maxSubagentWaitSeconds) {
		return protocolToolError(fmt.Sprintf("%s: wait must be between 1 and %d seconds", prefix, maxSubagentWaitSeconds))
	}
	snapshot, ok := findResidentStatusSnapshot(t.ResidentManager.Snapshot(), id)
	if !ok {
		return protocolToolError(fmt.Sprintf("%s: no such agent %q", prefix, id))
	}
	// Each watch is registered before its turn can finish: the queued turn's
	// before acceptance, and the steered turn's from the child's control loop
	// before that turn can report completion.
	turnID := uuid.NewString()
	var completionResult <-chan subagents.ResidentCompletion
	cancelWait := func() {}
	var onSteered func(string)
	if args.Wait != nil {
		completionResult, cancelWait = t.ResidentManager.WatchCompletion(snapshot.ID, turnID)
		onSteered = func(activeTurn string) {
			cancelWait()
			completionResult, cancelWait = t.ResidentManager.WatchCompletion(snapshot.ID, activeTurn)
		}
	}
	defer func() { cancelWait() }()
	delivered, err := t.ResidentManager.ResumeFollowUp(ctx, snapshot.ID, args.Prompt, mode, turnID, onSteered)
	if err != nil {
		return protocolToolError(prefix + ": " + err.Error())
	}
	delivery := "queued"
	if delivered.Steered {
		delivery = "steered"
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
	return renderSubagentResponse(subagentResumeResponse{Action: "resumed", Delivery: delivery, Agent: publicResidentStatus(snapshot), Wait: outcome})
}
