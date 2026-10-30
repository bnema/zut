package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bnema/zut/packages/agent/extensions"
	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/bnema/zut/packages/agent/modes"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/skills"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

type extToolAdapter struct {
	mgr *extensions.Manager
}

var _ ExtensionToolSource = (*extToolAdapter)(nil)
var _ ExtensionSkillSource = (*extToolAdapter)(nil)

func (a *extToolAdapter) Skills() []*skills.Skill {
	if a == nil || a.mgr == nil {
		return nil
	}
	return a.mgr.Skills()
}

func (a *extToolAdapter) Tools() []ExtensionToolInfo {
	infos := a.mgr.Tools()
	out := make([]ExtensionToolInfo, len(infos))
	for i, t := range infos {
		out[i] = ExtensionToolInfo{
			Extension:   t.Extension,
			Name:        t.Name,
			Description: t.Description,
			Schema:      t.Schema,
			Deferred:    t.Deferred,
			Interactive: t.Interactive,
		}
	}
	return out
}

func (a *extToolAdapter) NewExtensionTool(info ExtensionToolInfo) core.Tool {
	return extensions.NewTool(a.mgr, extensions.ToolInfo{
		Extension:   info.Extension,
		Name:        info.Name,
		Description: info.Description,
		Schema:      info.Schema,
		Deferred:    info.Deferred,
		Interactive: info.Interactive,
	})
}

func newSessionTransition(mu *sync.RWMutex) func(func()) {
	return func(fn func()) {
		mu.Lock()
		defer mu.Unlock()
		fn()
	}
}

// newPersistModelCallback returns the callback used by the TUI while its
// session transition write lock is held. It must not acquire that lock again
// because sync.RWMutex is not reentrant.
func newPersistModelCallback(persistMu *sync.Mutex, sess **core.Session, activeProvider, activeModel *string, notify func(string)) func(string, string) {
	return func(providerName, model string) {
		providerName = strings.TrimSpace(providerName)
		model = strings.TrimSpace(model)

		persistMu.Lock()
		var sessionErr error
		if *sess != nil {
			sessionErr = (*sess).UpdateModel(providerName, model)
		}
		*activeProvider = providerName
		*activeModel = model
		persistMu.Unlock()
		if sessionErr != nil {
			if notify != nil {
				notify("model change was not persisted: " + sessionErr.Error())
			}
			return
		}

		if err := updateConfig(func(cfg *Config) error {
			cfg.Provider = providerName
			cfg.Model = model
			return nil
		}); err != nil && notify != nil {
			notify("model changed for this run, but config could not be saved: " + err.Error())
		}
	}
}

func closeAgentLSP(ag *core.Agent) {
	if ag != nil {
		_ = tools.CloseLSPManagers(ag.ToolsSnapshot())
	}
}

func joinSessionCloseError(err error, sess *core.Session) error {
	if sess == nil {
		return err
	}
	if closeErr := sess.Close(); closeErr != nil {
		return errors.Join(err, fmt.Errorf("close session: %w", closeErr))
	}
	return err
}

func trimMessagesForResume(msgs []provider.Message, keepTail int) []provider.Message {
	if keepTail <= 0 || len(msgs) <= keepTail {
		return provider.RepairOrphanedToolResults(msgs)
	}
	var out []provider.Message
	start := len(msgs) - keepTail
	var carriedToolNames []string
	addToolNames := func(names []string) {
		for _, name := range names {
			if name == "" || slices.Contains(carriedToolNames, name) {
				continue
			}
			carriedToolNames = append(carriedToolNames, name)
		}
	}
	// Activation markers in the discarded prefix still affect the retained
	// provider request. Carry them onto its first message rather than dropping
	// deferred-tool availability when the resume tail is shortened.
	for _, msg := range msgs[:start] {
		addToolNames(msg.AddedToolNames)
	}
	// Preserve the synthetic compaction summary when present so an
	// already-compacted session stays compacted after resume. Count it
	// within the tail budget instead of dropping it at the exact boundary.
	if len(msgs) > 0 && msgs[0].Meta["compaction"] == "true" && start > 0 {
		out = append(out, msgs[0])
		start++
	}
	// Avoid hydrating a tail that starts with orphan tool_result rows;
	// provider APIs require those to be paired with an earlier tool_use.
	for start < len(msgs) && msgs[start].Role == provider.RoleTool {
		start++
	}
	out = append(out, msgs[start:]...)
	if len(carriedToolNames) > 0 && len(out) > 0 {
		first := out[0]
		for _, name := range first.AddedToolNames {
			addToolNames([]string{name})
		}
		first.AddedToolNames = append([]string(nil), carriedToolNames...)
		out[0] = first
	}
	return provider.RepairOrphanedToolResults(out)
}

// sessionResumeCandidate contains all state needed to resume a session.
// It is deliberately assembled before the caller changes any live state:
// opening the transcript, reading usage, and rebuilding a provider/model
// must all succeed before the candidate can replace the current session.
type sessionResumeCandidate struct {
	session          *core.Session
	agent            *core.Agent
	messages         []provider.Message
	fullMessageCount int
	cumulative       provider.Usage
	lastTurn         provider.Usage
	provider         string
	model            string
	rebuilt          bool
	// planSnapshots is the per-call checklist reconstruction over the full
	// on-disk transcript. The resumed agent only keeps the trimmed tail, so
	// the interactive must seed from this rather than replay its messages.
	planSnapshots map[string]core.PlanUpdate
}

