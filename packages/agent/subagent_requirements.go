package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

const requiredWorkerContextHeader = "[required-subagents update]"

// WireRequiredWorkerGate composes host-enforced requirement handling with any
// extension hooks already installed on the parent. Parent turns and tools stay
// available while required work runs; terminal outcomes are injected on the
// next turn, and only a terminal assistant response is held while an obligation
// remains unmet.
func (rt *subagentRuntime) WireRequiredWorkerGate(parent *core.Agent) {
	if rt == nil || rt.supervisor == nil || parent == nil {
		return
	}
	previousBeforeTurn := parent.BeforeTurnContext
	parent.BeforeTurnContext = func(ctx context.Context, step int) (bool, string, string) {
		var inherited string
		if previousBeforeTurn != nil {
			allowed, reason, contextText := previousBeforeTurn(ctx, step)
			if !allowed {
				return false, reason, contextText
			}
			inherited = contextText
		}
		if err := rt.waitRequiredWorkerReady(ctx); err != nil {
			return false, fmt.Sprintf("loading persisted subagents failed: %v; resolve the supervisor reload error before continuing", err), inherited
		}
		required := rt.supervisor.RequiredSnapshots()
		update := rt.formatRequiredWorkerContext(required)
		if update == "" {
			return true, "", inherited
		}
		return true, "", joinRequiredContext(inherited, update)
	}

	previousBeforeTool := parent.BeforeToolExecute
	parent.BeforeToolExecute = func(call provider.ToolCallBlock) (bool, string, json.RawMessage) {
		if previousBeforeTool != nil {
			return previousBeforeTool(call)
		}
		return true, "", nil
	}

	previousBeforeAssistant := parent.BeforeAssistantMessage
	parent.BeforeAssistantMessage = func(text string) (bool, string, string) {
		unmet := rt.supervisor.UnmetRequirements()
		if len(unmet) != 0 {
			// Worker completion is delivered through the coordinator-owned tracker.
			// Do not queue a synthetic reminder here: it races that terminal wave
			// and can wake the parent before every required result is available.
			return false, formatRequiredWorkerBlock(unmet), ""
		}
		if previousBeforeAssistant != nil {
			allowed, reason, replacement := previousBeforeAssistant(text)
			if !allowed {
				return false, reason, replacement
			}
			rt.acknowledgeSatisfiedRequirements()
			return true, reason, replacement
		}
		rt.acknowledgeSatisfiedRequirements()
		return true, "", ""
	}
}

// SetRequiredWorkerReady makes the gate wait for an asynchronous supervisor
// reload before it checks persisted obligations. A nil channel means no reload
// is pending. The error callback is read only after ready closes.
func (rt *subagentRuntime) SetRequiredWorkerReady(ready <-chan struct{}, result func() error) {
	if rt == nil {
		return
	}
	rt.requiredReadyMu.Lock()
	rt.requiredReady = ready
	rt.requiredReadyErr = result
	rt.requiredReadyMu.Unlock()
}

func (rt *subagentRuntime) waitRequiredWorkerReady(ctx context.Context) error {
	rt.requiredReadyMu.RLock()
	ready := rt.requiredReady
	result := rt.requiredReadyErr
	rt.requiredReadyMu.RUnlock()
	if ready == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ready:
		if result != nil {
			return result()
		}
		return nil
	}
}

func (rt *subagentRuntime) acknowledgeSatisfiedRequirements() {
	for _, snapshot := range rt.supervisor.RequiredSnapshots() {
		if snapshot.Requirement.State == subagents.RequirementSatisfied && !snapshot.Requirement.Notified {
			_ = rt.supervisor.MarkRequirementNotified(snapshot.ID)
		}
	}
}

const requiredWorkerContextRuneBudget = 6000

func (rt *subagentRuntime) formatRequiredWorkerContext(required []subagents.AgentSnapshot) string {
	rt.requiredContextMu.Lock()
	defer rt.requiredContextMu.Unlock()
	contextText := formatRequiredWorkerContextWithReported(required, rt.reportedRequiredUnmet)
	if rt.reportedRequiredUnmet == nil {
		rt.reportedRequiredUnmet = make(map[string]struct{})
	}
	for _, snapshot := range required {
		if snapshot.Requirement.Unmet() {
			rt.reportedRequiredUnmet[snapshot.ID] = struct{}{}
		}
	}
	return contextText
}

