package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/bnema/zut/packages/core"
)

// SubagentTool is the single model-facing subagent tool. One call carries one
// action; each action delegates to an internal implementation, and a nil
// implementation means that action is unavailable under the launch-time policy.
type SubagentTool struct {
	Spawn  *SubagentSpawnTool
	Status *SubagentStatusTool
	Stop   *SubagentStopTool
	Resume *SubagentResumeTool
}

// subagentSpawnGuidance is the operational contract for delegation. It is the
// facade description tail and the internal spawn tool description, so the text
// cannot drift between them.
const subagentSpawnGuidance = "Delegate a concrete, bounded scope to a resident sub-agent. For proactive delegation, use an independent sidecar only when the parent has useful non-overlapping work; keep immediate blockers local. A worker owns its scope until completion, so never duplicate it in the parent. If delegation owns the blocking task, end or yield the parent turn. Omit wait to return immediately and receive completion through [auto-subagents update]; set wait to an explicit 1–300 second value only when this turn should wait for the initial task. Set required=true when the outcome is mandatory before the parent's terminal response; failures remain recoverable through the resume action. Never use bash sleep, watch, tail -f, polling loops, repeated status calls, dashboard, metadata, or file checks solely to wait."

// subagentSchema merges the four action schemas into the flat, model-facing
// schema the project keeps for provider compatibility. Every property is
// annotated with the action that owns it; agent_id carries one merged
// description because it means "omit to list" for status and "required" for
// stop and resume. Because the schema advertises every action's arguments at
// once, a field that belongs to a different action is reported instead of
// ignored, so a mis-selected action cannot silently run as another one. Fields
// that no action owns still reach the sub-tool, whose strict decoder rejects
// them. TestSubagentFacadeSchemaCoversActionFields keeps this schema in sync
// with subagentActionFields.
const subagentSchema = `{
  "type": "object",
  "properties": {
    "action": {
      "type": "string",
      "enum": ["spawn", "status", "stop", "resume"],
      "description": "One action per call. spawn delegates a new resident sub-agent. status reads bounded state for one child or lists the current set. stop requests termination of a stuck child. resume continues an existing child with a new prompt, keeping its session context, and can wait for that follow-up turn."
    },
    "task": {
      "type": "string",
      "description": "The full task description for the sub-agent. Assign a concrete, bounded scope and explicit ownership that does not overlap other active work. Be specific: the child normally has the main agent's built-in tools, including lsp when enabled, but a selected profile can restrict its tools; it starts with NO context from this conversation. Shared isolation uses this working directory; worktree isolation captures a patch without merging it. Required when action is spawn."
    },
    "agent": {
      "type": "string",
      "description": "Optional named markdown profile from [subagents_list]. The child applies that profile's system prompt, model, thinking level, tool limits, and fast-mode preference. Omit for a generic child. Only valid when action is spawn."
    },
    "model": {
      "type": "string",
      "description": "Optional model id to pin the sub-agent to. Normally omit both model and provider so the sub-agent inherits the host session's resolved provider/model/auth route, or omit them when using an agent profile. Do not infer provider from model name. If you override this, also provide provider. Only valid when action is spawn."
    },
    "provider": {
      "type": "string",
      "description": "Optional provider id. Normally omit both model and provider so the sub-agent inherits the host session. If you override this, also provide model. Note: openai means public OpenAI API-key auth; openai-codex means ChatGPT/Codex subscription auth. Only valid when action is spawn."
    },
    "reasoning": {
      "type": "string",
      "enum": ["off", "minimum", "low", "medium", "high", "xhigh", "max"],
      "description": "Optional reasoning level for the child. Overrides the selected profile's thinking level when provided. Only valid when action is spawn."
    },
    "fast_mode": {
      "type": "boolean",
      "description": "Optional fast-mode override for the child. Omit to inherit the selected profile or host setting. Only valid when action is spawn."
    },
    "required": {
      "type": "boolean",
      "description": "Set true when the parent must receive this delegated result before it can finish. The worker remains asynchronous and reports through a host completion update. A bounded wait expiring does not terminate the accepted child and must not be retried while it remains active. Terminal failure or cancellation remains unmet until a successful follow-up. An outcome unobserved across host restart requires explicit user reconciliation. Only valid when action is spawn."
    },
    "wait": {
      "type": "integer",
      "minimum": 1,
      "maximum": 300,
      "description": "Optional explicit number of seconds to wait for this sub-agent's initial task to finish. Omit to return immediately. The sub-agent continues in the background if this wait expires. With resume it waits for the accepted follow-up turn instead. Only valid when action is spawn or resume."
    },
    "isolation": {
      "type": "string",
      "enum": ["shared", "worktree"],
      "description": "Workspace mode. Shared preserves existing behavior; worktree captures a patch without merging it. Only valid when action is spawn."
    },
    "agent_id": {
      "type": "string",
      "description": "Child id or unique id prefix. With status, omit it to list all resident sub-agents. Required when action is stop or resume."
    },
    "include_result": {
      "type": "boolean",
      "description": "With status and agent_id, retrieve the saved terminal result without running the child. Only valid when action is status."
    },
    "prompt": {
      "type": "string",
      "description": "New manager follow-up for the sub-agent. Its earlier task and conversation remain available in the retained session. Required when action is resume. After a terminal failure, inspect its saved result first; resume continues the retained session without discarding progress or satisfying required work. Combine with wait to block on that follow-up turn."
    }
  },
  "required": ["action"]
}`