// prepareSessionResume opens and validates a selected session without
// touching the current agent. Missing provider/model metadata is treated as
// legacy metadata: the current agent is reused, preserving compatibility
// with sessions written before those fields were persisted.
//
// The returned candidate owns session until its caller commits it. On any
// error this function closes the candidate session and leaves the current
// agent untouched.
func prepareSessionResume(path string, current *core.Agent, currentProvider, currentModel string, buildAgentFor func(string, string) (*core.Agent, string, string, error)) (sessionResumeCandidate, error) {
	return prepareSessionResumeWithOptions(path, current, currentProvider, currentModel, false, false, false, buildAgentFor)
}

// prepareSessionResumeForWorkspace always rebuilds the agent. A selected
// transcript from another cwd must not retain tools, instructions, or sandbox
// state assembled for the previous workspace even when provider/model match.
func prepareSessionResumeForWorkspace(path string, current *core.Agent, currentProvider, currentModel string, buildAgentFor func(string, string) (*core.Agent, string, string, error)) (sessionResumeCandidate, error) {
	return prepareSessionResumeWithOptions(path, current, currentProvider, currentModel, false, false, true, buildAgentFor)
}

// prepareSessionResumeWithOptions is the provider/model-aware implementation.
// An explicit CLI provider or model keeps that field authoritative; an empty
// field falls back to the selected session's metadata when it is complete.
func prepareSessionResumeWithOptions(path string, current *core.Agent, currentProvider, currentModel string, explicitProvider, explicitModel, forceRebuild bool, buildAgentFor func(string, string) (*core.Agent, string, string, error)) (candidate sessionResumeCandidate, err error) {
	if current == nil {
		return candidate, fmt.Errorf("no agent running; log in first")
	}

	sess, msgs, err := core.OpenSession(path)
	if err != nil {
		return candidate, err
	}
	keepSession := false
	defer func() {
		if !keepSession {
			err = joinSessionCloseError(err, sess)
		}
	}()

	cumulative, lastTurn, err := core.SessionUsageDetail(path)
	if err != nil {
		return candidate, fmt.Errorf("read session usage: %w", err)
	}

	fullMessageCount := len(msgs)
	resumedMessages := trimMessagesForResume(msgs, 100)
	// Reconstruct the historical checklists from the untrimmed messages: a
	// plan delta inside the trim window needs the pre-window base to rebuild
	// the right list, and the view must keep using the trimmed transcript.
	planSnapshots := core.PlanUpdateSnapshots(msgs)
	storedProvider := strings.TrimSpace(sess.Meta.Provider)
	storedModel := strings.TrimSpace(sess.Meta.Model)
	currentProvider = strings.TrimSpace(currentProvider)
	currentModel = strings.TrimSpace(currentModel)

	candidate = sessionResumeCandidate{
		session:          sess,
		agent:            current,
		messages:         resumedMessages,
		fullMessageCount: fullMessageCount,
		cumulative:       cumulative,
		lastTurn:         lastTurn,
		provider:         currentProvider,
		model:            currentModel,
		planSnapshots:    planSnapshots,
	}

	// Old sessions may have either field absent. Only a complete stored
	// selection is actionable; otherwise resume exactly as older zut did.
	if storedProvider == "" || storedModel == "" {
		keepSession = true
		return candidate, nil
	}

	resumeProvider := storedProvider
	if explicitProvider {
		resumeProvider = currentProvider
	}
	resumeModel := storedModel
	if explicitModel {
		resumeModel = currentModel
	}
	if !forceRebuild && resumeProvider == currentProvider && resumeModel == currentModel {
		keepSession = true
		return candidate, nil
	}
	if buildAgentFor == nil {
		return sessionResumeCandidate{}, fmt.Errorf("cannot resume session with provider %q and model %q: no agent builder configured", resumeProvider, resumeModel)
	}

	rebuilt, resolvedProvider, resolvedModel, err := buildAgentFor(resumeProvider, resumeModel)
	if err != nil {
		if rebuilt != current {
			closeAgentLSP(rebuilt)
		}
		return sessionResumeCandidate{}, fmt.Errorf("rebuild agent for session provider %q/model %q: %w", resumeProvider, resumeModel, err)
	}
	if rebuilt == nil {
		return sessionResumeCandidate{}, fmt.Errorf("rebuild agent for session provider %q/model %q returned no agent", resumeProvider, resumeModel)
	}
	resolvedProvider = strings.TrimSpace(resolvedProvider)
	resolvedModel = strings.TrimSpace(resolvedModel)
	if resolvedProvider == "" {
		resolvedProvider = storedProvider
	}
	if resolvedModel == "" {
		resolvedModel = storedModel
	}

	// Hydrate and seed the replacement while it is still private. The live
	// agent is not modified until the caller commits the complete candidate.
	rebuilt.SetMessages(resumedMessages)
	rebuilt.SeedCost(cumulative)
	rebuilt.SeedLastTurnUsage(lastTurn)
	candidate.agent = rebuilt
	candidate.provider = resolvedProvider
	candidate.model = resolvedModel
	candidate.rebuilt = true
	keepSession = true
	return candidate, nil
}

