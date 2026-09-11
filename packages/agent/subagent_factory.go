package agent

import (
	"context"
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
	agent := core.NewAgent(resolved.NewClient(), resolved.Model, system, registry)
	// A resident child inherits the parent's resolved step limit, including the
	// unlimited default. Its ceiling is the provider context window, with one
	// compaction recovery per accepted turn, plus any explicit parent
	// --max-steps. There is no child-only step budget.
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
	// The nudge reads the agent's own per-turn usage gauge. It stays advisory:
	// the provider context window is the hard ceiling on a child turn.
	configureResidentContextNudge(agent, resolved.ContextWindow)
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
	limit := agent.MaxSteps
	return func(ctx context.Context, prompt string) error {
		if ctx == nil {
			ctx = context.Background()
		}
		turnCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		var journalErr error
		var journalMu sync.Mutex
		checkJournal := func() error {
			journalMu.Lock()
			defer journalMu.Unlock()
			if journalErr == nil {
				return nil
			}
			return fmt.Errorf("persist resident child transcript: %w", journalErr)
		}
		// usedSteps is the step high-water mark of this accepted turn. The sink
		// runs on the runner goroutine, so it needs no lock; the mark bounds the
		// continuation after a compaction recovery so an explicit parent limit
		// is never replenished.
		usedSteps := 0
		sink := func(event core.AgentEvent) {
			if turnStart, ok := event.(core.EvTurnStart); ok && turnStart.Step > usedSteps {
				usedSteps = turnStart.Step
			}
			if err := journal.RecordAgentEvent(event); err != nil {
				journalMu.Lock()
				if journalErr == nil {
					journalErr = err
					cancel()
				}
				journalMu.Unlock()
			}
		}
		err := agent.Prompt(turnCtx, prompt, nil, sink)
		if journalFailure := checkJournal(); journalFailure != nil {
			return journalFailure
		}
		if err != nil && turnCtx.Err() == nil && provider.IsContextOverflowError(err) {
			err = recoverResidentContextOverflow(turnCtx, agent, journal, sink, limit, usedSteps, err)
			if journalFailure := checkJournal(); journalFailure != nil {
				return journalFailure
			}
		}
		return err
	}, nil
}

// A compaction recovery never rewrites a transcript with nothing worth
// summarizing: the child task and the current exchange must survive verbatim.
const (
	residentContextRecoveryMinimumMessages = 4
	residentContextRecoveryKeepTail        = 2
)

// recoverResidentContextOverflow gives one accepted resident turn a second
// chance after the provider rejected it for exceeding the model context
// window. It compacts the transcript, journals the replacement transcript, and
// continues the same turn. Recovery happens at most once per call, and any
// compaction or persistence failure restores the exact pre-compaction
// transcript before returning the original error.
func recoverResidentContextOverflow(ctx context.Context, agent *core.Agent, journal *subagents.ResidentJournal, sink func(core.AgentEvent), limit, usedSteps int, overflowErr error) error {
	if agent == nil || journal == nil {
		return overflowErr
	}
	before := agent.Messages()
	if len(before) < residentContextRecoveryMinimumMessages {
		// Summarizing a transcript this short would replace the instruction
		// rather than shrink prior work.
		return overflowErr
	}
	if _, err := agent.CompactWithEvents(ctx, residentContextRecoveryKeepTail, sink); err != nil {
		agent.SetMessages(before)
		return errors.Join(overflowErr, fmt.Errorf("compact resident child transcript: %w", err))
	}
	if err := journal.RecordCompacted(agent.Messages()); err != nil {
		// Memory must not run ahead of durable history: a later resume would
		// otherwise replay the pre-compaction transcript.
		agent.SetMessages(before)
		return errors.Join(overflowErr, fmt.Errorf("persist resident child compaction: %w", err))
	}
	if limit > 0 {
		remaining := limit - usedSteps
		if remaining <= 0 {
			// An explicit parent limit is already spent. The checkpoint stays
			// durable so an explicit resume starts from the compacted
			// transcript, but this turn is not continued.
			return overflowErr
		}
		agent.MaxSteps = remaining
		defer func() { agent.MaxSteps = limit }()
	}
	return agent.Continue(ctx, sink)
}

// residentContextReminderBands are the context-usage percentages that arm the
// resident reminder. Each band fires at most once per child runner, so a child
// whose prompt keeps growing toward the window is told to wrap up again
// without turning the reminder into a budget.
var residentContextReminderBands = []int{85, 90, 95}

func configureResidentContextNudge(agent *core.Agent, contextMax int) {
	if agent == nil {
		return
	}
	// deliveredBand is owned by the runner closure and never reset from
	// Resume or journal paths. The injected developer-context message persists
	// via appendDynamicContext and is not replayed verbatim from the child
	// journal after restart; restarting a child rests the ladder.
	deliveredBand := -1
	agent.BeforeTurnContext = func(_ context.Context, _ int) (bool, string, string) {
		if contextMax <= 0 {
			return true, "", ""
		}
		usage := agent.LastTurnUsage()
		used := usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens
		band := residentContextReminderBand(used, contextMax)
		if band <= deliveredBand {
			return true, "", ""
		}
		deliveredBand = band
		return true, "", fmt.Sprintf("Context usage is at %d%% of the model window. Finish the current task, report results, limits, and remaining verifications, and do not start broad new work.", used*100/contextMax)
	}
}

// residentContextReminderBand returns the highest armed band crossed by the
// current prompt size, or -1 while usage stays below the first band. A direct
// jump to a higher band delivers one reminder for that band.
func residentContextReminderBand(used, contextMax int) int {
	if contextMax <= 0 {
		return -1
	}
	band := -1
	for _, candidate := range residentContextReminderBands {
		if used*100 >= contextMax*candidate {
			band = candidate
		}
	}
	return band
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
