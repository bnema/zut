package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// SubagentSpawnTool lets the main agent fork a background sub-agent
// against the host's cwd via subagents.Supervisor.SpawnReq. The sub-agent runs
// in parallel: the tool returns the agent id immediately and the main
// turn continues uninterrupted. The user can monitor / chat with the
// spawned agent via /subagents.
//
// Available when the host's launch-time tool policy permits delegation. The
// primary-agent prompt decides whether delegation is proactive or only on an
// explicit user request.
type SubagentSpawnTool struct {
	// Supervisor is the supervisor used to spawn agents. Nil means subagents
	// are unavailable in this mode and the tool always errors.
	Supervisor *subagents.Supervisor

	// Enabled reports whether the host currently exposes this tool. When nil,
	// the tool is treated as disabled.
	Enabled func() bool

	// DefaultModel and DefaultProvider return the host agent's resolved
	// model and provider. They are used when the tool call omits both
	// fields and does not select a named profile, so auto-subagents follows
	// the same auth route as the user sees in the parent session.
	DefaultModel     func() string
	DefaultProvider  func() string
	DefaultReasoning func() string

	// ResolveSubagent validates and resolves a named markdown profile.
	// The child receives only the name and loads the profile itself.
	ResolveSubagent func(name string) (*subagents.Profile, error)

	// OnSpawned, if set, is called after every successful spawn with
	// the new agent + the task it was started with. Used by the
	// interactive host to track agents and surface a summary back
	// in chat when all sub-agents finish.
	OnSpawned func(agent *subagents.Agent, task string)
}