// applySessionResume shares the startup gate, candidate hydration, and
// commit order used by interactive and non-interactive session opening. The
// old append handle is closed only after the candidate has been prepared and
// seeded successfully.
func applySessionResume(sess *core.Session, ag *core.Agent, currentProvider, currentModel string, explicitProvider, explicitModel bool, buildAgentFor func(string, string) (*core.Agent, string, string, error)) (sessionResumeCandidate, error) {
	candidate := sessionResumeCandidate{
		session:          sess,
		agent:            ag,
		fullMessageCount: 0,
		provider:         currentProvider,
		model:            currentModel,
	}
	if ag != nil {
		candidate.fullMessageCount = len(ag.Messages())
	}
	if sess == nil || ag == nil {
		return candidate, nil
	}
	ag.SetSessionTimeContext(sess.Meta.Started, sess.Meta.Timezone, sess.Meta.TimezoneOffset)
	// A newly created meta-only session is already on the active pair. Avoid
	// reopening and closing it through the resume path: Close intentionally
	// removes fresh empty sessions, and a second append handle would otherwise
	// keep an unlinked file alive.
	if len(ag.Messages()) == 0 &&
		strings.TrimSpace(sess.Meta.Provider) == strings.TrimSpace(currentProvider) &&
		strings.TrimSpace(sess.Meta.Model) == strings.TrimSpace(currentModel) {
		ag.SetPlan(sess.Meta.PlanSteps())
		return candidate, nil
	}
	candidate, err := prepareSessionResumeWithOptions(sess.Path, ag, currentProvider, currentModel, explicitProvider, explicitModel, false, buildAgentFor)
	if err != nil {
		return sessionResumeCandidate{}, joinSessionCloseError(err, sess)
	}
	committed := false
	defer func() {
		if !committed && candidate.rebuilt && candidate.agent != ag {
			closeAgentLSP(candidate.agent)
		}
	}()
	// A replacement agent is still private at this point. Bind the durable
	// identity before it can be published or run any startup work.
	if err := candidate.agent.BindSessionID(candidate.session.Meta.ID); err != nil {
		return sessionResumeCandidate{}, joinSessionCloseError(err, candidate.session)
	}
	if !candidate.rebuilt {
		candidate.agent.SetMessages(candidate.messages)
		candidate.agent.SeedCost(candidate.cumulative)
		candidate.agent.SeedLastTurnUsage(candidate.lastTurn)
	}
	if closeErr := sess.Close(); closeErr != nil {
		return sessionResumeCandidate{}, errors.Join(fmt.Errorf("close session: %w", closeErr), joinSessionCloseError(nil, candidate.session))
	}
	candidate.agent.SetSessionTimeContext(candidate.session.Meta.Started, candidate.session.Meta.Timezone, candidate.session.Meta.TimezoneOffset)
	// Seed the agent-owned plan from the loaded session so a resumed session's
	// core state matches its file: the transcript holds steps while core would
	// otherwise hold none, which makes show answer "Plan is empty" and indexed
	// update/remove fail. Done after the candidate is fully hydrated but before
	// the caller commits it.
	candidate.agent.SetPlan(candidate.session.Meta.PlanSteps())
	committed = true
	if candidate.agent != ag {
		closeAgentLSP(ag)
	}
	return candidate, nil
}

// fanoutAgentEvent translates a core.AgentEvent into the wire-format
// EventFromHost and pushes it through the extension manager. Only
// the events that have a clear extension-facing meaning are
// forwarded; internal-only ones (text_delta, tool_progress) are
// dropped to keep the per-extension stream sane.
func sessionContext(sess *core.Session) *extproto.SessionContext {
	if sess == nil {
		return nil
	}
	return &extproto.SessionContext{
		ID:        sess.Meta.ID,
		ParentID:  sess.Meta.Parent,
		Path:      sess.Path,
		CWD:       sess.Meta.CWD,
		ForkPoint: sess.Meta.ForkPoint,
	}
}

func announceSession(mgr *extensions.Manager, sess *core.Session) {
	if mgr == nil {
		return
	}
	var states map[string]json.RawMessage
	if sess != nil {
		states = copyExtensionStates(sess.ExtensionState)
	}
	mgr.EmitSessionEvent("session_start", sessionContext(sess), states)
	if sess != nil {
		mgr.EmitSessionEvent("session_opened", sessionContext(sess), states)
	}
}

func copyExtensionStates(states map[string]json.RawMessage) map[string]json.RawMessage {
	if len(states) == 0 {
		return nil
	}
	out := make(map[string]json.RawMessage, len(states))
	for name, state := range states {
		out[name] = append(json.RawMessage(nil), state...)
	}
	return out
}

var (
	errGoalActiveReplacement  = errors.New("active goal replacement")
	errGoalMissionMismatch    = errors.New("goal mission mismatch")
	errGoalStaleUpdate        = errors.New("stale goal update")
	errGoalStorageUnavailable = errors.New("goal storage unavailable")
	errGoalStateWrite         = errors.New("goal state write")

	errPlanStorageUnavailable = errors.New("plan storage unavailable")
	errPlanStateWrite         = errors.New("plan state write")
)