// subagentActionFields lists the arguments each action owns. A field owned by
// the selected action, or by no action, is forwarded; a field owned by another
// action is rejected before dispatch.
var subagentActionFields = map[string][]string{
	SubagentActionSpawn:  {"task", "agent", "model", "provider", "reasoning", "fast_mode", "required", "wait", "isolation"},
	SubagentActionStatus: {"agent_id", "include_result"},
	SubagentActionStop:   {"agent_id"},
	SubagentActionResume: {"agent_id", "prompt", "wait"},
}

var (
	errSubagentActionRequired = errors.New("subagent action is required")
	errSubagentActionType     = errors.New("subagent action must be a string")
	errSubagentActionUnknown  = errors.New("subagent action is unknown")
)

// subagentForeignFieldError reports an argument that belongs only to other
// actions than the one the caller selected.
type subagentForeignFieldError struct {
	field  string
	action string
	owners []string
}

func (e *subagentForeignFieldError) Error() string {
	if len(e.owners) == 1 {
		return fmt.Sprintf("subagent: %s is not valid for action %s; it belongs to action %s", e.field, e.action, e.owners[0])
	}
	return fmt.Sprintf("subagent: %s is not valid for action %s; it belongs to the %s actions", e.field, e.action, strings.Join(e.owners, ", "))
}

func (t *SubagentTool) Name() string { return SubagentToolName }
func (t *SubagentTool) Description() string {
	return "Spawn, inspect, resume, and stop resident sub-agents. One action per call.\n\n" + subagentSpawnGuidance
}
func (t *SubagentTool) Schema() json.RawMessage { return json.RawMessage(subagentSchema) }

// ConfirmationKey implements core.ConfirmationKeyer. The facade multiplexes
// four actions under one tool name, so the session "always allow" grant is
// scoped per action instead of per name. A missing, empty, or non-string
// action returns the plain base name so a malformed call cannot widen the
// remembered grant.
func (t *SubagentTool) ConfirmationKey(args json.RawMessage) string {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(args, &values); err != nil {
		return SubagentToolName
	}
	var action string
	if err := json.Unmarshal(values["action"], &action); err != nil {
		return SubagentToolName
	}
	action = strings.TrimSpace(action)
	if action == "" {
		return SubagentToolName
	}
	return SubagentToolName + ":" + action
}

var _ core.ConfirmationKeyer = (*SubagentTool)(nil)

func (t *SubagentTool) Execute(ctx context.Context, raw json.RawMessage, progress func(string)) (core.ToolResult, error) {
	action, payload, err := parseSubagentAction(raw)
	switch {
	case errors.Is(err, errSubagentActionRequired):
		return protocolToolError("subagent: action is required")
	case errors.Is(err, errSubagentActionType):
		return protocolToolError("subagent: action must be a string")
	case errors.Is(err, errSubagentActionUnknown):
		return protocolToolError("subagent: unknown action")
	}
	var foreign *subagentForeignFieldError
	if errors.As(err, &foreign) {
		return protocolToolError(foreign.Error())
	}
	if err != nil {
		return core.ToolResult{}, err
	}
	switch action {
	case SubagentActionSpawn:
		if t.Spawn == nil {
			return protocolToolError("subagent: spawn action is unavailable in this mode")
		}
		return t.Spawn.Execute(ctx, payload, progress)
	case SubagentActionStatus:
		if t.Status == nil {
			return protocolToolError("subagent: status action is unavailable in this mode")
		}
		return t.Status.Execute(ctx, payload, progress)
	case SubagentActionStop:
		if t.Stop == nil {
			return protocolToolError("subagent: stop action is unavailable in this mode")
		}
		return t.Stop.Execute(ctx, payload, progress)
	case SubagentActionResume:
		if t.Resume == nil {
			return protocolToolError("subagent: resume action is unavailable in this mode")
		}
		return t.Resume.Execute(ctx, payload, progress)
	}
	return protocolToolError("subagent: unknown action")
}

// parseSubagentAction validates the action and returns the payload that action
// owns. Raw field bytes are forwarded unchanged so argument validation and
// number handling stay in the sub-tools.
func parseSubagentAction(raw json.RawMessage) (string, json.RawMessage, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return "", nil, fmt.Errorf("invalid args: %w", err)
	}
	rawAction, present := values["action"]
	if !present {
		return "", nil, errSubagentActionRequired
	}
	var action string
	if err := json.Unmarshal(rawAction, &action); err != nil {
		return "", nil, fmt.Errorf("%w: %v", errSubagentActionType, err)
	}
	action = strings.TrimSpace(action)
	if action == "" {
		return "", nil, errSubagentActionRequired
	}
	if _, ok := subagentActionFields[action]; !ok {
		return "", nil, errSubagentActionUnknown
	}
	delete(values, "action")
	// A field can belong to several actions (agent_id serves status, stop and
	// resume), so collect every owner before deciding.
	owners := make(map[string][]string, len(subagentActionFields))
	for ownerAction, fields := range subagentActionFields {
		for _, name := range fields {
			owners[name] = append(owners[name], ownerAction)
		}
	}
	payload := make(map[string]json.RawMessage, len(values))
	for name, value := range values {
		fieldOwners, claimed := owners[name]
		if claimed && !slices.Contains(fieldOwners, action) {
			sort.Strings(fieldOwners)
			return "", nil, &subagentForeignFieldError{field: name, action: action, owners: fieldOwners}
		}
		payload[name] = value
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", nil, err
	}
	return action, encoded, nil
}
