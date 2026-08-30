package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// newResidentChildRunner is the host-owned construction boundary for one
// resident child. It resolves a fresh provider client and a fresh core.Agent
// exactly once for the child's durable session; no subprocess configuration or
// credential transfer is involved.
func newResidentChildRunner(args Args, spec subagents.ResidentChildSpec, journal *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
	if strings.TrimSpace(spec.SessionID) == "" {
		return nil, fmt.Errorf("resident child %q has no session identity", spec.ID)
	}
	resolved, err := Resolve(args, true)
	if err != nil {
		return nil, fmt.Errorf("resolve resident child %q: %w", spec.ID, err)
	}

	registry, err := residentChildRegistry(resolved.ToolRegistry, spec.Tools)
	if err != nil {
		return nil, err
	}
	system := resolved.SystemPrompt
	if extra := strings.TrimSpace(spec.SystemPrompt); extra != "" {
		if spec.SystemPromptMode == "replace" {
			system = extra
		} else {
			system = strings.TrimSpace(system) + "\n\n" + extra
		}
	}
	budgetLimit := subagents.EffectiveBudgetLimit(spec.BudgetLimit, resolved.ContextWindow)
	if guidance := subagents.BudgetSystemPrompt(budgetLimit); guidance != "" {
		system = strings.TrimSpace(system) + "\n\n" + guidance
	}
	agent := core.NewAgent(resolved.NewClient(), resolved.Model, system, registry)
	agent.MaxSteps = resolved.MaxSteps
	agent.ContextWindow = resolved.ContextWindow
	agent.MaxTokens = resolved.MaxOutput
	agent.Reasoning = resolved.Reasoning
	agent.Temperature = resolved.Temperature
	agent.FastMode = resolved.FastMode
	var baseline subagents.ResidentUsageSnapshot
	if journal != nil {
		baseline = journal.ConfigureUsage(resolved.ContextWindow, resolved.AuthMethod == "oauth")
		agent.SeedCost(baseline.Usage)
	}
	budget := subagents.NewRolloutBudget(budgetLimit, baseline.Usage)
	configureResidentBudget(agent, budget)
	rootCacheID := strings.TrimSpace(spec.RootCacheID)
	if rootCacheID == "" {
		// Journals written before root cache identity was persisted can still
		// resume safely: their immediate parent was the only available root.
		rootCacheID = strings.TrimSpace(spec.ParentSessionID)
		if rootCacheID == "" {
			rootCacheID = spec.SessionID
		}
	}
	if err := agent.BindRequestIdentity(rootCacheID, spec.SessionID); err != nil {
		return nil, fmt.Errorf("bind resident child %q request identity: %w", spec.ID, err)
	}
	if journal != nil {
		messages, err := subagents.ReadResidentTranscriptMessages(journal.Dir())
		if err != nil {
			return nil, fmt.Errorf("restore resident child %q transcript: %w", spec.ID, err)
		}
		if len(messages) > 0 {
			agent.SetMessages(messages)
		}
	}
	return func(ctx context.Context, prompt string) error {
		if ctx == nil {
			ctx = context.Background()
		}
		turnCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		startedExhausted := budget.Snapshot().State == subagents.BudgetExceeded
		var journalErr error
		var journalMu sync.Mutex
		err := agent.Prompt(turnCtx, prompt, nil, func(event core.AgentEvent) {
			if usage, ok := event.(core.EvUsage); ok {
				budget.Observe(usage.Cumulative)
			}
			if err := journal.RecordAgentEvent(event); err != nil {
				journalMu.Lock()
				if journalErr == nil {
					journalErr = err
					cancel()
				}
				journalMu.Unlock()
			}
		})
		journalMu.Lock()
		defer journalMu.Unlock()
		if journalErr != nil {
			return fmt.Errorf("persist resident child transcript: %w", journalErr)
		}
		if budget.Snapshot().State == subagents.BudgetExceeded {
			if !startedExhausted && err == nil {
				return nil
			}
			return errors.Join(err, subagents.ErrBudgetExceeded)
		}
		return err
	}, nil
}

func configureResidentBudget(agent *core.Agent, budget *subagents.RolloutBudget) {
	if agent == nil || budget == nil {
		return
	}
	agent.BeforeTurnContext = func(_ context.Context, _ int) (bool, string, string) {
		instruction, exceeded := budget.TurnContext()
		if exceeded {
			return false, subagents.ErrBudgetExceeded.Error(), ""
		}
		snapshot := budget.Snapshot()
		if snapshot.Limit > 0 {
			remaining := residentBudgetOutputLimit(snapshot.Limit - snapshot.Used)
			if agent.MaxTokens <= 0 || agent.MaxTokens > remaining {
				agent.MaxTokens = remaining
			}
		}
		if snapshot.State == subagents.BudgetFinalizing {
			agent.SetTools(core.Registry{})
		}
		return true, "", instruction
	}
	agent.BeforeToolExecute = func(provider.ToolCallBlock) (bool, string, json.RawMessage) {
		state := budget.Snapshot().State
		if state == subagents.BudgetFinalizing || state == subagents.BudgetExceeded {
			return false, "subagent rollout budget is reserved for the final response; tools are disabled", nil
		}
		return true, "", nil
	}
}

func residentBudgetOutputLimit(remaining int64) int {
	if remaining <= 1 {
		return 1
	}
	maxInt := int64(^uint(0) >> 1)
	if remaining > maxInt {
		return int(maxInt)
	}
	return int(remaining)
}

// residentChildArgs applies the complete durable child specification without
// rediscovering a mutable profile file. That preserves the profile inheritance
// decision accepted at spawn time across explicit resume and restart.
func residentChildArgs(args Args, parentProvider string, spec subagents.ResidentChildSpec) Args {
	next := args
	// Primary delegation policy belongs to the parent, not its workers. A child
	// receives its accepted profile instructions and exact child tool list.
	next.Orchestrate = false
	next.ResidentChild = true
	// An explicit CLI key belongs to the provider that resolved the parent.
	// A child can select another provider, but it must resolve that provider's
	// own credential rather than forwarding or persisting the parent's key.
	if canonicalProvider(parentProvider) != canonicalProvider(spec.Provider) {
		next.APIKey = ""
	}
	next.Provider = spec.Provider
	next.BaseURL = spec.BaseURL
	next.InsecureTLS = spec.InsecureTLS
	next.Model = spec.Model
	next.Reasoning = spec.Reasoning
	next.FastMode = spec.FastMode
	next.FastModeSet = true
	if strings.TrimSpace(spec.Workspace) != "" {
		next.CWD = spec.Workspace
	}
	if spec.InheritSkills != nil && !*spec.InheritSkills {
		next.NoSkill = true
	}
	if spec.InheritProjectContext != nil && !*spec.InheritProjectContext {
		next.NoContextFiles = true
	}
	return next
}

// residentChildRegistry applies an already validated, exact child tool list.
// It always strips delegation and host-goal mutation tools: child agents are
// independent workers, not nested orchestrators or goal owners.
func residentChildRegistry(catalogue core.Registry, names []string) (core.Registry, error) {
	registry := make(core.Registry, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("resident child has an empty tool name")
		}
		if name == tools.SubagentSpawnToolName || name == tools.SubagentStatusToolName || name == tools.SubagentStopToolName || name == tools.SubagentResumeToolName || name == "update_goal" {
			return nil, fmt.Errorf("resident child tool %q is not allowed", name)
		}
		tool, ok := catalogue[name]
		if !ok {
			return nil, fmt.Errorf("resident child declares unavailable tool %q", name)
		}
		registry[name] = tool
	}
	return registry, nil
}