func persistToolResultState(mgr *extensions.Manager, sess *core.Session, result core.ToolResult, goalMaxTokenBudget ...*uint64) error {
	if sess == nil {
		if _, ok := tools.GoalUpdateFromResult(result); ok {
			return goalCommitError(errGoalStorageUnavailable)
		}
		if result.PlanOperation != nil && !result.IsError {
			return planCommitError(errPlanStorageUnavailable)
		}
		return nil
	}
	if details, ok := result.Details.(extensions.ToolResultDetails); ok && details.Extension != "" && len(details.State) > 0 {
		if err := sess.AppendExtensionState(details.Extension, details.State); err != nil {
			return fmt.Errorf("extension state: %w", err)
		}
		if mgr != nil {
			mgr.UpdateSessionState(details.Extension, details.State)
		}
	}
	if err := persistGoalToolResult(sess, result, goalMaxTokenBudget...); err != nil {
		return goalCommitError(err)
	}
	if err := persistPlanToolResult(sess, result); err != nil {
		return planCommitError(err)
	}
	return nil
}

func goalCommitError(err error) error {
	return &core.ToolResultCommitError{Message: safeGoalCommitMessage(err), Err: fmt.Errorf("goal state: %w", err)}
}

func safeGoalCommitMessage(err error) string {
	switch {
	case errors.Is(err, errGoalActiveReplacement):
		return "goal transition rejected: the current goal is still active"
	case errors.Is(err, errGoalMissionMismatch):
		return "goal transition rejected: it does not belong to the active mission"
	case errors.Is(err, errGoalStaleUpdate):
		return "goal transition rejected: the active goal changed"
	case errors.Is(err, errGoalStorageUnavailable):
		return "goal state unavailable: session persistence is disabled"
	case errors.Is(err, errGoalStateWrite):
		return "goal state could not be saved"
	default:
		return "goal update could not be persisted"
	}
}

func planCommitError(err error) error {
	return &core.ToolResultCommitError{Message: safePlanCommitMessage(err), Err: fmt.Errorf("plan state: %w", err)}
}

func safePlanCommitMessage(err error) string {
	switch {
	case errors.Is(err, errPlanStorageUnavailable):
		return "plan state unavailable: session persistence is disabled"
	case errors.Is(err, errPlanStateWrite):
		return "plan state could not be saved"
	default:
		return "plan update could not be persisted"
	}
}

func persistGoalToolResult(sess *core.Session, result core.ToolResult, goalMaxTokenBudget ...*uint64) error {
	if sess == nil {
		return nil
	}
	update, ok := tools.GoalUpdateFromResult(result)
	if !ok {
		return nil
	}
	if update.Status == core.GoalSuperseded {
		if sess.Meta.Goal == nil || sess.Meta.Goal.Status != core.GoalActive {
			return nil
		}
		if update.GoalID != sess.Meta.Goal.ID {
			return errGoalStaleUpdate
		}
		if sess.Meta.Mission != nil && update.MissionID != "" && update.MissionID != sess.Meta.Mission.ID {
			return errGoalMissionMismatch
		}
		replacement := &core.SessionGoal{Objective: update.Objective, Status: core.GoalActive, Owner: core.GoalOwnerManager}
		if len(goalMaxTokenBudget) > 0 && goalMaxTokenBudget[0] != nil && *goalMaxTokenBudget[0] > 0 {
			budget := *goalMaxTokenBudget[0]
			replacement.TokenBudget = &budget
		}
		if err := sess.SupersedeGoal(replacement); err != nil {
			return fmt.Errorf("%w: %w", errGoalStateWrite, err)
		}
		return nil
	}
	if update.Status == core.GoalActive {
		if strings.TrimSpace(update.Objective) == "" {
			if sess.Meta.Goal == nil || sess.Meta.Goal.Status != core.GoalPaused {
				return nil
			}
			if sess.Meta.Mission != nil && update.MissionID != "" && update.MissionID != sess.Meta.Mission.ID {
				return errGoalMissionMismatch
			}
			goal := *sess.Meta.Goal
			goal.Status = core.GoalActive
			goal.Reason = ""
			if err := sess.UpdateGoal(&goal); err != nil {
				return fmt.Errorf("%w: %w", errGoalStateWrite, err)
			}
			return nil
		}
		if sess.Meta.Goal != nil && sess.Meta.Goal.Status == core.GoalActive {
			return errGoalActiveReplacement
		}
		if sess.Meta.Mission != nil && update.MissionID != "" && update.MissionID != sess.Meta.Mission.ID {
			return errGoalMissionMismatch
		}
		goal := &core.SessionGoal{Objective: update.Objective, Status: core.GoalActive, Owner: core.GoalOwnerManager}
		if len(goalMaxTokenBudget) > 0 && goalMaxTokenBudget[0] != nil && *goalMaxTokenBudget[0] > 0 {
			budget := *goalMaxTokenBudget[0]
			goal.TokenBudget = &budget
		}
		if err := sess.UpdateGoal(goal); err != nil {
			return fmt.Errorf("%w: %w", errGoalStateWrite, err)
		}
		return nil
	}
	if sess.Meta.Goal == nil || sess.Meta.Goal.Status != core.GoalActive {
		return nil
	}
	if sess.Meta.Mission != nil && update.MissionID != "" && update.MissionID != sess.Meta.Mission.ID {
		return errGoalMissionMismatch
	}
	goal := *sess.Meta.Goal
	goal.Status = update.Status
	goal.Reason = update.Reason
	if err := sess.UpdateGoal(&goal); err != nil {
		return fmt.Errorf("%w: %w", errGoalStateWrite, err)
	}
	return nil
}

