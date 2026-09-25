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
	// SubagentToolName is the single model-facing subagent tool. One call
	// carries one action; see the SubagentAction* constants.
	SubagentToolName = "subagent"

	SubagentActionSpawn  = "spawn"
	SubagentActionStatus = "status"
	SubagentActionStop   = "stop"
	SubagentActionResume = "resume"
	// SubagentActionInterrupt cancels only the running turn and keeps the child.
	SubagentActionInterrupt = "interrupt"

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

// Name returns the shared facade name: the four subagent tool types are
// internal implementations and are never registered on their own.
func (t *SubagentSpawnTool) Name() string { return SubagentToolName }

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
		completion, waitTimedOut, err = waitForResidentCompletion(ctx, completionResult, *a.Wait, prefix)
		if err != nil {
			return core.ToolResult{}, err
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

// waitForResidentCompletion blocks until the watched resident turn reports its
// completion or the explicit bounded wait expires. A cancelled parent context
// wins over an expiring wait, and a completion that arrives in the same instant
// as the deadline is reported instead of a timeout.
func waitForResidentCompletion(ctx context.Context, completionResult <-chan subagents.ResidentCompletion, seconds int, prefix string) (completion subagents.ResidentCompletion, timedOut bool, err error) {
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	select {
	case result, ok := <-completionResult:
		if !ok {
			return subagents.ResidentCompletion{}, false, fmt.Errorf("%s: completion wait ended unexpectedly", prefix)
		}
		return result, false, nil
	case <-waitCtx.Done():
		if ctx.Err() != nil {
			return subagents.ResidentCompletion{}, false, ctx.Err()
		}
		select {
		case result, ok := <-completionResult:
			if !ok {
				return subagents.ResidentCompletion{}, false, fmt.Errorf("%s: completion wait ended unexpectedly", prefix)
			}
			return result, false, nil
		default:
			return subagents.ResidentCompletion{}, true, nil
		}
	}
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
