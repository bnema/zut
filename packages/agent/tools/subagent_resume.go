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
// existing session context. Every explicit follow-up is accepted durably and
// runs FIFO after any active resident turn.
type SubagentResumeTool struct {
	ResidentManager *subagents.ResidentManager
	Enabled         func() bool
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
		"description": "Child id or unique id prefix for the resident sub-agent to continue."
    },
    "prompt": {
      "type": "string",
      "description": "New manager follow-up for the sub-agent. Its earlier task and conversation remain available in the retained session."
    }
  },
  "required": ["agent_id", "prompt"]
}`

func (t *SubagentResumeTool) Name() string { return SubagentResumeToolName }

func (t *SubagentResumeTool) Description() string {
	return "Continue a resident sub-agent with a follow-up prompt and retained session."
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
	snapshot, ok := findResidentStatusSnapshot(t.ResidentManager.Snapshot(), id)
	if !ok {
		return protocolToolError(fmt.Sprintf("%s: no such agent %q", prefix, id))
	}
	if err := t.ResidentManager.Resume(ctx, snapshot.ID, args.Prompt); err != nil {
		return protocolToolError(prefix + ": " + err.Error())
	}
	return renderResidentAction("resumed", publicResidentStatus(snapshot))
}