// persistPlanToolResult writes the materialized checklist for a validated plan
// mutation. show carries neither Details nor PlanOperation, and error results
// were already rejected, so both are skipped here.
func persistPlanToolResult(sess *core.Session, result core.ToolResult) error {
	if sess == nil || result.IsError || result.PlanOperation == nil {
		return nil
	}
	update, ok := result.Details.(core.PlanUpdate)
	if !ok {
		return nil
	}
	if err := sess.UpdatePlan(&core.SessionPlan{Steps: update.Plan}); err != nil {
		return fmt.Errorf("%w: %w", errPlanStateWrite, err)
	}
	return nil
}

func fanoutAgentEvent(mgr *extensions.Manager, ev core.AgentEvent) {
	if mgr == nil {
		return
	}
	switch e := ev.(type) {
	case core.EvTurnStart:
		mgr.EmitEvent(extproto.EventFromHost{Event: "turn_start", Step: e.Step})
	case core.EvToolCall:
		mgr.EmitEvent(extproto.EventFromHost{
			Event: "tool_call", ToolID: e.ID, ToolName: e.Name, ToolArgs: e.Args,
		})
	case core.EvAssistantMessage:
		// Concat the visible text portions of the message; binary
		// blocks (tool_use, etc.) are skipped because subscribers
		// usually want a string they can grep / display.
		var text string
		for _, c := range e.Message.Content {
			if tb, ok := c.(provider.TextBlock); ok {
				text += tb.Text
			}
		}
		mgr.EmitEvent(extproto.EventFromHost{Event: "assistant_message", Text: text})
	case core.EvTurnEnd:
		ev := extproto.EventFromHost{Event: "turn_end", Stop: string(e.Stop)}
		if e.Err != nil {
			ev.Error = e.Err.Error()
		}
		mgr.EmitEvent(ev)
	}
}

// Run is the top-level entrypoint for the zut binary.
func Run(rawArgs []string, version string) error {
	// Apply network configuration before any subcommand can make an HTTP
	// request. Standard proxy environment variables retain precedence.
	applyConfiguredHTTPProxy()

	// Extension installs can invoke external build and clone processes.
	// Give those processes a cancellation context without changing signal
	// handling for the interactive modes below.
	extCtx := context.Background()
	stopExtContext := func() {}
	if len(rawArgs) > 0 && (rawArgs[0] == "ext" || rawArgs[0] == "mcp") {
		extCtx, stopExtContext = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	}
	defer stopExtContext()

	// Daemon client subcommands run before every other router so their
	// names never reach the generic flag parser.
	if len(rawArgs) > 0 {
		if cmd := daemonClientSubcommand(rawArgs[0]); cmd != nil {
			if code := cmd(rawArgs[1:], os.Stdout, os.Stderr); code != 0 {
				os.Exit(code)
			}
			return nil
		}
	}

	// Subcommand router: `zut bot ...` is handled separately so the
	// generic flag parser doesn't reject "bot" as a positional arg.
	if handled, err := runBotCommand(rawArgs, version); handled {
		return err
	}
	if handled, err := runExtCommand(extCtx, rawArgs, version); handled {
		return err
	}
	if handled, err := runMCPCommand(extCtx, rawArgs, version, os.Stdout); handled {
		return err
	}
	if handled, err := runUpdateCommand(rawArgs, version); handled {
		return err
	}
	if handled, err := runDaemonCommand(rawArgs, version); handled {
		return err
	}
	if handled, err := runSessionsCommand(rawArgs); handled {
		return err
	}
	if handled, err := runZutfileCommand(rawArgs, version); handled {
		return err
	}
	return runWithArgsRaw(rawArgs, version)
}

func runWithArgsRaw(rawArgs []string, version string) error {
	// `zut rpc` is shorthand for `zut --rpc` so third-party apps can
	// spawn the binary with a clean argv. Strip the leading 'rpc'
	// token and let the rest flow through the normal arg parser.
	if len(rawArgs) > 0 && rawArgs[0] == "rpc" {
		rawArgs = append([]string{"--rpc"}, rawArgs[1:]...)
	}

	if fi, err := os.Stdin.Stat(); err == nil {
		rawArgs = argsForStdin(rawArgs, fi.Mode())
	}
	args, err := ParseArgs(rawArgs)
	if err != nil {
		PrintHelp(version)
		return err
	}
	if args.Help {
		PrintHelp(version)
		return nil
	}
	if args.Version {
		fmt.Println("zut", version)
		return nil
	}
	// Reject invalid orchestration combinations before catalog repair or model
	// refresh can mutate local state or launch background work.
	if err := validateOrchestrationArgs(args); err != nil {
		return err
	}
	// Model catalog: load any cached discovery data before we inspect
	// the model list (list-models, print/json, interactive).
	userModels := LoadUserModels()
	PrepareRuntimeCatalog(context.Background(), args.ListModels, args.Provider, args.APIKey, args.BaseURL, userModels, args.Model)

	if args.ListModels {
		printModels()
		return nil
	}

	return runClientWithArgs(args, rawArgs, version)
}