func formatRequiredWorkerContextWithReported(required []subagents.AgentSnapshot, previouslyReported map[string]struct{}) string {
	var items []subagents.AgentSnapshot
	for _, snapshot := range required {
		requirement := snapshot.Requirement
		if !requirement.Required || requirement.State == subagents.RequirementPending {
			continue
		}
		if requirement.Notified && !requirement.Unmet() {
			continue
		}
		items = append(items, snapshot)
	}
	if len(items) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(requiredWorkerContextHeader)
	sb.WriteString("\nThe host waited for required delegated work before this model turn. These outcomes are durable and authoritative:\n")
	for _, snapshot := range items {
		requirement := snapshot.Requirement
		if requirement.Unmet() {
			if _, reported := previouslyReported[snapshot.ID]; reported {
				fmt.Fprintf(&sb, "\n- %s: %s (turn %d; previously reported; diagnostic: %s)\n", snapshot.ID, requirement.State, requirement.TargetTurn, subagents.ResultRef(snapshot.ID))
				continue
			}
		}
		fmt.Fprintf(&sb, "\n- %s: %s (turn %d)\n  task: %s\n", snapshot.ID, requirement.State, requirement.TargetTurn, strings.TrimSpace(snapshot.Task))
		if requirement.ErrorCode != "" {
			fmt.Fprintf(&sb, "  error_code: %s\n  diagnostic: %s\n", requirement.ErrorCode, subagents.ResultRef(snapshot.ID))
		}
		output := strings.TrimSpace(snapshot.LastAssistant)
		if snapshot.Result != nil && strings.TrimSpace(snapshot.Result.Output) != "" {
			output = strings.TrimSpace(snapshot.Result.Output)
		}
		if output != "" {
			fmt.Fprintf(&sb, "  result:\n%s\n", indentRequiredOutput(output, "    ", 6000))
		}
	}
	if hasIndeterminateRequirement(items) {
		sb.WriteString("\nA required outcome is indeterminate after host restart. Do not retry it automatically: the user must inspect the durable result or external side effect, then explicitly resume, restart, or remove the terminal worker.")
	} else if hasUnmetRequirement(items) {
		sb.WriteString("\nAt least one required outcome is unresolved. Retry that worker with subagent_resume; do not emit a terminal answer. Only the user can waive it by removing the terminal worker with /subagents rm.")
	} else {
		sb.WriteString("\nAll listed requirements are satisfied. Use their results before answering.")
	}
	return truncateRequiredContext(sb.String(), requiredWorkerContextRuneBudget)
}

func truncateRequiredContext(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

func formatRequiredWorkerBlock(unmet []subagents.AgentSnapshot) string {
	var sb strings.Builder
	sb.WriteString(requiredWorkerContextHeader)
	sb.WriteString("\nTerminal completion is blocked by required delegated work:")
	for _, snapshot := range unmet {
		fmt.Fprintf(&sb, "\n- %s: %s — %s", snapshot.ID, snapshot.Requirement.State, strings.TrimSpace(snapshot.Task))
		if snapshot.Requirement.ErrorCode != "" {
			fmt.Fprintf(&sb, " (%s; diagnostic: %s)", snapshot.Requirement.ErrorCode, subagents.ResultRef(snapshot.ID))
		}
	}
	if hasIndeterminateRequirement(unmet) {
		sb.WriteString("\nDo not retry an indeterminate outcome automatically. The user must inspect its durable result or external side effect, then explicitly resume, restart, or remove the worker.")
	} else {
		sb.WriteString("\nRetry unresolved work with subagent_resume. Do not claim completion. The user may explicitly waive a terminal worker with /subagents rm.")
	}
	return sb.String()
}

func hasIndeterminateRequirement(required []subagents.AgentSnapshot) bool {
	for _, snapshot := range required {
		if snapshot.Requirement.State == subagents.RequirementIndeterminate {
			return true
		}
	}
	return false
}

func hasUnmetRequirement(required []subagents.AgentSnapshot) bool {
	for _, snapshot := range required {
		if snapshot.Requirement.Unmet() {
			return true
		}
	}
	return false
}

func joinRequiredContext(first, second string) string {
	first = strings.TrimSpace(first)
	second = strings.TrimSpace(second)
	switch {
	case first == "":
		return second
	case second == "":
		return first
	default:
		return first + "\n\n" + second
	}
}

func indentRequiredOutput(text, prefix string, maxRunes int) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if maxRunes > 0 && len(runes) > maxRunes {
		text = string(runes[:maxRunes]) + "\n…(truncated)"
	}
	return prefix + strings.ReplaceAll(text, "\n", "\n"+prefix)
}
