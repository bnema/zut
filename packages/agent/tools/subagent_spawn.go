package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/google/uuid"
)

// SubagentSpawnTool lets the main agent delegate work through the resident
// manager. A spawn without wait returns after acceptance; an explicit bounded
// wait may return the initial completion or expire while the child stays active.
// Required work records a durable obligation that must be resolved before the
// parent can produce its terminal response.
//
// Available when the host's launch-time tool policy permits delegation. The
// primary-agent prompt decides whether delegation is proactive or only on an
// explicit user request.
const (
	SubagentSpawnToolName  = "subagent_spawn"
	SubagentStatusToolName = "subagent_status"
	SubagentStopToolName   = "subagent_stop"
	SubagentResumeToolName = "subagent_resume"

	maxSubagentWaitSeconds = 5 * 60
)

type SubagentSpawnTool struct {
	ResidentManager   *subagents.ResidentManager
	BuildResidentSpec func(context.Context, ResidentSpawnRequest) (subagents.ResidentChildSpec, error)
	OnResidentSpawned func(subagents.ResidentChildSpec, string)

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
	ResolveSubagent func(name string) (*subagents.Profile, error)
}

// ResidentSpawnRequest is the resolved tool input the host uses to construct
// a complete non-secret resident ChildSpec.
type ResidentSpawnRequest struct {
	Task          string
	Profile       *subagents.Profile
	Model         string
	Provider      string
	Reasoning     string
	FastMode      *bool
	Required      bool
	WorkspaceMode subagents.WorkspaceMode
}