// daemonClientSubcommand returns the daemon client handler for name, or nil.
func daemonClientSubcommand(name string) func([]string, io.Writer, io.Writer) int {
	switch name {
	case "ls":
		return runLsCommand
	case "tree":
		return runTreeCommand
	case "kill":
		return runKillCommand
	case "attach":
		return runAttachCommand
	}
	return nil
}

// runClientWithArgs dispatches a parsed invocation to the daemon client
// paths.
func runClientWithArgs(args Args, rawArgs []string, version string) error {
	if err := validateOrchestrationArgs(args); err != nil {
		return err
	}
	ctx := context.Background()
	switch args.Mode {
	case ModePrint:
		return runPrintClient(ctx, args, rawArgs, version)
	case ModeStream:
		return runStreamClient(ctx, args, rawArgs, version)
	case ModeJSON:
		return runJSONClient(ctx, args, rawArgs, version)
	case ModeRPC:
		return runRPCClient(ctx, args, rawArgs, version)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	spec, err := buildCreateSpec(ctx, args, rawArgs, cwd, os.Environ(), defaultSessionLocator())
	if err != nil {
		return err
	}
	if code := runTUIClient(ctx, "", spec, os.Stderr); code != 0 {
		os.Exit(code)
	}
	return nil
}

// sessionLocator resolves session files on the client side, mirroring
// openOrCreateSessionState.
type sessionLocator struct {
	latest   func(root, cwd string) string
	findByID func(ctx context.Context, id string) (string, error)
	pick     func(root, cwd string) (string, error)
	prune    func(root, cwd string)
}

func defaultSessionLocator() sessionLocator {
	return sessionLocator{
		latest: core.LatestSession,
		findByID: func(ctx context.Context, id string) (string, error) {
			return core.FindManagedSessionByID(ctx, ZutHome(), id)
		},
		pick:  pickSession,
		prune: core.PruneEmptySessions,
	}
}

// buildCreateSpec maps parsed args to the daemon CreateSpec. ResumePath is
// chosen like openOrCreateSessionState; empty means a fresh session. A
// missing --session file is created by the daemon (openOrCreateSessionState).
// A bare --resume whose picker returns nothing starts fresh, as before, so the
// resume flag is dropped from Args (the daemon refuses an empty resume).
func buildCreateSpec(ctx context.Context, args Args, rawArgs []string, cwd string, env []string, loc sessionLocator) (*session.CreateSpec, error) {
	spec := &session.CreateSpec{CWD: cwd, Env: env, Args: rawArgs, Zutfile: args.zutfile}
	if args.NoSess {
		return spec, nil
	}
	root := agentSessionsRoot(ZutHome(), args)
	// Explicit paths and UUIDs may identify empty sessions; keep them.
	if args.ResumeSessionID == "" && args.Session == "" && loc.prune != nil {
		loc.prune(root, args.CWD)
	}
	switch {
	case args.Session != "":
		spec.ResumePath = args.Session
	case args.Continue:
		spec.ResumePath = loc.latest(root, args.CWD)
	case args.Resume && args.ResumeSessionID != "":
		path, err := loc.findByID(ctx, args.ResumeSessionID)
		if err != nil {
			return nil, err
		}
		if path == "" {
			return nil, fmt.Errorf("session %q not found", args.ResumeSessionID)
		}
		spec.ResumePath = path
	case args.Resume:
		path, err := loc.pick(root, args.CWD)
		if err != nil {
			return nil, err
		}
		spec.ResumePath = path
		if path == "" {
			spec.Args = withoutResumeFlag(rawArgs)
		}
	}
	return spec, nil
}

// withoutResumeFlag drops a bare -r/--resume from rawArgs.
func withoutResumeFlag(rawArgs []string) []string {
	out := make([]string, 0, len(rawArgs))
	for _, a := range rawArgs {
		if a == "-r" || a == "--resume" {
			continue
		}
		out = append(out, a)
	}
	return out
}

type printStats struct {
	Provider              string `json:"provider"`
	Model                 string `json:"model"`
	PromptTokens          int    `json:"prompt_tokens"`
	ReasoningTokens       *int   `json:"reasoning_tokens"`
	GeneratedOutputTokens int    `json:"generated_output_tokens"`
	ElapsedMS             int64  `json:"elapsed_ms"`
}

func writePrintStats(path, providerID, model string, usage provider.Usage, elapsed time.Duration) error {
	var reasoning *int
	generated := usage.OutputTokens
	if usage.ReasoningTokensKnown {
		count := usage.ReasoningTokens
		reasoning = &count
		generated -= count
		if generated < 0 {
			generated = 0
		}
	}
	stats := printStats{
		Provider:              providerID,
		Model:                 model,
		PromptTokens:          usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens,
		ReasoningTokens:       reasoning,
		GeneratedOutputTokens: generated,
		ElapsedMS:             elapsed.Milliseconds(),
	}
	data, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		return fmt.Errorf("encode stats: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write stats %q: %w", path, err)
	}
	return nil
}

