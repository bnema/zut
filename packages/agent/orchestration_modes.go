package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/bnema/zut/packages/agent/modes"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

type orchestratedModeHooks struct {
	promptLabel string

	// startupSink is used for entry.pre agent turns. The completion updates
	// themselves go through runPrimary and therefore use the same renderer as
	// the initial parent turn.
	startupSink func() (func(core.AgentEvent), func())
	runPrimary  func(context.Context, *core.Agent, string, func([]provider.Message) error) (string, modes.ContextRecoveryResult, error)
	emitError   func(error)
	finish      func(string) error
}

// runOrchestratedMode owns the mode-neutral headless lifecycle. Modes provide
// only their existing renderer and wire format; the tracker, worker runtime,
// startup hooks, transcript persistence, and completion-wave loop are shared.
func runOrchestratedMode(parentCtx context.Context, args Args, version string, hooks orchestratedModeHooks) (runErr error) {
	ctx, stopSignal := signal.NotifyContext(parentCtx, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stopSignal()

	if args.NoYolo {
		fmt.Fprintf(os.Stderr, "warning: --no-yolo has no effect in %s mode (no interactive prompt available); tools will run without confirmation\n", hooks.promptLabel)
	}
	r, err := Resolve(args, true)
	if err != nil {
		return err
	}
	initialCfg, err := LoadConfig()
	if err != nil {
		return fmt.Errorf("load config for orchestrated %s: %w", hooks.promptLabel, err)
	}
	prompt := args.Prompt
	if prompt == "" {
		piped, readErr := readAllStdin()
		if readErr != nil {
			return fmt.Errorf("read %s prompt from stdin: %w", hooks.promptLabel, readErr)
		}
		prompt = strings.TrimSpace(piped)
	}
	if prompt == "" {
		return fmt.Errorf("%s mode requires a prompt (arg or stdin)", hooks.promptLabel)
	}
	extMgr, stopExt := setupNonInteractiveExtensions(ctx, args, &r, version)
	defer stopExt()

	tracker := subagents.NewCompletionTracker()
	runtime := newOrchestratedRuntime(ctx, args, r, initialCfg, tracker)
	defer func() {
		if closeErr := closeSubagentRuntimeFresh(runtime); closeErr != nil {
			runErr = errors.Join(runErr, closeErr)
		}
	}()

	prepareRegistry := func(reg core.Registry) core.Registry {
		return runtime.PrepareRegistry(reg)
	}
	runtime.SetProvider(r.Provider)
	runtime.SetProviderSettings(r.BaseURL, r.InsecureTLS)
	runtime.SetFastMode(r.FastMode)
	runtime.PrepareResolvedRegistry(r.ToolRegistry, r.WebSearchPolicy)
	ag := r.NewAgent()
	initialAg := ag
	defer func() {
		closeAgentLSP(ag)
		if ag != initialAg {
			closeAgentLSP(initialAg)
		}
	}()
	wireNonInteractiveAgentExtHooks(ctx, ag, extMgr)

	sess, err := openOrCreateSession(ctx, args, r, ag, version)
	if err != nil {
		return err
	}
	if sess != nil {
		defer func() { runErr = joinSessionCloseError(runErr, sess) }()
		var providerName, model string
		sess, ag, providerName, model, err = applyInitialSessionResumeWithRuntime(ctx, args, r, extMgr, sess, ag, runtime)
		if err != nil {
			return err
		}
		r.Provider, r.Model = providerName, model
		ag.OnToolResult = func(_ string, result core.ToolResult) { persistExtensionToolResult(extMgr, sess, result) }
		runtime.SetActiveSession(sess.ID)
	}
	announceSession(extMgr, sess)

	start := len(ag.Messages())
	preSink, finishPre := hooks.startupSink()
	preErr := runZutfileStartupPre(ctx, args.StartupPre, r.CWD, r.Sandbox, ag, preSink, os.Stderr)
	finishPre()
	if preErr != nil {
		if hooks.emitError != nil {
			hooks.emitError(preErr)
		}
		return preErr
	}
	if strings.TrimSpace(args.StartupPre) != "" {
		refreshedPolicy, refreshErr := reloadResourcesAfterStartupPreWithRegistry(ctx, args, extMgr, r.Sandbox, ag, prepareRegistry)
		if refreshErr != nil {
			if hooks.emitError != nil {
				hooks.emitError(refreshErr)
			}
			return refreshErr
		}
		// entry.pre may have changed the effective permission set. Keep the
		// worker supervisor in step with the refreshed parent registry.
		runtime.SetWebSearchPolicy(refreshedPolicy)
	}

	var usagePersistErr error
	if sess != nil {
		ag.OnUsage = func(cumulative provider.Usage) {
			if usagePersistErr == nil {
				usagePersistErr = sess.AppendUsage(ag.LastTurnUsage(), cumulative)
			}
		}
	}
	var persistCompaction func([]provider.Message) error
	if sess != nil {
		persistCompaction = func(messages []provider.Message) error {
			return sess.AppendCompactionWithUsage(messages, ag.Cost())
		}
	}

	transcriptStart := start
	runParent := func(turnCtx context.Context, text string) (string, error) {
		output, recovery, turnErr := hooks.runPrimary(turnCtx, ag, text, persistCompaction)
		if recovery.Compacted {
			transcriptStart = recovery.OutputStart
		}
		if usagePersistErr != nil {
			// Usage persistence happens after the mode renderer has completed.
			// JSON therefore needs an explicit terminal error for this failure;
			// unlike provider errors, no renderer event has reported it yet.
			if turnErr == nil && hooks.emitError != nil {
				hooks.emitError(usagePersistErr)
			}
			return output, errors.Join(turnErr, usagePersistErr)
		}
		return output, turnErr
	}

	finalText, err := runParent(ctx, prompt)
	if err == nil {
		finalText, err = runHeadlessContinuation(ctx, tracker, finalText, runParent)
		if err != nil && hooks.emitError != nil {
			// A completion wait has no renderer call in which to report its
			// failure, so JSON must add its single terminal error object here.
			// Errors from the follow-up parent turn were already rendered by
			// that mode and must not be emitted a second time.
			var waitErr *headlessCompletionWaitError
			if errors.As(err, &waitErr) {
				hooks.emitError(waitErr)
			}
		}
	}
	if persistErr := WriteNewTranscript(ag, sess, transcriptStart); persistErr != nil {
		if err == nil && hooks.emitError != nil {
			hooks.emitError(persistErr)
		}
		err = errors.Join(err, persistErr)
	}
	if err != nil {
		return err
	}
	return hooks.finish(finalText)
}

func newOrchestratedRuntime(ctx context.Context, args Args, r Resolved, cfg Config, tracker *subagents.CompletionTracker) *subagentRuntime {
	onSpawned := func(a *subagents.Agent, task string) {
		tracker.TrackTurn(a, task, false)
	}
	onResumed := func(a *subagents.Agent, prompt string) {
		// BeforeResumed normally owns future-turn registration. Keep this
		// fallback for direct runtime callers that do not install the pre-hook;
		// SubagentResumeTool suppresses it when BeforeResumed accepted delivery.
		tracker.TrackTurn(a, prompt, true)
	}
	onBeforeResumed := func(a *subagents.Agent, prompt string) func() {
		return tracker.TrackFutureTurn(a, prompt, true)
	}
	onStopRequested := func(a *subagents.Agent) {
		tracker.TrackExit(a, "stopped")
	}
	return newSubagentRuntime(subagentRuntimeConfig{
		Context:         ctx,
		Args:            args,
		Root:            filepath.Join(ZutHome(), "subagents"),
		RepoRoot:        r.CWD,
		Provider:        r.Provider,
		Model:           r.Model,
		Reasoning:       r.Reasoning,
		BaseURL:         r.BaseURL,
		InsecureTLS:     r.InsecureTLS,
		FastMode:        r.FastMode,
		APIKey:          args.APIKey,
		Policy:          subagentPolicyFromConfig(cfg.Subagents),
		WebSearchPolicy: webSearchPolicyForRegistry(r.WebSearchPolicy, r.ToolRegistry),
		OnSpawned:       onSpawned,
		OnResumed:       onResumed,
		BeforeResumed:   onBeforeResumed,
		OnStopRequested: onStopRequested,
	})
}

func runOrchestratedStreamMode(ctx context.Context, args Args, version string) error {
	return runOrchestratedMode(ctx, args, version, orchestratedModeHooks{
		promptLabel: "stream",
		startupSink: func() (func(core.AgentEvent), func()) {
			return newStreamTextSink(os.Stdout)
		},
		runPrimary: func(turnCtx context.Context, ag *core.Agent, prompt string, persist func([]provider.Message) error) (string, modes.ContextRecoveryResult, error) {
			recovery, err := modes.RunStreamWithContextRecovery(turnCtx, ag, prompt, nil, os.Stdout, os.Stderr, persist)
			return "", recovery, err
		},
		finish: func(string) error { return nil },
	})
}

func runOrchestratedJSONMode(ctx context.Context, args Args, version string) error {
	enc := json.NewEncoder(os.Stdout)
	var outputErr error
	encode := func(value any) {
		if outputErr == nil {
			outputErr = enc.Encode(value)
		}
	}
	runErr := runOrchestratedMode(ctx, args, version, orchestratedModeHooks{
		promptLabel: "json",
		startupSink: func() (func(core.AgentEvent), func()) {
			return func(ev core.AgentEvent) {
				encode(modes.EventToJSON(ev))
			}, func() {}
		},
		runPrimary: func(turnCtx context.Context, ag *core.Agent, prompt string, persist func([]provider.Message) error) (string, modes.ContextRecoveryResult, error) {
			recovery, err := modes.RunJSONWithContextRecovery(turnCtx, ag, prompt, nil, os.Stdout, persist)
			return "", recovery, err
		},
		emitError: func(err error) {
			if err != nil {
				encode(map[string]any{"type": "error", "message": err.Error()})
			}
		},
		finish: func(string) error { return nil },
	})
	return errors.Join(runErr, outputErr)
}