type subagentSpawnArgs struct {
	Task      string `json:"task"`
	Agent     string `json:"agent,omitempty"`
	Model     string `json:"model,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
	FastMode  *bool  `json:"fast_mode,omitempty"`
	Required  bool   `json:"required,omitempty"`
	Wait      *int   `json:"wait,omitempty"`
	Isolation string `json:"isolation,omitempty"`
}

const subagentSpawnSchemaTemplate = `{
  "type": "object",
  "properties": {
    "task": {
      "type": "string",
      "description": "The full task description for the sub-agent. Assign a concrete, bounded scope and explicit ownership that does not overlap other active work. Be specific: the child normally has the main agent's built-in tools, including lsp when enabled, but a selected profile can restrict its tools; it starts with NO context from this conversation. Shared isolation uses this working directory; worktree isolation captures a patch without merging it."
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
      "description": "Optional reasoning level for the child. Overrides the selected profile's thinking level when provided."
    },
    "fast_mode": {
      "type": "boolean",
      "description": "Optional fast-mode override for the child. Omit to inherit the selected profile or host setting."
    },
    "required": {
      "type": "boolean",
      "description": "Set true when the parent must receive this delegated result before it can finish. The worker remains asynchronous and reports through a host completion update. A bounded wait expiring does not terminate the accepted child and must not be retried while it remains active. Terminal failure or cancellation remains unmet until a successful follow-up. An outcome unobserved across host restart requires explicit user reconciliation."
    },
    "wait": {
      "type": "integer",
      "minimum": 1,
      "maximum": 300,
      "description": "Optional explicit number of seconds to wait for this sub-agent's initial task to finish. Omit to return immediately. The sub-agent continues in the background if this wait expires."
    },
    "isolation": {
      "type": "string",
      "enum": ["shared", "worktree"],
      "description": "Workspace mode. Shared preserves existing behavior; worktree captures a patch without merging it."
    }
  },
  "required": ["task"]
}`

func (t *SubagentSpawnTool) Name() string { return SubagentSpawnToolName }
func (t *SubagentSpawnTool) Description() string {
	return "Delegate a concrete, bounded scope to a resident sub-agent. For proactive delegation, use an independent sidecar only when the parent has useful non-overlapping work; keep immediate blockers local. A worker owns its scope until completion, so never duplicate it in the parent. If delegation owns the blocking task, end or yield the parent turn. Omit wait to return immediately and receive completion through [auto-subagents update]; set wait to an explicit 1–300 second value only when this turn should wait for the initial task. Set required=true when the outcome is mandatory before the parent's terminal response; failures remain recoverable through subagent_resume. Never use bash sleep, watch, tail -f, polling loops, repeated subagent_status, dashboard, metadata, or file checks solely to wait."
}
func (t *SubagentSpawnTool) Schema() json.RawMessage {
	return json.RawMessage(subagentSpawnSchemaTemplate)
}

func (t *SubagentSpawnTool) Execute(ctx context.Context, raw json.RawMessage, _ func(string)) (core.ToolResult, error) {
	prefix := t.Name()
	if t.ResidentManager == nil {
		return protocolToolError(prefix + ": subagent runtime not available in this mode")
	}
	if t.Enabled == nil || !t.Enabled() {
		return protocolToolError(prefix + ": subagent delegation is unavailable in this mode")
	}
	var a subagentSpawnArgs
	if err := decodeSubagentArgs(raw, &a); err != nil {
		return core.ToolResult{}, err
	}
	task := strings.TrimSpace(a.Task)
	if task == "" {
		return protocolToolError(prefix + ": task is required")
	}
	if a.Wait != nil && (*a.Wait < 1 || *a.Wait > maxSubagentWaitSeconds) {
		return protocolToolError(fmt.Sprintf("%s: wait must be between 1 and %d seconds", prefix, maxSubagentWaitSeconds))
	}

	workspaceMode := subagents.WorkspaceShared
	if value := strings.TrimSpace(a.Isolation); value != "" {
		workspaceMode = subagents.WorkspaceMode(value)
		if workspaceMode != subagents.WorkspaceShared && workspaceMode != subagents.WorkspaceWorktree {
			return protocolToolError(prefix + ": isolation must be shared or worktree")
		}
	}
	agentName := strings.TrimSpace(a.Agent)
	var profile *subagents.Profile
	var fastModeOverride *bool
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
		fastModeOverride = profile.FastMode
	}
	if a.FastMode != nil {
		fastModeOverride = a.FastMode
	}
	model := strings.TrimSpace(a.Model)
	providerID := strings.TrimSpace(a.Provider)
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

	reasoning, err := normalizeReasoning(a.Reasoning)
	if err != nil {
		return protocolToolError(prefix + ": " + err.Error())
	}
	if reasoning == "" && profile != nil && strings.TrimSpace(profile.Thinking) != "" {
		reasoning, err = normalizeReasoning(profile.Thinking)
		if err != nil {
			return protocolToolError(prefix + ": profile " + profile.Name + ": " + err.Error())
		}
	}
	if reasoning == "" && t.DefaultReasoning != nil {
		reasoning, err = normalizeReasoning(t.DefaultReasoning())
		if err != nil {
			return protocolToolError(prefix + ": host " + err.Error())
		}
	}
	if t.BuildResidentSpec == nil {
		return protocolToolError(prefix + ": resident child factory is unavailable")
	}
	spec, err := t.BuildResidentSpec(ctx, ResidentSpawnRequest{
		Task: task, Profile: profile, Model: model, Provider: providerID,
		Reasoning: reasoning, FastMode: fastModeOverride, Required: a.Required,
		WorkspaceMode: workspaceMode,
	})
	if err != nil {
		return protocolToolError(prefix + ": " + err.Error())
	}
	var completion subagents.ResidentCompletion
	waitTimedOut := false
	if a.Wait != nil {
		spec.InitialTurnID = uuid.NewString()
		completionResult, cancelWait := t.ResidentManager.WatchCompletion(spec.ID, spec.InitialTurnID)
		defer cancelWait()
		if _, err := t.ResidentManager.Spawn(ctx, spec, task); err != nil {
			return core.ToolResult{}, fmt.Errorf("%s: %w", prefix, err)
		}
		waitCtx, cancel := context.WithTimeout(ctx, time.Duration(*a.Wait)*time.Second)
		defer cancel()
		select {
		case result, ok := <-completionResult:
			if !ok {
				return core.ToolResult{}, fmt.Errorf("%s: completion wait ended unexpectedly", prefix)
			}
			completion = result
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return core.ToolResult{}, ctx.Err()
			}
			select {
			case result, ok := <-completionResult:
				if !ok {
					return core.ToolResult{}, fmt.Errorf("%s: completion wait ended unexpectedly", prefix)
				}
				completion = result
			default:
				waitTimedOut = true
			}
		}
	} else if _, err := t.ResidentManager.Spawn(ctx, spec, task); err != nil {
		return core.ToolResult{}, fmt.Errorf("%s: %w", prefix, err)
	}
	state := string(subagents.ResidentQueued)
	if a.Wait != nil {
		if waitTimedOut {
			if current, ok := t.ResidentManager.State(spec.ID); ok {
				state = string(current)
			}
		} else {
			state = completion.Completion().Status
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "spawned sub-agent %s\nstate: %s\nworkspace: %s\ntask: %s\n", spec.ID, state, spec.WorkspaceMode, truncateTask(task, 200))
	if spec.Profile != "" {
		fmt.Fprintf(&sb, "agent: %s\n", spec.Profile)
	}
	fmt.Fprintf(&sb, "model: %s\nprovider: %s\n", spec.Model, spec.Provider)
	if spec.Reasoning != "" {
		fmt.Fprintf(&sb, "reasoning: %s\n", spec.Reasoning)
	}
	if spec.BudgetLimit > 0 {
		fmt.Fprintf(&sb, "budget: %d weighted tokens (%s)\n", spec.BudgetLimit, spec.BudgetSource)
	}
	if spec.Required {
		fmt.Fprintf(&sb, "required: %s\n", state)
	}
	if a.Wait != nil {
		if waitTimedOut {
			fmt.Fprintf(&sb, "wait: timed out after %d seconds\n", *a.Wait)
			sb.WriteString("\nThe accepted sub-agent remains active in the background. It owns the delegated scope: do not repeat that work in the parent. Continue only with a previously selected non-overlapping task, or end/yield for the host-event-driven [auto-subagents update].")
		} else if completion.Err != nil {
			fmt.Fprintf(&sb, "error: %s\n", completion.Err)
			if completion.Summary != "" {
				fmt.Fprintf(&sb, "partial: %s\n", completion.Summary)
			}
		} else {
			if completion.Summary != "" {
				fmt.Fprintf(&sb, "final: %s\n", completion.Summary)
			}
		}
	} else {
		sb.WriteString("\nThe sub-agent is running in the background and owns the delegated scope. Do not repeat that work in the parent. Continue only with a previously selected non-overlapping task, or end/yield for the host-event-driven [auto-subagents update].")
	}
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: sb.String()}}}, nil
}

func decodeSubagentArgs(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid args: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("invalid args: trailing JSON value")
	}
	return nil
}

func normalizeReasoning(value string) (string, error) {
	value = strings.TrimSpace(value)
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