// runZutfileStartupPreEnv runs zutfile entry.pre. "!command" values execute
// via BashTool with env; other values are sent as an agent turn. shellOut
// receives live shell chunks and sink the agent events of a plain-text pre.
func runZutfileStartupPreEnv(ctx context.Context, pre, cwd string, env []string, sandbox *tools.Sandbox, ag *core.Agent, sink func(core.AgentEvent), shellOut io.Writer) error {
	pre = strings.TrimSpace(pre)
	if pre == "" {
		return nil
	}
	if cmd, ok := modes.ShellEscapeCommand(pre); ok {
		raw, err := json.Marshal(map[string]any{
			"command": cmd,
			"timeout": modes.ShellEscapeTimeoutSeconds,
		})
		if err != nil {
			return err
		}
		if shellOut != nil {
			fmt.Fprintf(shellOut, "$ %s\n", cmd)
			if f, ok := shellOut.(interface{ Sync() error }); ok {
				_ = f.Sync()
			}
		}
		progress := func(chunk string) {
			if shellOut == nil || chunk == "" {
				return
			}
			fmt.Fprint(shellOut, chunk)
			if f, ok := shellOut.(interface{ Sync() error }); ok {
				_ = f.Sync()
			}
		}
		bash := &tools.BashTool{CWD: cwd, Env: env, Sandbox: sandbox}
		res, err := bash.Execute(ctx, raw, progress)
		if err != nil {
			return fmt.Errorf("zutfile entry.pre: %w", err)
		}
		if res.IsError {
			var sb strings.Builder
			for _, c := range res.Content {
				if tb, ok := c.(provider.TextBlock); ok {
					sb.WriteString(tb.Text)
				}
			}
			msg := strings.TrimSpace(sb.String())
			if msg == "" {
				msg = "command failed"
			}
			return fmt.Errorf("zutfile entry.pre: %s", msg)
		}
		return nil
	}
	if ag == nil {
		return fmt.Errorf("zutfile entry.pre requires an agent for non-shell prompts")
	}
	if sink == nil {
		sink = func(core.AgentEvent) {}
	}
	return ag.Prompt(ctx, pre, nil, sink)
}

var errInteractiveAgentChanged = errors.New("interactive agent or web-search policy changed during prompt refresh")

// refreshAgentToolsAndPrompt re-resolves tools (including rediscovered
// skills and currently loaded extension tools) and updates the live
// agent's registry and system prompt. Used after /reload-ext and after
// zutfile entry.pre installs new skills or extensions.
// mutateRegistry, if non-nil, can inject session-specific tools (e.g. the subagent tool).
// interactive, when non-nil, serializes the final commit with agent replacement.
func refreshAgentToolsAndPrompt(args Args, sharedSandbox *tools.Sandbox, extToolAdapter ExtensionToolSource, ag *core.Agent, mutateRegistry func(core.Registry) core.Registry, interactive *modes.Interactive) (subagents.WebSearchPolicy, error) {
	if ag == nil {
		return subagents.WebSearchDeny, nil
	}
	var webSearchPolicyGeneration uint64
	if interactive != nil {
		// Capture before Resolve: the final commit must fail if settings,
		// Telegram attachment, or a newer registry commit changes policy while
		// filesystem/config/extension discovery is in flight.
		webSearchPolicyGeneration = interactive.WebSearchPolicyGeneration()
	}
	resolved, err := Resolve(args, true)
	if err != nil {
		return subagents.WebSearchDeny, fmt.Errorf("refresh agent prompt and tools: %w", err)
	}
	if sharedSandbox != nil {
		resolved.UseSandbox(sharedSandbox)
	}
	if extToolAdapter != nil {
		resolved.MergeExtensionTools(extToolAdapter)
	}
	reg := resolved.ToolRegistry
	if resolved.WebSearchPolicy != subagents.WebSearchAllow {
		tools.RemoveWebCapabilities(reg)
	}
	if mutateRegistry != nil {
		reg = mutateRegistry(reg)
	}
	if interactive != nil {
		oldTools, applied := interactive.ApplyAgentPromptConfigAtWebSearchGeneration(ag, resolved.SystemPrompt, reg, webSearchPolicyGeneration)
		if !applied {
			_ = tools.CloseLSPManagers(reg)
			return subagents.WebSearchDeny, errInteractiveAgentChanged
		}
		interactive.DeferUntilIdle(func() {
			_ = tools.CloseLSPManagers(oldTools)
		})
		return webSearchPolicyForRegistry(resolved.WebSearchPolicy, ag.ToolsSnapshot()), nil
	}
	oldTools := ag.SetPromptConfig(resolved.SystemPrompt, reg)
	_ = tools.CloseLSPManagers(oldTools)
	return webSearchPolicyForRegistry(resolved.WebSearchPolicy, reg), nil
}

func webSearchPolicyForRegistry(policy subagents.WebSearchPolicy, reg core.Registry) subagents.WebSearchPolicy {
	if policy != subagents.WebSearchAllow {
		return subagents.WebSearchDeny
	}
	for _, name := range tools.WebCapabilityNames {
		if _, ok := reg[name]; !ok {
			return subagents.WebSearchDeny
		}
	}
	return subagents.WebSearchAllow
}

