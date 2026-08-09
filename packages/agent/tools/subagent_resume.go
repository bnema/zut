package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
)

// SubagentResumeTool gives a sub-agent a follow-up turn while preserving its
// existing session context. Every explicit follow-up starts a fresh max_turns
// budget. For an idle live worker, the follow-up is delivered directly; for an
// active live worker, it is queued for the next available message turn; for a
// terminal worker, the supervisor restarts the retained session with the
// follow-up as its initial turn.
type SubagentResumeTool struct {
	Supervisor *subagents.Supervisor
	Enabled    func() bool

	// BeforeResumed, when set, registers the follow-up before the supervisor
	// sends or queues its prompt. It returns a cleanup for resume failures that
	// happen before delivery. The interactive host uses this to avoid losing a
	// fast idle or restarted-worker turn.
	BeforeResumed func(agent *subagents.Agent, prompt string) func()

	// OnResumed receives the worker and follow-up after a successful resume.
	// It remains available for callers that only need post-success
	// notification; tracking callers should prefer BeforeResumed.
	OnResumed func(agent *subagents.Agent, prompt string)
}

type subagentResumeArgs struct {
	AgentID string `json:"agent_id"`
	Prompt  string `json:"prompt"`
}

const subagentResumeSchema = `{
  "type": "object",
  "properties": {
    "agent_id": {
      "type": "string",
      "description": "Worker id or unique id prefix for the sub-agent to continue."
    },
    "prompt": {
      "type": "string",
      "description": "New manager follow-up for the sub-agent. Its earlier task and conversation remain available in the retained session."
    }
  },
  "required": ["agent_id", "prompt"]
}`

func (t *SubagentResumeTool) Name() string { return "subagent_resume" }

func (t *SubagentResumeTool) Description() string {
	return "Continue a sub-agent with a fresh turn budget and its retained session; deliver immediately when idle, queue while active, or restart a stopped worker."
}

func (t *SubagentResumeTool) Schema() json.RawMessage {
	return json.RawMessage(subagentResumeSchema)
}

func (t *SubagentResumeTool) Execute(ctx context.Context, raw json.RawMessage, _ func(string)) (core.ToolResult, error) {
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

	var args subagentResumeArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return core.ToolResult{}, fmt.Errorf("invalid args: %w", err)
	}
	id := strings.TrimSpace(args.AgentID)
	if id == "" {
		return protocolToolError(prefix + ": agent_id is required")
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return protocolToolError(prefix + ": prompt is required")
	}
	snapshot, ok := findSubagentStatusSnapshot(t.Supervisor.SnapshotAll(), id)
	if !ok {
		return protocolToolError(fmt.Sprintf("%s: no such agent %q", prefix, id))
	}
	resumed, err := t.Supervisor.ResumeWithPromptBefore(ctx, snapshot.ID, args.Prompt, t.BeforeResumed)
	if err != nil {
		return protocolToolError(prefix + ": " + err.Error())
	}
	if t.BeforeResumed == nil && t.OnResumed != nil {
		t.OnResumed(resumed, args.Prompt)
	}

	snapshot, ok = findSubagentStatusSnapshot(t.Supervisor.SnapshotAll(), snapshot.ID)
	if !ok {
		return core.ToolResult{}, fmt.Errorf("%s: resumed agent disappeared from supervisor", prefix)
	}
	return renderSubagentAction("resumed", snapshot)
}