type subagentSpawnArgs struct {
	Task      string `json:"task"`
	Agent     string `json:"agent,omitempty"`
	Model     string `json:"model,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	FastMode  *bool  `json:"fast_mode,omitempty"`
	Isolation string `json:"isolation,omitempty"`
	Timeout   string `json:"timeout,omitempty"`
	MaxTurns  *int   `json:"max_turns,omitempty"`
}

const subagentSpawnSchemaTemplate = `{
  "type": "object",
  "properties": {
    "task": {
      "type": "string",
      "description": "The full task description for the sub-agent. Be specific: the child normally has the main agent's built-in tools, including lsp when enabled, but a selected profile can restrict its tools; it shares this working directory and starts with NO context from this conversation."
    },
    "agent": {
      "type": "string",
      "description": "Optional named markdown profile from [subagents_list]. The child applies that profile's system prompt, model, thinking level, tool limits, and fast-mode preference. Omit for a generic child."
    },
    "model": {
      "type": "string",
      "description": "Optional model id to pin the sub-agent to. Normally omit both model and provider so the sub-agent inherits the host session's resolved provider/model/auth route, or omit them when using an agent profile. Do not infer provider from model name. If you override this, also provide provider."
    },
    "provider": {
      "type": "string",
      "description": "Optional provider id. Normally omit both model and provider so the sub-agent inherits the host session. If you override this, also provide model. Note: openai means public OpenAI API-key auth; openai-codex means ChatGPT/Codex subscription auth."
    },
    "reasoning": {
      "type": "string",
      "enum": ["off", "minimum", "low", "medium", "high", "xhigh", "max"],
      "description": "Optional reasoning/thinking level for the child. Overrides the selected profile's thinking level when provided."
    },
    "thinking": {
      "type": "string",
      "enum": ["off", "minimum", "low", "medium", "high", "xhigh", "max"],
      "description": "Alias for reasoning, accepted for compatibility with common agent profile terminology. Prefer reasoning when both are available."
    },
    "fast_mode": {
      "type": "boolean",
      "description": "Optional fast-mode override for the child. Omit to inherit the selected profile or host setting."
    },
    "isolation": {
      "type": "string",
      "enum": ["shared", "worktree"],
      "description": "Workspace mode. Shared preserves existing behavior; worktree captures a patch without merging it."
    },
    "timeout": {
      "type": "string",
      "description": "Optional Go duration such as 20m."
    },
    "max_turns": {
      "type": "integer",
      "minimum": 1,
      "maximum": %d,
      "description": "Optional maximum prompt-level turns for this worker. Omit to use the supervisor default; the effective policy allows 1 through %d."
    }
  },
  "required": ["task"]
}`

func (t *SubagentSpawnTool) Name() string { return "subagent_spawn" }
func (t *SubagentSpawnTool) Description() string {
	return "Spawn a background sub-agent to work on a parallel sub-task. Optionally select a named markdown profile, model/provider/reasoning, timeout, turn limit, and shared/worktree isolation, and fast-mode preference. Returns the sub-agent id immediately. Completion is host-event-driven: wait only for the injected [auto-subagents update]; never use bash sleep, watch, tail -f, polling loops, repeated subagent_status, or dashboard/metadata/event-log/file checks solely to wait. Work on unrelated independent tasks or end/yield your turn. Legitimate waits inside user-requested commands, provider flows, extensions, or tests are allowed."
}
func (t *SubagentSpawnTool) Schema() json.RawMessage {
	maxTurns := 3
	if t.Supervisor != nil {
		if limit := t.Supervisor.MaxTurns(); limit > 0 {
			maxTurns = limit
		}
	}
	return json.RawMessage(fmt.Sprintf(subagentSpawnSchemaTemplate, maxTurns, maxTurns))
}

func (t *SubagentSpawnTool) Execute(ctx context.Context, raw json.RawMessage, progress func(string)) (core.ToolResult, error) {
	prefix := t.Name()
	if t.Supervisor == nil {
		return protocolToolError(prefix + ": subagent supervisor not available in this mode")
	}
	if t.Enabled == nil || !t.Enabled() {
		return protocolToolError(prefix + ": subagent delegation is unavailable in this mode")
	}
	var a subagentSpawnArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return core.ToolResult{}, fmt.Errorf("invalid args: %w", err)
	}
	task := strings.TrimSpace(a.Task)
	if task == "" {
		return protocolToolError(prefix + ": task is required")
	}

	workspaceMode := subagents.WorkspaceShared
	if value := strings.TrimSpace(a.Isolation); value != "" {
		workspaceMode = subagents.WorkspaceMode(value)
		if workspaceMode != subagents.WorkspaceShared && workspaceMode != subagents.WorkspaceWorktree {
			return protocolToolError(prefix + ": isolation must be shared or worktree")
		}
	}
	var timeout time.Duration
	if value := strings.TrimSpace(a.Timeout); value != "" {
		parsed, parseErr := time.ParseDuration(value)
		if parseErr != nil || parsed <= 0 {
			return protocolToolError(prefix + ": timeout must be a positive duration")
		}
		timeout = parsed
	}
	if a.MaxTurns != nil {
		if *a.MaxTurns < 1 {
			return protocolToolError(prefix + ": max_turns must be positive")
		}
		if limit := t.Supervisor.MaxTurns(); limit > 0 && *a.MaxTurns > limit {
			return protocolToolError(fmt.Sprintf("%s: max_turns must be 1 through %d; omit it to use the supervisor default", prefix, limit))
		}
	}

	agentName := strings.TrimSpace(a.Agent)
	var profile *subagents.Profile
	var fastModeOverride *bool
	fastModeFromProfile := false
	if agentName != "" {
		if t.ResolveSubagent == nil {
			return protocolToolError(prefix + ": named subagent profiles are unavailable")
		}
		var err error
		profile, err = t.ResolveSubagent(agentName)
		if err != nil {
			return protocolToolError(prefix + ": " + err.Error())
		}
		if profile == nil {
			return protocolToolError(prefix + ": unknown subagent profile " + agentName)
		}
		agentName = profile.Name
		fastModeOverride = profile.FastMode
		fastModeFromProfile = profile.FastMode != nil
	}
	if a.FastMode != nil {
		fastModeOverride = a.FastMode
		fastModeFromProfile = false
	}

	model := strings.TrimSpace(a.Model)
	providerID := strings.TrimSpace(a.Provider)
	var profileTools []string
	if profile != nil {
		profileTools = append([]string(nil), profile.Tools...)
	}
	if (model == "") != (providerID == "") {
		return protocolToolError(prefix + ": omit both model/provider to inherit the host or profile, or provide both explicitly")
	}
	if profile != nil {
		// A profile may specify a qualified model, a bare model, a provider,
		// or neither. Fill only missing pieces so explicit spawn options win;
		// the parent session supplies whatever the profile leaves unspecified.
		profileProvider, profileModel := profile.ModelSelection()
		if model == "" {
			model = profileModel
		}
		if providerID == "" {
			providerID = profileProvider
		}
	}
	if model == "" && t.DefaultModel != nil {
		model = strings.TrimSpace(t.DefaultModel())
	}
	if providerID == "" && t.DefaultProvider != nil {
		providerID = strings.TrimSpace(t.DefaultProvider())
	}

	reasoning, err := reasoningOverride(a.Reasoning, a.Thinking)
	if err != nil {
		return protocolToolError(prefix + ": " + err.Error())
	}
	if reasoning == "" && profile != nil && strings.TrimSpace(profile.Thinking) != "" {
		reasoning, err = reasoningOverride(profile.Thinking, "")
		if err != nil {
			return protocolToolError(prefix + ": profile " + profile.Name + ": " + err.Error())
		}
	}
	if reasoning == "" && t.DefaultReasoning != nil {
		reasoning, err = reasoningOverride(t.DefaultReasoning(), "")
		if err != nil {
			return protocolToolError(prefix + ": host " + err.Error())
		}
	}

	maxTurns := 0
	if a.MaxTurns != nil {
		maxTurns = *a.MaxTurns
	}
	agent, err := t.Supervisor.SpawnReq(ctx, subagents.SpawnRequest{
		Task:          task,
		Model:         model,
		Provider:      providerID,
		Reasoning:     reasoning,
		FastMode:      fastModeOverride,
		Subagent:      agentName,
		Timeout:       timeout,
		MaxTurns:      maxTurns,
		WorkspaceMode: workspaceMode,
		Tools:         profileTools,
	})
	if err != nil {
		return core.ToolResult{}, fmt.Errorf("%s: %w", prefix, err)
	}
	if t.OnSpawned != nil {
		t.OnSpawned(agent, task)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "spawned sub-agent %s\n", agent.ID)
	fmt.Fprintf(&sb, "state: %s/%s\n", agent.ProcessState(), agent.TurnState())
	fmt.Fprintf(&sb, "workspace: %s\n", agent.WorkspaceMode)
	fmt.Fprintf(&sb, "task: %s\n", truncateTask(task, 200))
	if agentName != "" {
		fmt.Fprintf(&sb, "agent: %s\n", agentName)
	}
	if model != "" {
		fmt.Fprintf(&sb, "model: %s\n", model)
	}
	if providerID != "" {
		fmt.Fprintf(&sb, "provider: %s\n", providerID)
	}
	if reasoning != "" {
		fmt.Fprintf(&sb, "reasoning: %s\n", reasoning)
	}
	if agent.FastMode {
		sb.WriteString("fast mode: enabled\n")
	}
	if fastModeFromProfile && fastModeOverride != nil && *fastModeOverride && agent.FastModeOverridesHost() {
		sb.WriteString("warning: subagent profile has fast mode enabled, overriding global fast mode off\n")
	}
	sb.WriteString("\nThe sub-agent is running in the background. Completion is host-event-driven: wait for the injected [auto-subagents update], the only completion signal. ")
	sb.WriteString("Never use bash sleep, watch, tail -f, polling loops, repeated subagent_status, or dashboard/metadata/event-log/file checks solely to wait. ")
	sb.WriteString("Work on unrelated independent tasks; otherwise end or yield your turn. Legitimate waits inside user-requested commands, provider flows, extensions, or tests are allowed.")
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: sb.String()}},
		Details: map[string]any{
			"agent_id":      agent.ID,
			"task":          task,
			"agent":         agentName,
			"model":         model,
			"provider":      providerID,
			"reasoning":     reasoning,
			"fast_mode":     agent.FastMode,
			"isolation":     string(workspaceMode),
			"timeout":       agent.Timeout.String(),
			"max_turns":     agent.MaxTurns,
			"state":         agent.Status(),
			"process_state": string(agent.ProcessState()),
			"turn_state":    string(agent.TurnState()),
			"result_ref":    subagents.ResultRef(agent.ID),
		},
	}, nil
}

func reasoningOverride(reasoning, thinking string) (string, error) {
	reasoning = strings.TrimSpace(reasoning)
	thinking = strings.TrimSpace(thinking)
	if reasoning != "" && thinking != "" && provider.NormalizeReasoning(reasoning) != provider.NormalizeReasoning(thinking) {
		return "", fmt.Errorf("reasoning and thinking disagree; provide only one")
	}
	value := reasoning
	if value == "" {
		value = thinking
	}
	if value == "" {
		return "", nil
	}
	normalized := provider.NormalizeReasoning(value)
	switch normalized {
	case "":
		return "off", nil
	case "minimum", "low", "medium", "high", "xhigh", "max":
		return normalized, nil
	default:
		return "", fmt.Errorf("reasoning must be off|minimum|low|medium|high|xhigh|max")
	}
}

// protocolToolError keeps model-visible validation failures in the
// ToolResult channel rather than treating them as host execution errors.
func protocolToolError(msg string) (core.ToolResult, error) {
	//nolint:nilerr // ToolResult.IsError is the established tool protocol.
	return toolErr(msg), nil
}

func toolErr(msg string) core.ToolResult {
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: msg}},
		IsError: true,
	}
}

func truncateTask(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