func webSearchToolAllowedForSession(args Args) bool {
	if args.NoTools || args.PermissionSet != nil {
		return false
	}
	if args.ToolsSet || len(args.Tools) > 0 {
		return toolListContains(args.Tools, "web_search")
	}
	return true
}

func agentSessionsRoot(root string, args Args) string {
	if args.AgentName == "" {
		return root
	}
	return filepath.Join(root, "sessions", "agents", safeAgentName(args.AgentName))
}

func pickSession(root, cwd string) (string, error) {
	files := core.ListSessions(root, cwd)
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "no sessions for", cwd)
		return "", nil
	}
	for i, f := range files {
		fmt.Fprintf(os.Stderr, "  %2d) %s\n", i+1, f)
	}
	fmt.Fprint(os.Stderr, "pick #: ")
	rd := bufio.NewReader(os.Stdin)
	line, _ := rd.ReadString('\n')
	line = strings.TrimSpace(line)
	var n int
	if _, err := fmt.Sscanf(line, "%d", &n); err != nil || n < 1 || n > len(files) {
		return "", fmt.Errorf("invalid selection")
	}
	return files[n-1], nil
}

// writeNewTranscriptLocked is the same as WriteNewTranscript. The
// suffix marks that interactive callers must hold persistMu when
// invoking it so concurrent appends from the agent loop don't race
// with this catch-up flush.
func writeNewTranscriptLocked(ag *core.Agent, sess *core.Session, from int) (next int, err error) {
	next = from
	if sess == nil || ag == nil {
		return next, nil
	}
	msgs, total := ag.MessagesFrom(from)
	for offset, msg := range msgs {
		index := from + offset
		if err := sess.AppendMessage(msg); err != nil {
			return next, fmt.Errorf("append transcript message %d: %w", index, err)
		}
		next = index + 1
	}
	if len(msgs) == 0 {
		next = total
	}
	cum := ag.Cost()
	if err := sess.AppendUsage(cum, cum); err != nil {
		return next, fmt.Errorf("append transcript usage: %w", err)
	}
	if err := sess.Flush(); err != nil {
		return next, fmt.Errorf("flush transcript: %w", err)
	}
	return next, nil
}

func argsForStdin(rawArgs []string, mode os.FileMode) []string {
	if mode&os.ModeCharDevice != 0 {
		return rawArgs
	}
	return append([]string{"--print"}, rawArgs...)
}

func combinePromptInput(stdin, positional string) string {
	parts := make([]string, 0, 2)
	if stdin = strings.TrimSpace(stdin); stdin != "" {
		parts = append(parts, stdin)
	}
	if positional = strings.TrimSpace(positional); positional != "" {
		parts = append(parts, positional)
	}
	return strings.Join(parts, "\n")
}

// readAllPipe reads f to EOF unless it is a terminal.
func readAllPipe(f *os.File) (string, error) {
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if (fi.Mode() & os.ModeCharDevice) != 0 {
		return "", nil
	}
	return readAllStdinFrom(f)
}

func readAllStdinFrom(r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
	return string(b), err
}

func printModels() {
	models := provider.Active()

	// Compute column widths from actual data so wide providers (e.g.
	// xiaomi-token-plan-sgp) and long bedrock model ids don't force the
	// `name` column off-screen. Floors mirror the historical layout so
	// short catalogs look the same as before.
	provW, idW, srcW := len("provider"), len("model id"), len("source")
	for _, m := range models {
		if w := len(m.Provider); w > provW {
			provW = w
		}
		if w := len(m.ID); w > idW {
			idW = w
		}
		source := m.Source
		if source == "" {
			source = "catalog"
		}
		if m.Speculative {
			source = "speculative"
		}
		if w := len(source); w > srcW {
			srcW = w
		}
	}

	header := fmt.Sprintf("%-*s  %-*s  %8s  %8s  %s  %-*s  %s",
		provW, "provider",
		idW, "model id",
		"context", "max-out", "reasoning",
		srcW, "source",
		"name")
	fmt.Println(header)

	for _, m := range models {
		reason := " "
		if m.Reasoning {
			reason = "✓"
		}
		source := m.Source
		if source == "" {
			source = "catalog"
		}
		if m.Speculative {
			source = "speculative"
		}
		fmt.Printf("%-*s  %-*s  %8d  %8d     %s      %-*s  %s\n",
			provW, m.Provider,
			idW, m.ID,
			m.ContextWindow, m.MaxOutput,
			reason,
			srcW, source,
			m.DisplayName)
	}
}

// confirmationKeyFor returns the session-scoped confirmation grant key for a
// tool call. A tool that implements core.ConfirmationKeyer may expose a finer
// key than its name (the subagent facade keys one grant per action); every
// other tool, and any unknown tool, keeps its own name. It mirrors the
// confirmation gate's lookup so a remembered grant matches the call that will
// actually run.
func confirmationKeyFor(reg core.Registry, name string, args json.RawMessage) string {
	tool, err := reg.Get(name)
	if err != nil {
		return name
	}
	return core.ConfirmationKey(tool, name, args)
}
