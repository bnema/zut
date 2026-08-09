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
	"github.com/bnema/zut/packages/agent/skills"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/auth"
	"github.com/bnema/zut/packages/tui"
)

// interactiveExtHooks is a tiny adapter that lets the extension
// manager call back into the Interactive instance built later in
// runInteractive. Alerts are buffered until the instance is attached so
// an extension that signals attention during startup is not silently lost.
// Keep the startup buffer bounded because extensions can flood their host.
const maxBufferedInteractiveAlerts = 64

type interactiveExtHooks struct {
	mu              sync.Mutex
	interactive     *modes.Interactive
	pendingAlerts   []interactiveAlert
	pendingStatuses map[string]interactiveStatus
	pendingWidgets  map[string]interactiveWidget
}

type interactiveAlert struct {
	extName string
	alert   extproto.AlertRequest
}

type interactiveStatus struct {
	extName string
	key     string
	level   string
	text    string
}

type interactiveWidget struct {
	extName  string
	id       string
	position string
	title    string
	lines    []string
}

func (h *interactiveExtHooks) iv() *modes.Interactive {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.interactive
}

func (h *interactiveExtHooks) attachInteractive(iv *modes.Interactive) {
	if h == nil || iv == nil {
		return
	}
	h.mu.Lock()
	h.interactive = iv
	pendingAlerts := h.pendingAlerts
	pendingStatuses := h.pendingStatuses
	pendingWidgets := h.pendingWidgets
	h.pendingAlerts = nil
	h.pendingStatuses = nil
	h.pendingWidgets = nil
	h.mu.Unlock()
	for _, item := range pendingAlerts {
		iv.Alert(item.extName, item.alert)
	}
	for _, item := range pendingStatuses {
		iv.SetStatus(item.extName, item.key, item.level, item.text)
	}
	for _, item := range pendingWidgets {
		iv.SetWidget(item.extName, item.id, item.position, item.title, item.lines)
	}
}

func (h *interactiveExtHooks) Notify(extName, level, message string) {
	if iv := h.iv(); iv != nil {
		iv.Notify(extName, level, message)
	}
}
func (h *interactiveExtHooks) Alert(extName string, alert extproto.AlertRequest) {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.interactive == nil {
		if len(h.pendingAlerts) < maxBufferedInteractiveAlerts {
			h.pendingAlerts = append(h.pendingAlerts, interactiveAlert{extName: extName, alert: alert})
		}
		h.mu.Unlock()
		return
	}
	iv := h.interactive
	h.mu.Unlock()
	iv.Alert(extName, alert)
}
func (h *interactiveExtHooks) Submit(text string) {
	if iv := h.iv(); iv != nil {
		iv.SubmitOrQueue(text, nil)
	}
}
func (h *interactiveExtHooks) SubmitSlash(text string) {
	if iv := h.iv(); iv != nil {
		iv.SubmitSlash(text)
	}
}
func (h *interactiveExtHooks) Insert(text string) {
	if iv := h.iv(); iv != nil {
		iv.Insert(text)
	}
}
func (h *interactiveExtHooks) Display(extName, text string) {
	if iv := h.iv(); iv != nil {
		iv.Display(extName, text)
	}
}
func (h *interactiveExtHooks) ClearNotes(extName string) {
	if iv := h.iv(); iv != nil {
		iv.ClearNotes(extName)
	}
}
func (h *interactiveExtHooks) OpenPanel(extName string, spec extproto.PanelSpec) {
	if iv := h.iv(); iv != nil {
		iv.OpenPanel(extName, spec)
	}
}
func (h *interactiveExtHooks) UpdatePanel(extName, panelID, title string, lines []string, footer string) {
	if iv := h.iv(); iv != nil {
		iv.UpdatePanel(extName, panelID, title, lines, footer)
	}
}
func (h *interactiveExtHooks) ClosePanel(extName, panelID string) {
	if iv := h.iv(); iv != nil {
		iv.ClosePanel(extName, panelID)
	}
}
func pendingChromeKey(extName, key string) string { return extName + "\x00" + key }

func (h *interactiveExtHooks) SetStatus(extName, key, level, text string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.interactive == nil {
		if h.pendingStatuses == nil {
			h.pendingStatuses = map[string]interactiveStatus{}
		}
		pendingKey := pendingChromeKey(extName, key)
		if strings.TrimSpace(text) == "" {
			delete(h.pendingStatuses, pendingKey)
		} else {
			h.pendingStatuses[pendingKey] = interactiveStatus{extName: extName, key: key, level: level, text: text}
		}
		h.mu.Unlock()
		return
	}
	iv := h.interactive
	h.mu.Unlock()
	iv.SetStatus(extName, key, level, text)
}
func (h *interactiveExtHooks) SetWidget(extName, id, position, title string, lines []string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.interactive == nil {
		if h.pendingWidgets == nil {
			h.pendingWidgets = map[string]interactiveWidget{}
		}
		h.pendingWidgets[pendingChromeKey(extName, id)] = interactiveWidget{
			extName: extName, id: id, position: position, title: title, lines: append([]string(nil), lines...),
		}
		h.mu.Unlock()
		return
	}
	iv := h.interactive
	h.mu.Unlock()
	iv.SetWidget(extName, id, position, title, lines)
}
func (h *interactiveExtHooks) ClearWidget(extName, id string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.interactive == nil {
		if h.pendingWidgets != nil {
			delete(h.pendingWidgets, pendingChromeKey(extName, id))
		}
		h.mu.Unlock()
		return
	}
	iv := h.interactive
	h.mu.Unlock()
	iv.ClearWidget(extName, id)
}

func (h *interactiveExtHooks) ClearExtensionChrome(extName string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.interactive == nil {
		prefix := extName + "\x00"
		for key := range h.pendingStatuses {
			if strings.HasPrefix(key, prefix) {
				delete(h.pendingStatuses, key)
			}
		}
		for key := range h.pendingWidgets {
			if strings.HasPrefix(key, prefix) {
				delete(h.pendingWidgets, key)
			}
		}
		h.mu.Unlock()
		return
	}
	iv := h.interactive
	h.mu.Unlock()
	iv.ClearExtensionChrome(extName)
}

// extToolAdapter bridges *extensions.Manager to the
// ExtensionToolSource interface declared in build.go (kept narrow to
// avoid a build->extensions import cycle). One adapter instance per
// run; used at every Resolve point so re-built agents pick up the
// same set of extension tools.
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
	})
}

// liveInteractiveAgent returns the agent currently owned by the TUI. The
// fallback is only used before the Interactive has been constructed.
//
// Agent rebuilds, such as cross-provider model switches, replace the TUI's
// agent without replacing the startup pointer held by runInteractive. Session
// operations must therefore resolve the live agent at the time of the action.
func liveInteractiveAgent(iv *modes.Interactive, fallback *core.Agent) *core.Agent {
	if iv != nil {
		return iv.Agent()
	}
	return fallback
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

		cfg, err := LoadConfig()
		if err != nil {
			if notify != nil {
				notify("model changed for this run, but config could not be read: " + err.Error())
			}
			return
		}
		cfg.Provider = providerName
		cfg.Model = model
		if err := SaveConfig(cfg); err != nil && notify != nil {
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

func closeResolvedLSP(r Resolved) {
	_ = tools.CloseLSPManagers(r.ToolRegistry)
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
	return prepareSessionResumeWithOptions(path, current, currentProvider, currentModel, false, false, buildAgentFor)
}

// prepareSessionResumeWithOptions is the provider/model-aware implementation.
// An explicit CLI provider or model keeps that field authoritative; an empty
// field falls back to the selected session's metadata when it is complete.
func prepareSessionResumeWithOptions(path string, current *core.Agent, currentProvider, currentModel string, explicitProvider, explicitModel bool, buildAgentFor func(string, string) (*core.Agent, string, string, error)) (candidate sessionResumeCandidate, err error) {
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
	if resumeProvider == currentProvider && resumeModel == currentModel {
		keepSession = true
		return candidate, nil
	}
	if buildAgentFor == nil {
		return sessionResumeCandidate{}, fmt.Errorf("cannot resume session with provider %q and model %q: no agent builder configured", resumeProvider, resumeModel)
	}

	rebuilt, resolvedProvider, resolvedModel, err := buildAgentFor(resumeProvider, resumeModel)
	if err != nil {
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

func buildNonInteractiveSessionAgentWithRegistry(ctx context.Context, args Args, base Resolved, extMgr *extensions.Manager, providerName, model string, prepareRegistry func(core.Registry) core.Registry) (*core.Agent, string, string, error) {
	next := args
	next.Provider = providerName
	next.Model = model
	resolved, err := Resolve(next, true)
	if err != nil {
		return nil, "", "", err
	}
	resolved.UseSandbox(base.Sandbox)
	if extMgr != nil {
		resolved.MergeExtensionTools(&extToolAdapter{mgr: extMgr})
	}
	if prepareRegistry != nil {
		prepareRegistry(resolved.ToolRegistry)
	}
	ag := resolved.NewAgent()
	wireNonInteractiveAgentExtHooks(ctx, ag, extMgr)
	return ag, resolved.Provider, resolved.Model, nil
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
	// A newly created meta-only session is already on the active pair. Avoid
	// reopening and closing it through the resume path: Close intentionally
	// removes fresh empty sessions, and a second append handle would otherwise
	// keep an unlinked file alive.
	if len(ag.Messages()) == 0 &&
		strings.TrimSpace(sess.Meta.Provider) == strings.TrimSpace(currentProvider) &&
		strings.TrimSpace(sess.Meta.Model) == strings.TrimSpace(currentModel) {
		return candidate, nil
	}
	candidate, err := prepareSessionResumeWithOptions(sess.Path, ag, currentProvider, currentModel, explicitProvider, explicitModel, buildAgentFor)
	if err != nil {
		return sessionResumeCandidate{}, joinSessionCloseError(err, sess)
	}
	if !candidate.rebuilt {
		candidate.agent.SetMessages(candidate.messages)
		candidate.agent.SeedCost(candidate.cumulative)
		candidate.agent.SeedLastTurnUsage(candidate.lastTurn)
	}
	if closeErr := sess.Close(); closeErr != nil {
		return sessionResumeCandidate{}, errors.Join(fmt.Errorf("close session: %w", closeErr), joinSessionCloseError(nil, candidate.session))
	}
	return candidate, nil
}

// applyInitialSessionResume applies the same provider/model-aware candidate
// contract used by interactive session switching to non-interactive startup
// modes. The returned session owns the replacement append handle.
func applyInitialSessionResume(ctx context.Context, args Args, base Resolved, extMgr *extensions.Manager, sess *core.Session, ag *core.Agent) (*core.Session, *core.Agent, string, string, error) {
	return applyInitialSessionResumeWithRegistry(ctx, args, base, extMgr, sess, ag, nil)
}

func applyInitialSessionResumeWithRegistry(ctx context.Context, args Args, base Resolved, extMgr *extensions.Manager, sess *core.Session, ag *core.Agent, prepareRegistry func(core.Registry) core.Registry) (*core.Session, *core.Agent, string, string, error) {
	candidate, err := applySessionResume(sess, ag, base.Provider, base.Model,
		strings.TrimSpace(args.Provider) != "", strings.TrimSpace(args.Model) != "", func(providerName, model string) (*core.Agent, string, string, error) {
			return buildNonInteractiveSessionAgentWithRegistry(ctx, args, base, extMgr, providerName, model, prepareRegistry)
		})
	if err != nil {
		return nil, ag, "", "", err
	}
	return candidate.session, candidate.agent, candidate.provider, candidate.model, nil
}

func applyInitialSessionResumeWithRuntime(ctx context.Context, args Args, base Resolved, extMgr *extensions.Manager, sess *core.Session, ag *core.Agent, runtime *subagentRuntime) (*core.Session, *core.Agent, string, string, error) {
	candidate, err := applySessionResume(sess, ag, base.Provider, base.Model,
		strings.TrimSpace(args.Provider) != "", strings.TrimSpace(args.Model) != "", func(providerName, model string) (*core.Agent, string, string, error) {
			next := args
			next.Provider = providerName
			next.Model = model
			resolved, resolveErr := Resolve(next, true)
			if resolveErr != nil {
				return nil, "", "", resolveErr
			}
			resolved.UseSandbox(base.Sandbox)
			if extMgr != nil {
				resolved.MergeExtensionTools(&extToolAdapter{mgr: extMgr})
			}
			if runtime != nil {
				runtime.SetProvider(resolved.Provider)
				runtime.SetModel(resolved.Model)
				runtime.SetProviderSettings(resolved.BaseURL, resolved.InsecureTLS)
				runtime.SetFastMode(resolved.FastMode)
				runtime.PrepareResolvedRegistry(resolved.ToolRegistry, resolved.WebSearchPolicy)
			}
			rebuilt := resolved.NewAgent()
			wireNonInteractiveAgentExtHooks(ctx, rebuilt, extMgr)
			return rebuilt, resolved.Provider, resolved.Model, nil
		})
	if err != nil {
		return nil, ag, "", "", err
	}
	return candidate.session, candidate.agent, candidate.provider, candidate.model, nil
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

func persistExtensionToolResult(mgr *extensions.Manager, sess *core.Session, result core.ToolResult) {
	if sess == nil {
		return
	}
	details, ok := result.Details.(extensions.ToolResultDetails)
	if !ok || details.Extension == "" || len(details.State) == 0 {
		return
	}
	if err := sess.AppendExtensionState(details.Extension, details.State); err != nil {
		fmt.Fprintf(os.Stderr, "extension state: %v\n", err)
		return
	}
	if mgr != nil {
		mgr.UpdateSessionState(details.Extension, details.State)
	}
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
	// Extension installs can invoke external build and clone processes.
	// Give those processes a cancellation context without changing signal
	// handling for the interactive modes below.
	extCtx := context.Background()
	stopExtContext := func() {}
	if len(rawArgs) > 0 && rawArgs[0] == "ext" {
		extCtx, stopExtContext = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	}
	defer stopExtContext()

	// Subcommand router: `zut bot ...` is handled separately so the
	// generic flag parser doesn't reject "bot" as a positional arg.
	if handled, err := runBotCommand(rawArgs, version); handled {
		return err
	}
	if handled, err := runExtCommand(extCtx, rawArgs, version); handled {
		return err
	}
	if handled, err := runUpdateCommand(rawArgs, version); handled {
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
	prepareRuntimeCatalog()

	if args.ListModels {
		printModels()
		return nil
	}

	return runWithArgs(args, version)
}

func runWithArgs(args Args, version string) error {
	if err := validateOrchestrationArgs(args); err != nil {
		return err
	}
	ctx := context.Background()
	switch args.Mode {
	case ModePrint:
		return runPrintMode(ctx, args, version)
	case ModeStream:
		return runStreamMode(ctx, args, version)
	case ModeJSON:
		return runJSONMode(ctx, args, version)
	case ModeRPC:
		return runRPCMode(ctx, args, version)
	case ModeSubagentWorker:
		return runSubagentWorkerMode(ctx, args, version)
	default:
		return runInteractive(ctx, args, version)
	}
}

// ---- print / json modes: require credentials, run single-shot ----

// nonInteractiveExtHooks is the HostHooks impl used by print / json
// modes. They have no TUI, so notify / display go to stderr and
// submit / insert are no-ops (the extension can't steer a
// single-shot run once it's in flight anyway).
type nonInteractiveExtHooks struct{}

func (nonInteractiveExtHooks) Notify(ext, level, message string) {
	fmt.Fprintf(os.Stderr, "[%s] %s: %s\n", ext, level, message)
}
func (nonInteractiveExtHooks) Alert(string, extproto.AlertRequest)                  {}
func (nonInteractiveExtHooks) Submit(string)                                        {}
func (nonInteractiveExtHooks) SubmitSlash(string)                                   {}
func (nonInteractiveExtHooks) Insert(string)                                        {}
func (nonInteractiveExtHooks) Display(string, string)                               {}
func (nonInteractiveExtHooks) ClearNotes(string)                                    {}
func (nonInteractiveExtHooks) OpenPanel(string, extproto.PanelSpec)                 {}
func (nonInteractiveExtHooks) UpdatePanel(string, string, string, []string, string) {}
func (nonInteractiveExtHooks) ClosePanel(string, string)                            {}
func (nonInteractiveExtHooks) SetStatus(string, string, string, string)             {}
func (nonInteractiveExtHooks) SetWidget(string, string, string, string, []string)   {}
func (nonInteractiveExtHooks) ClearWidget(string, string)                           {}

// setupNonInteractiveExtensions loads --ext paths and (unless
// --no-ext) runs discovery. Returns the manager so the caller can
// wire tools into the resolved registry, and a cleanup closure to
// defer. Mirrors the interactive-mode setup minus the TUI hooks.
func setupNonInteractiveExtensions(ctx context.Context, args Args, r *Resolved, version string) (*extensions.Manager, func()) {
	extMgr := extensions.New(ZutHome(), r.CWD, version, r.Provider, r.Model, nonInteractiveExtHooks{})
	for _, e := range extMgr.LoadExplicit(ctx, args.Exts) {
		fmt.Fprintln(os.Stderr, "extension load:", e)
	}
	if !args.NoExt {
		for _, e := range extMgr.Discover(ctx) {
			fmt.Fprintln(os.Stderr, "extension load:", e)
		}
	}
	extMgr.WaitForReady(3 * time.Second)
	r.MergeExtensionTools(&extToolAdapter{mgr: extMgr})
	return extMgr, func() { extMgr.Stop(2 * time.Second) }
}

// wireNonInteractiveAgentExtHooks installs the same BeforeToolExecute
// / BeforeTurn / BeforeAssistantMessage / OnEvent hooks the
// interactive path wires up, so extensions get their normal
// event-intercept surface in print / json / rpc flows too.
func wireNonInteractiveAgentExtHooks(ctx context.Context, ag *core.Agent, extMgr *extensions.Manager) {
	if ag == nil || extMgr == nil {
		return
	}
	ag.BeforeToolExecute = func(call provider.ToolCallBlock) (bool, string, json.RawMessage) {
		res := extMgr.InterceptToolCall(ctx, call.ID, call.Name, call.Arguments)
		if res.Block {
			return false, res.Reason, nil
		}
		return true, "", res.ModifiedArgs
	}
	ag.BeforeTurnContext = func(turnCtx context.Context, step int) (bool, string, string) {
		res := extMgr.InterceptTurnStart(turnCtx, step)
		return !res.Block, res.Reason, res.Context
	}
	ag.BeforeAssistantMessage = func(text string) (bool, string, string) {
		res := extMgr.InterceptAssistantMessage(ctx, text)
		if res.Block {
			return false, res.Reason, ""
		}
		return true, "", res.ReplaceText
	}
	ag.OnEvent = func(ev core.AgentEvent) { fanoutAgentEvent(extMgr, ev) }
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

func runPrintMode(ctx context.Context, args Args, version string) (runErr error) {
	if args.Orchestrate {
		return runOrchestratedPrintMode(ctx, args, version)
	}
	if args.NoYolo {
		fmt.Fprintln(os.Stderr, "warning: --no-yolo has no effect in print mode (no interactive prompt available); tools will run without confirmation")
	}
	r, err := Resolve(args, true)
	if err != nil {
		return err
	}
	extMgr, stopExt := setupNonInteractiveExtensions(ctx, args, &r, version)
	defer stopExt()

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
		var providerName, model string
		sess, ag, providerName, model, err = applyInitialSessionResume(ctx, args, r, extMgr, sess, ag)
		if err != nil {
			return err
		}
		r.Provider, r.Model = providerName, model
		ag.OnToolResult = func(_ string, result core.ToolResult) { persistExtensionToolResult(extMgr, sess, result) }
		defer func() { runErr = joinSessionCloseError(runErr, sess) }()
	}
	announceSession(extMgr, sess)

	prompt := args.Prompt
	if prompt == "" {
		piped, _ := readAllStdin()
		prompt = strings.TrimSpace(piped)
	}
	if prompt == "" {
		return fmt.Errorf("print mode requires a prompt (arg or stdin)")
	}

	start := len(ag.Messages())
	if err := runZutfileStartupPre(ctx, args.StartupPre, r.CWD, r.Sandbox, ag, nil, os.Stderr); err != nil {
		return err
	}
	if err := reloadResourcesAfterStartupPre(ctx, args, extMgr, r.Sandbox, ag); err != nil {
		return err
	}
	var persistCompaction func([]provider.Message) error
	if sess != nil {
		persistCompaction = sess.AppendCompaction
	}
	started := time.Now()
	usage, recovery, err := modes.RunPrintWithContextRecovery(ctx, ag, prompt, nil, os.Stdout, persistCompaction)
	elapsed := time.Since(started)
	transcriptStart := start
	if recovery.Compacted {
		transcriptStart = recovery.OutputStart
	}
	if persistErr := WriteNewTranscript(ag, sess, transcriptStart); persistErr != nil {
		if err != nil {
			return errors.Join(err, persistErr)
		}
		return persistErr
	}
	if err != nil {
		return err
	}
	if args.StatsPath != "" {
		return writePrintStats(args.StatsPath, r.Provider, r.Model, usage, elapsed)
	}
	return nil
}

func runStreamMode(ctx context.Context, args Args, version string) (runErr error) {
	if args.Orchestrate {
		return runOrchestratedStreamMode(ctx, args, version)
	}
	if args.NoYolo {
		fmt.Fprintln(os.Stderr, "warning: --no-yolo has no effect in stream mode (no interactive prompt available); tools will run without confirmation")
	}
	r, err := Resolve(args, true)
	if err != nil {
		return err
	}
	extMgr, stopExt := setupNonInteractiveExtensions(ctx, args, &r, version)
	defer stopExt()

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
		var providerName, model string
		sess, ag, providerName, model, err = applyInitialSessionResume(ctx, args, r, extMgr, sess, ag)
		if err != nil {
			return err
		}
		r.Provider, r.Model = providerName, model
		ag.OnToolResult = func(_ string, result core.ToolResult) { persistExtensionToolResult(extMgr, sess, result) }
		defer func() { runErr = joinSessionCloseError(runErr, sess) }()
	}
	announceSession(extMgr, sess)

	prompt := args.Prompt
	if prompt == "" {
		piped, _ := readAllStdin()
		prompt = strings.TrimSpace(piped)
	}
	if prompt == "" {
		return fmt.Errorf("stream mode requires a prompt (arg or stdin)")
	}

	start := len(ag.Messages())
	preSink, finishPre := newStreamTextSink(os.Stdout)
	if err := runZutfileStartupPre(ctx, args.StartupPre, r.CWD, r.Sandbox, ag, preSink, os.Stderr); err != nil {
		finishPre()
		return err
	}
	finishPre()
	if err := reloadResourcesAfterStartupPre(ctx, args, extMgr, r.Sandbox, ag); err != nil {
		return err
	}
	var persistCompaction func([]provider.Message) error
	if sess != nil {
		persistCompaction = sess.AppendCompaction
	}
	recovery, err := modes.RunStreamWithContextRecovery(ctx, ag, prompt, nil, os.Stdout, os.Stderr, persistCompaction)
	transcriptStart := start
	if recovery.Compacted {
		transcriptStart = recovery.OutputStart
	}
	if persistErr := WriteNewTranscript(ag, sess, transcriptStart); persistErr != nil {
		if err != nil {
			return errors.Join(err, persistErr)
		}
		return persistErr
	}
	return err
}

func newStreamTextSink(out io.Writer) (func(core.AgentEvent), func()) {
	var streamed, wroteText, lastWasNL bool
	writeText := func(text string) {
		if text == "" {
			return
		}
		_, _ = fmt.Fprint(out, text)
		if syncer, ok := out.(interface{ Sync() error }); ok {
			_ = syncer.Sync()
		}
		wroteText = true
		lastWasNL = strings.HasSuffix(text, "\n")
	}
	sink := func(ev core.AgentEvent) {
		switch e := ev.(type) {
		case core.EvAssistantStart:
			streamed = false
		case core.EvTextDelta:
			streamed = true
			writeText(e.Delta)
		case core.EvAssistantMessage:
			if streamed {
				return
			}
			var text strings.Builder
			for _, content := range e.Message.Content {
				if block, ok := content.(provider.TextBlock); ok {
					if text.Len() > 0 {
						text.WriteString("\n")
					}
					text.WriteString(block.Text)
				}
			}
			writeText(text.String())
		}
	}
	finish := func() {
		if wroteText && !lastWasNL {
			writeText("\n")
		}
	}
	return sink, finish
}

func runJSONMode(ctx context.Context, args Args, version string) (runErr error) {
	if args.Orchestrate {
		return runOrchestratedJSONMode(ctx, args, version)
	}
	if args.NoYolo {
		fmt.Fprintln(os.Stderr, "warning: --no-yolo has no effect in json mode (no interactive prompt available); tools will run without confirmation")
	}
	r, err := Resolve(args, true)
	if err != nil {
		return err
	}
	extMgr, stopExt := setupNonInteractiveExtensions(ctx, args, &r, version)
	defer stopExt()

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
		var providerName, model string
		sess, ag, providerName, model, err = applyInitialSessionResume(ctx, args, r, extMgr, sess, ag)
		if err != nil {
			return err
		}
		r.Provider, r.Model = providerName, model
		ag.OnToolResult = func(_ string, result core.ToolResult) { persistExtensionToolResult(extMgr, sess, result) }
		defer func() { runErr = joinSessionCloseError(runErr, sess) }()
	}
	announceSession(extMgr, sess)

	prompt := args.Prompt
	if prompt == "" {
		piped, _ := readAllStdin()
		prompt = strings.TrimSpace(piped)
	}
	if prompt == "" {
		return fmt.Errorf("json mode requires a prompt (arg or stdin)")
	}

	start := len(ag.Messages())
	enc := json.NewEncoder(os.Stdout)
	preSink := func(ev core.AgentEvent) {
		_ = enc.Encode(modes.EventToJSON(ev))
	}
	if err := runZutfileStartupPre(ctx, args.StartupPre, r.CWD, r.Sandbox, ag, preSink, os.Stderr); err != nil {
		_ = enc.Encode(map[string]any{"type": "error", "message": err.Error()})
		return err
	}
	if err := reloadResourcesAfterStartupPre(ctx, args, extMgr, r.Sandbox, ag); err != nil {
		_ = enc.Encode(map[string]any{"type": "error", "message": err.Error()})
		return err
	}
	var persistCompaction func([]provider.Message) error
	if sess != nil {
		persistCompaction = sess.AppendCompaction
	}
	recovery, err := modes.RunJSONWithContextRecovery(ctx, ag, prompt, nil, os.Stdout, persistCompaction)
	transcriptStart := start
	if recovery.Compacted {
		transcriptStart = recovery.OutputStart
	}
	if persistErr := WriteNewTranscript(ag, sess, transcriptStart); persistErr != nil {
		finalErr := persistErr
		if err != nil {
			finalErr = errors.Join(err, persistErr)
		}
		_ = enc.Encode(map[string]any{"type": "error", "message": finalErr.Error()})
		return finalErr
	}
	return err
}

// runZutfileStartupPre runs entry.pre before the main non-interactive prompt.
// "!command" values execute via BashTool; other values are sent as an agent turn.
// shellOut receives live shell chunks (typically os.Stderr); sink receives
// agent events for plain-text pre (stream mode wires a live text sink).
func runZutfileStartupPre(ctx context.Context, pre, cwd string, sandbox *tools.Sandbox, ag *core.Agent, sink func(core.AgentEvent), shellOut io.Writer) error {
	pre = strings.TrimSpace(pre)
	if pre == "" {
		return nil
	}
	if cmd, ok := modes.ShellEscapeCommand(pre); ok {
		raw, err := json.Marshal(map[string]any{"command": cmd})
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
		bash := &tools.BashTool{CWD: cwd, Sandbox: sandbox}
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
// mutateRegistry, if non-nil, can inject session-specific tools (e.g. subagent_spawn).
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
		delete(reg, "web_search")
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
	if policy == subagents.WebSearchAllow {
		if _, ok := reg["web_search"]; ok {
			return subagents.WebSearchAllow
		}
	}
	return subagents.WebSearchDeny
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

func webSearchExplicitlyEnabledForSession(args Args) bool {
	return !args.NoTools && args.PermissionSet == nil && (args.ToolsSet || len(args.Tools) > 0) && toolListContains(args.Tools, "web_search")
}

// reloadResourcesAfterStartupPre reloads extensions (if any) and
// refreshes the agent tool registry + system prompt so skills/extensions
// installed by entry.pre are visible to the following turn.
func reloadResourcesAfterStartupPre(ctx context.Context, args Args, extMgr *extensions.Manager, sharedSandbox *tools.Sandbox, ag *core.Agent) error {
	_, err := reloadResourcesAfterStartupPreWithRegistry(ctx, args, extMgr, sharedSandbox, ag, nil)
	return err
}

func reloadResourcesAfterStartupPreWithRegistry(ctx context.Context, args Args, extMgr *extensions.Manager, sharedSandbox *tools.Sandbox, ag *core.Agent, mutateRegistry func(core.Registry) core.Registry) (subagents.WebSearchPolicy, error) {
	if strings.TrimSpace(args.StartupPre) == "" || ag == nil {
		return subagents.WebSearchDeny, nil
	}
	adapter := &extToolAdapter{mgr: extMgr}
	if extMgr != nil {
		_ = extMgr.Reload(ctx, 2*time.Second)
	}
	return refreshAgentToolsAndPrompt(args, sharedSandbox, adapter, ag, mutateRegistry, nil)
}

// ---- interactive mode: opens the TUI even without credentials ----

func runInteractive(ctx context.Context, args Args, version string) (runErr error) {
	initialCfg, _ := LoadConfig()
	// Resolve WITHOUT requiring credentials.
	r, err := Resolve(args, false)
	if err != nil {
		return err
	}

	authStore := AuthStoreFor()
	mgr := auth.NewManager(authStore)
	defer mgr.Close()

	// Keep the sandbox pointer stable across agent rebuilds (login / model
	// switch). The Interactive UI toggles the lock via this pointer, and
	// rebuilt tool instances must share the same one so the lock sticks.
	sharedSandbox := r.Sandbox

	// persistMu guards the active session and selection across the agent
	// loop, TUI callbacks, and session swaps. Declare it before tool
	// construction because subagent defaults must read the live selection too.
	var persistMu sync.Mutex
	// Serialize session transitions with persistence callbacks so a candidate
	// is read only after the old agent is flushed and cannot be overwritten by
	// an in-flight append before commit.
	var sessionTransitionMu sync.RWMutex
	activeProvider := r.Provider
	activeModel := r.Model

	// Build the extension manager BEFORE the agent so we can fold
	// extension-defined tools into the registry. Attach the interactive
	// host after constructing it below so startup alerts can be buffered.
	var iv *modes.Interactive
	extHooks := &interactiveExtHooks{}
	extMgr := extensions.New(ZutHome(), r.CWD, version, r.Provider, r.Model, extHooks)
	var startupExtensionErrors []string
	// --ext paths first so they win against installed extensions of
	// the same name (loadOne's first-write-wins semantics).
	for _, e := range extMgr.LoadExplicit(ctx, args.Exts) {
		startupExtensionErrors = append(startupExtensionErrors, e.Error())
	}
	// --no-ext skips the global + project-local discovery scan;
	// explicit --ext paths above are still honoured so you can run
	// "only this extension" with --no-ext --ext ./x.
	if !args.NoExt {
		for _, e := range extMgr.Discover(ctx) {
			startupExtensionErrors = append(startupExtensionErrors, e.Error())
		}
	}
	// Wait briefly for extensions to flush their initial register_tool
	// frames before we build the agent's tool registry. Half a second
	// is plenty for any extension that's actually well-behaved; ones
	// that don't send a ready frame eat the full grace and proceed.
	// 3s is the per-extension grace period for the ready frame.
	// Native binaries are instant; runtimes like `npx tsx` take ~1.5s
	// from cold cache. The wait is tight only for extensions that
	// haven't sent ready by then; ones that signalled earlier release
	// the wait immediately.
	extMgr.WaitForReady(3 * time.Second)
	defer extMgr.Stop(2 * time.Second)

	extToolAdapter := &extToolAdapter{mgr: extMgr}
	r.MergeExtensionTools(extToolAdapter)
	webSearchGuard := &webSearchSessionGuard{}

	// Keep the shared subagent runtime in the outer scope: session reloads,
	// dashboard updates, agent rebuilds, and shutdown all use the same owner.
	var runtime *subagentRuntime
	var subagentsMgr *subagents.Supervisor
	// These callbacks deliberately resolve iv lazily because the Interactive
	// instance is constructed after the agent and its registry.
	onSpawnedSupervisor := func(a *subagents.Agent, task string) {
		if iv != nil {
			iv.TrackSubagentWorker(a, task)
		}
	}
	onResumedSupervisor := func(a *subagents.Agent, prompt string) {
		if iv != nil {
			iv.TrackResumedSubagentWorker(a, prompt)
		}
	}
	onBeforeResumedSupervisor := func(a *subagents.Agent, prompt string) func() {
		if iv != nil {
			return iv.PrepareResumedSubagentWorker(a, prompt)
		}
		return nil
	}
	onStopRequestedSupervisor := func(a *subagents.Agent) {
		if iv != nil {
			iv.TrackStoppedSubagentWorker(a)
		}
	}
	activeProviderForSubagents := func() string {
		persistMu.Lock()
		defer persistMu.Unlock()
		return activeProvider
	}
	activeModelForSubagents := func() string {
		persistMu.Lock()
		defer persistMu.Unlock()
		return activeModel
	}

	runtime = newSubagentRuntime(subagentRuntimeConfig{
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
		Policy:          subagentPolicyFromConfig(initialCfg.Subagents),
		WebSearchPolicy: webSearchPolicyForRegistry(r.WebSearchPolicy, r.ToolRegistry),
		WebSearchGuard:  webSearchGuard,
		ActiveProvider:  activeProviderForSubagents,
		ActiveModel:     activeModelForSubagents,
		OnSpawned:       onSpawnedSupervisor,
		OnResumed:       onResumedSupervisor,
		BeforeResumed:   onBeforeResumedSupervisor,
		OnStopRequested: onStopRequestedSupervisor,
	})
	subagentsMgr = runtime.Supervisor()
	reloadDone := make(chan struct{})
	var reloadErrs []error
	if subagentsMgr != nil {
		// Replaying historical event logs can take seconds. Populate the
		// detached-agent dashboard without blocking the first interactive paint.
		go func() {
			_, reloadErrs = subagentsMgr.Reload()
			close(reloadDone)
		}()
	} else {
		close(reloadDone)
	}
	defer func() {
		// Reload may attach detached workers, so let it finish before shutdown
		// snapshots the supervisor's worker set.
		<-reloadDone
		_ = closeSubagentRuntimeFresh(runtime)
	}()

	prepareInteractiveRegistry := runtime.PrepareRegistry
	prepareResolvedInteractiveRegistry := runtime.PrepareResolvedRegistry
	prepareResolvedInteractiveRegistry(r.ToolRegistry, r.WebSearchPolicy)
	initialWebSearchPolicy := webSearchPolicyForRegistry(r.WebSearchPolicy, r.ToolRegistry)
	webSearchGuard.setAvailable(initialWebSearchPolicy == subagents.WebSearchAllow)
	runtime.SetWebSearchPolicy(initialWebSearchPolicy)
	resolveSubagent := runtime.resolveSubagent

	// Confirmation gate: when --no-yolo is on, the agent must ask
	// the user before every tool call. In interactive mode the TUI
	// provides the Confirmer; in print/json/rpc modes there's no
	// way to prompt, so the gate is constructed with a nil inner
	// which auto-refuses every call with a helpful reason.
	var confirmGate *core.ConfirmGate
	if args.NoYolo {
		confirmGate = core.NewConfirmGate(nil) // set below for interactive
	}

	// Capture current args in a closure so BuildAgent can re-resolve
	// after a successful login (picks up the newly stored credential).
	wireAgentExt := func(a *core.Agent) *core.Agent {
		if a == nil {
			return a
		}
		a.BeforeToolExecute = func(call provider.ToolCallBlock) (bool, string, json.RawMessage) {
			// Guards run before confirmation because they may rewrite the
			// arguments. The user must approve the effective call that will
			// actually execute, not the model's original arguments.
			r := extMgr.InterceptToolCall(ctx, call.ID, call.Name, call.Arguments)
			if r.Block {
				return false, r.Reason, nil
			}
			effectiveArgs := call.Arguments
			if len(r.ModifiedArgs) > 0 && json.Valid(r.ModifiedArgs) {
				effectiveArgs = r.ModifiedArgs
			}
			if confirmGate != nil {
				var content strings.Builder
				_, currentTools := a.PromptConfig()
				if tool, err := currentTools.Get(call.Name); err == nil {
					if previewer, ok := tool.(core.ToolPreviewer); ok {
						preview, err := previewer.Preview(ctx, effectiveArgs)
						if err != nil {
							return false, err.Error(), nil
						}
						for _, block := range preview.Content {
							if text, ok := block.(provider.TextBlock); ok {
								if content.Len() > 0 {
									content.WriteString("\n")
								}
								content.WriteString(text.Text)
							}
						}
					}
				}
				ok, reason, _ := confirmGate.CheckToolCall(core.ToolCallConfirmation{
					ID:      call.ID,
					Name:    call.Name,
					Summary: core.BuildPreview(effectiveArgs, 120),
					Content: content.String(),
					Origin:  call.Origin,
				})
				if !ok {
					return false, reason, nil
				}
			}
			return true, "", r.ModifiedArgs
		}
		a.BeforeTurnContext = func(turnCtx context.Context, step int) (bool, string, string) {
			r := extMgr.InterceptTurnStart(turnCtx, step)
			return !r.Block, r.Reason, r.Context
		}
		a.BeforeAssistantMessage = func(text string) (bool, string, string) {
			r := extMgr.InterceptAssistantMessage(ctx, text)
			if r.Block {
				return false, r.Reason, ""
			}
			return true, "", r.ReplaceText
		}
		a.OnEvent = func(ev core.AgentEvent) { fanoutAgentEvent(extMgr, ev) }
		return a
	}

	var ag *core.Agent
	buildAgent := func() (*core.Agent, string, string, error) {
		resolved, err := Resolve(args, true)
		if err != nil {
			return nil, "", "", err
		}
		resolved.UseSandbox(sharedSandbox)
		runtime.SetProvider(resolved.Provider)
		runtime.SetProviderSettings(resolved.BaseURL, resolved.InsecureTLS)
		runtime.SetFastMode(resolved.FastMode)
		resolved.MergeExtensionTools(extToolAdapter)
		prepareResolvedInteractiveRegistry(resolved.ToolRegistry, resolved.WebSearchPolicy)
		return wireAgentExt(resolved.NewAgent()), resolved.Provider, resolved.Model, nil
	}

	// Rebuild agent with an explicit provider/model override.
	buildAgentFor := func(providerOverride, modelOverride string) (*core.Agent, string, string, error) {
		next := args
		if providerOverride != "" {
			next.Provider = providerOverride
		}
		if modelOverride != "" {
			next.Model = modelOverride
		}
		resolved, err := Resolve(next, true)
		if err != nil {
			return nil, "", "", err
		}
		resolved.UseSandbox(sharedSandbox)
		runtime.SetProvider(resolved.Provider)
		runtime.SetProviderSettings(resolved.BaseURL, resolved.InsecureTLS)
		runtime.SetFastMode(resolved.FastMode)
		resolved.MergeExtensionTools(extToolAdapter)
		prepareResolvedInteractiveRegistry(resolved.ToolRegistry, resolved.WebSearchPolicy)
		return wireAgentExt(resolved.NewAgent()), resolved.Provider, resolved.Model, nil
	}

	// Rebuild agent for the rescue picker after a recoverable failure.
	// Unlike buildAgentFor, this drops launch-time --api-key and
	// --base-url overrides because those are typically the cause of the
	// rescue (a bad key, a typo'd base URL, or a corporate gateway that
	// only the originally-picked provider needed). Re-resolving without
	// them lets the rescue retry use env vars / auth.json / provider
	// defaults the way zut would have without the overrides.
	buildAgentForRescue := func(providerOverride, modelOverride string) (*core.Agent, string, string, error) {
		next := args
		next.APIKey = ""
		next.BaseURL = ""
		if providerOverride != "" {
			next.Provider = providerOverride
		}
		if modelOverride != "" {
			next.Model = modelOverride
		}
		resolved, err := Resolve(next, true)
		if err != nil {
			return nil, "", "", err
		}
		resolved.UseSandbox(sharedSandbox)
		runtime.SetProvider(resolved.Provider)
		runtime.SetProviderSettings(resolved.BaseURL, resolved.InsecureTLS)
		runtime.SetFastMode(resolved.FastMode)
		resolved.MergeExtensionTools(extToolAdapter)
		prepareResolvedInteractiveRegistry(resolved.ToolRegistry, resolved.WebSearchPolicy)
		return wireAgentExt(resolved.NewAgent()), resolved.Provider, resolved.Model, nil
	}

	if r.HasCredential() {
		ag = wireAgentExt(r.NewAgent())
	}

	// /reload-ext callback: after the manager has respawned every
	// extension, re-resolve the tool registry (built-ins + freshly-
	// registered extension tools) and swap it onto the current
	// agent in-place. The current agent may have been replaced by a
	// /model swap since spawn, so re-read the live `ag` on each
	// invocation.
	extMgr.SetOnReload(func() {
		current := liveInteractiveAgent(iv, ag)
		if current == nil {
			return
		}
		if _, err := refreshAgentToolsAndPrompt(args, sharedSandbox, extToolAdapter, current, prepareInteractiveRegistry, iv); err != nil && iv != nil {
			iv.ReportError(err)
		}
	})

	var sess *core.Session
	var sessBaselineMsgs int // messages already on disk when current session opened
	var sessionTitlePending bool
	if !args.NoSess && ag != nil {
		var sessErr error
		sess, sessErr = openOrCreateSession(ctx, args, r, ag, version)
		if sessErr != nil {
			return sessErr
		}
		sessBaselineMsgs = len(ag.Messages())
		if sess != nil {
			// Startup resume uses the same provider/model-aware preparation as
			// the interactive session picker. This matters for --continue,
			// --resume, and explicit --session paths whose latest meta row was
			// written by another provider/model.
			candidate, resumeErr := applySessionResume(sess, ag, r.Provider, r.Model,
				strings.TrimSpace(args.Provider) != "", strings.TrimSpace(args.Model) != "", buildAgentFor)
			if resumeErr != nil {
				return resumeErr
			}
			sess = candidate.session
			ag = candidate.agent
			sessBaselineMsgs = candidate.fullMessageCount
			activeProvider = candidate.provider
			activeModel = candidate.model
			r.Provider = candidate.provider
			r.Model = candidate.model
		}
		if sess != nil && ag != nil {
			sessionTitlePending = sess.Meta.Parent != "" && sess.Title == "" && len(ag.Messages()) <= sess.Meta.ForkPoint
		}
	}
	announceSession(extMgr, sess)
	// Print the hint after the session close defer below and all terminal/TUI
	// cleanup defers. Close removes a fresh empty session, so only a session
	// that remains discoverable through the hinted command gets a hint.
	defer func() {
		if !iv.ExitedViaCtrlC() {
			return
		}
		persistMu.Lock()
		current := sess
		persistMu.Unlock()
		if resumeID := resumableSessionID(ZutHome(), current); resumeID != "" {
			fmt.Print(resumeSessionHint(resumeID))
		}
	}()
	defer func() {
		persistMu.Lock()
		defer persistMu.Unlock()
		runErr = joinSessionCloseError(runErr, sess)
	}()

	// persistMessage is the per-message hook bound to the agent. It
	// appends each new transcript message to the live session as soon
	// as it lands, so a kill / closed terminal / OS crash costs at
	// most the in-flight turn instead of the whole session. The
	// baseline counter advances in lock-step so the exit-time flush
	// doesn't double-write rows already on disk.
	persistMessage := func(m provider.Message) {
		sessionTransitionMu.RLock()
		defer sessionTransitionMu.RUnlock()
		persistMu.Lock()
		defer persistMu.Unlock()
		if sess == nil {
			return
		}
		if err := sess.AppendMessage(m); err == nil {
			sessBaselineMsgs++
		}
	}
	persistUsage := func(cum provider.Usage) {
		sessionTransitionMu.RLock()
		defer sessionTransitionMu.RUnlock()
		persistMu.Lock()
		defer persistMu.Unlock()
		if sess == nil {
			return
		}
		_ = sess.AppendUsage(cum, cum)
	}
	persistCompaction := func(messages []provider.Message) {
		sessionTransitionMu.RLock()
		var session *extproto.SessionContext
		var states map[string]json.RawMessage
		persistMu.Lock()
		if sess != nil {
			if err := sess.AppendCompaction(messages); err == nil {
				sessBaselineMsgs = len(messages)
				session = sessionContext(sess)
				states = copyExtensionStates(sess.ExtensionState)
			}
		}
		persistMu.Unlock()
		sessionTransitionMu.RUnlock()
		if session != nil {
			extMgr.EmitSessionEvent("session_compacted", session, states)
		}
	}
	persistCompactHandoff := func(state json.RawMessage) error {
		sessionTransitionMu.RLock()
		defer sessionTransitionMu.RUnlock()
		persistMu.Lock()
		defer persistMu.Unlock()
		if sess == nil {
			return nil
		}
		return sess.UpdateCompactHandoff(state)
	}
	currentCompactHandoff := func() json.RawMessage {
		persistMu.Lock()
		defer persistMu.Unlock()
		if sess == nil {
			return nil
		}
		return append(json.RawMessage(nil), sess.Meta.CompactHandoff...)
	}
	persistToolResult := func(_ string, result core.ToolResult) {
		persistMu.Lock()
		defer persistMu.Unlock()
		persistExtensionToolResult(extMgr, sess, result)
	}
	wireAgentPersist := func(a *core.Agent) *core.Agent {
		if a == nil {
			return a
		}
		a.OnMessageAppended = persistMessage
		a.OnUsage = persistUsage
		a.OnTranscriptCompacted = persistCompaction
		a.OnToolResult = persistToolResult
		return a
	}
	wireAgentPersist(ag)
	defer func() {
		closeAgentLSP(liveInteractiveAgent(iv, ag))
		closeResolvedLSP(r)
	}()

	// Re-wrap the build closures so any agent constructed by the TUI
	// (login, /model swap to a different provider) also gets the
	// persistence hooks. Without this, switching provider would
	// silently revert to the old in-memory-only behaviour. Selection state
	// is published by the successful transition itself, not by preparation.
	baseBuildAgent := buildAgent
	buildAgent = func() (*core.Agent, string, string, error) {
		a, p, m, err := baseBuildAgent()
		return wireAgentPersist(a), p, m, err
	}
	baseBuildAgentFor := buildAgentFor
	buildAgentFor = func(providerOverride, modelOverride string) (*core.Agent, string, string, error) {
		a, p, m, err := baseBuildAgentFor(providerOverride, modelOverride)
		return wireAgentPersist(a), p, m, err
	}
	baseBuildAgentForRescue := buildAgentForRescue
	buildAgentForRescue = func(providerOverride, modelOverride string) (*core.Agent, string, string, error) {
		a, p, m, err := baseBuildAgentForRescue(providerOverride, modelOverride)
		return wireAgentPersist(a), p, m, err
	}

	// loadSession replaces the current session with the one at path and
	// hands its messages to the agent. Used by the /sessions picker.
	loadSession := func(path string) (loadErr error) {
		// Hold the transition lock from the pre-flush through the commit.
		// Persistence callbacks take the read side, so an active session
		// cannot be snapshotted before its lazy writes land or overwritten by
		// a callback while the candidate is being installed.
		sessionTransitionMu.Lock()
		defer sessionTransitionMu.Unlock()

		currentAg := liveInteractiveAgent(iv, ag)
		if currentAg == nil {
			return fmt.Errorf("no agent running; log in first")
		}

		persistMu.Lock()
		currentProvider := activeProvider
		currentModel := activeModel
		oldSess := sess
		// Flush before reading the candidate. Use the locked form rather
		// than the public callback, which would acquire the read lock held
		// by this transition.
		if oldSess != nil {
			next, flushErr := writeNewTranscriptLocked(currentAg, oldSess, sessBaselineMsgs)
			sessBaselineMsgs = next
			if flushErr != nil {
				persistMu.Unlock()
				return fmt.Errorf("flush current session: %w", flushErr)
			}
		}
		currentMessageCount := len(currentAg.Messages())
		currentCost := currentAg.Cost()
		persistMu.Unlock()

		// A candidate build must not publish its provider/model as the live
		// selection before the candidate commits. The builder also stages
		// provider settings on the shared runtime, so restore those settings on
		// every failed path, including errors returned during preparation.
		runtimeConfig := runtime.snapshotConfiguration()
		committed := false
		var candidate sessionResumeCandidate
		defer func() {
			if !committed {
				runtime.restoreConfiguration(runtimeConfig)
				loadErr = joinSessionCloseError(loadErr, candidate.session)
			}
		}()
		candidate, err := prepareSessionResume(path, currentAg, currentProvider, currentModel, baseBuildAgentFor)
		if err != nil {
			// prepareSessionResume closes its append handle on failure;
			// no live agent or session has been changed.
			return err
		}

		// Wire the private candidate before it can become visible. This is
		// also required for legacy same-provider resumes, where the current
		// agent is reused and only its transcript/usage are refreshed.
		wireAgentPersist(candidate.agent)

		persistMu.Lock()
		// A provider rebuild ran outside persistMu. Refuse to install it if
		// the live agent, session, selection, transcript, or cost changed in
		// the meantime; otherwise a slow resume could discard a newer turn or
		// model swap.
		live := liveInteractiveAgent(iv, ag)
		changed := sess != oldSess || activeProvider != currentProvider || activeModel != currentModel || live != currentAg
		if !changed && live != nil {
			changed = len(live.Messages()) != currentMessageCount || live.Cost() != currentCost
		}
		if changed {
			persistMu.Unlock()
			return fmt.Errorf("session changed while loading; please try again")
		}
		newStates := copyExtensionStates(candidate.session.ExtensionState)
		var oldCloseErr error
		if oldSess != nil {
			oldCloseErr = oldSess.Close()
		}
		sess = candidate.session
		sessionTitlePending = candidate.session != nil && candidate.session.Meta.Parent != "" && candidate.session.Title == "" && candidate.fullMessageCount <= candidate.session.Meta.ForkPoint
		// The live agent only receives a compact resume window, but the
		// session file remains intact. Keep the persistence baseline at
		// the original on-disk message count so future turns append after
		// the full session instead of duplicating the hydrated tail.
		sessBaselineMsgs = candidate.fullMessageCount

		if candidate.rebuilt {
			// Commit the TUI agent while persistMu still protects the session
			// writer, so no new message can land on the old file between the
			// two state changes.
			if iv != nil {
				iv.ApplySessionAgentWithCompactHandoff(candidate.agent, candidate.provider, candidate.model, candidate.session.Meta.CompactHandoff)
			}
			ag = candidate.agent
		} else {
			currentAg.SetMessages(candidate.messages)
			currentAg.SeedCost(candidate.cumulative)
			currentAg.SeedLastTurnUsage(candidate.lastTurn)
			ag = currentAg
		}
		activeProvider = candidate.provider
		activeModel = candidate.model
		if subagentsMgr != nil && candidate.session != nil {
			// Keep the dashboard scope in the same commit as the session,
			// agent, usage, and persistence baseline.
			if iv != nil {
				iv.SetSubagentSessionScope(candidate.session.ID)
			} else {
				runtime.SetActiveSession(candidate.session.ID)
			}
		}
		committed = true
		persistMu.Unlock()
		if candidate.session.Meta.Parent != "" {
			extMgr.EmitSessionEvent("session_forked", sessionContext(candidate.session), newStates)
		}
		extMgr.EmitSessionEvent("session_switched", sessionContext(candidate.session), newStates)
		if oldCloseErr != nil {
			return fmt.Errorf("close current session: %w", oldCloseErr)
		}
		return nil
	}

	// changeCWD switches the running session to a new working directory.
	// Wired into InteractiveConfig.ChangeCWD and invoked by the hidden
	// /cd slash command (which itself is only fired by the workspaces
	// extension's panel-key Enter handler today; the user can type /cd
	// directly but it's not in autocomplete / help / the README).
	//
	// Steps, in order:
	//   1. resolve + validate the new path (~ expansion, abs/rel)
	//   2. stage the candidate cwd, sandbox, and subagent state
	//   3. rebuild the agent and allocate its new session
	//   4. commit all new state together, closing the old session last
	//   5. push the committed state into the running Interactive
	//
	// The /jail state is preserved verbatim: if the sandbox was locked
	// to the old cwd, it stays locked, just re-pointed at the new one.
	changeCWD := func(path string) error {
		sessionTransitionMu.Lock()
		defer sessionTransitionMu.Unlock()
		if path == "" {
			return fmt.Errorf("empty path")
		}
		// ~ expansion.
		if path == "~" || strings.HasPrefix(path, "~/") {
			home, herr := os.UserHomeDir()
			if herr != nil || home == "" {
				return fmt.Errorf("cannot expand ~: %v", herr)
			}
			if path == "~" {
				path = home
			} else {
				path = filepath.Join(home, path[2:])
			}
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(args.CWD, path)
		}
		absPath, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		info, err := os.Stat(absPath)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("not a directory: %s", absPath)
		}

		currentAg := liveInteractiveAgent(iv, ag)
		if currentAg == nil {
			return fmt.Errorf("no agent running; log in first")
		}

		// Stage the candidate state while retaining a complete rollback
		// snapshot. A failed rebuild or session allocation must leave the
		// current agent, session, sandbox, and subagent root usable.
		oldCWD := args.CWD
		oldResolvedCWD := r.CWD
		oldPermissionSet := args.PermissionSet
		persistMu.Lock()
		oldActiveProvider := activeProvider
		oldActiveModel := activeModel
		persistMu.Unlock()
		runtimeConfig := runtime.snapshotConfiguration()
		oldSandboxRoot := ""
		oldSandboxLocked := false
		if sharedSandbox != nil {
			oldSandboxRoot = sharedSandbox.Root
			oldSandboxLocked = sharedSandbox.Locked()
		}
		rollback := func() {
			args.CWD = oldCWD
			r.CWD = oldResolvedCWD
			args.PermissionSet = oldPermissionSet
			persistMu.Lock()
			activeProvider = oldActiveProvider
			activeModel = oldActiveModel
			persistMu.Unlock()
			runtime.restoreConfiguration(runtimeConfig)
			if sharedSandbox != nil {
				sharedSandbox.Root = oldSandboxRoot
				if oldSandboxLocked {
					sharedSandbox.Lock()
				} else {
					sharedSandbox.Unlock()
				}
			}
		}

		args.CWD = absPath
		r.CWD = absPath
		runtime.SetRepoRoot(absPath)
		if args.PermissionSet != nil {
			expanded := args.PermissionSet.Expand(absPath, args.AgentDataDir)
			args.PermissionSet = &expanded
		}
		if sharedSandbox != nil {
			sharedSandbox.Root = absPath
			if oldSandboxLocked {
				sharedSandbox.Lock()
			} else {
				sharedSandbox.Unlock()
			}
		}

		// Rebuild the agent so tools / AGENTS.md / system prompt
		// re-bind to the new cwd. buildAgent() reads from args + r.
		newAg, newProvider, newModel, berr := buildAgent()
		if berr != nil {
			rollback()
			return fmt.Errorf("rebuild agent: %w", berr)
		}

		// Fresh session in the new cwd's bucket. We bypass
		// openOrCreateSession's --continue / --resume branches
		// because /cd's semantics are "start fresh here", matching
		// what relaunching `zut --cwd <path>` would do today.
		var newSess *core.Session
		if !args.NoSess {
			newRoot := agentSessionsRoot(ZutHome(), args)
			core.PruneEmptySessions(newRoot, absPath)
			var serr error
			newSess, serr = core.NewSession(newRoot, absPath, newProvider, newModel, version)
			if serr != nil {
				rollback()
				return fmt.Errorf("open session in %s: %w", absPath, serr)
			}
		}

		// Invalidate any title request before swapping the host-owned session
		// pointer. The interactive state reset below happens after the swap,
		// so this guard closes the window where a late result could otherwise
		// be written to the new session.
		if iv != nil {
			iv.CancelPendingSessionTitle()
		}

		// Commit only after every fallible operation has succeeded. Close
		// the old session last so rollback never has to resurrect it.
		var newStates map[string]json.RawMessage
		var oldCloseErr error
		persistMu.Lock()
		if sess != nil {
			next, flushErr := writeNewTranscriptLocked(currentAg, sess, sessBaselineMsgs)
			sessBaselineMsgs = next
			if flushErr != nil {
				persistMu.Unlock()
				rollbackErr := joinSessionCloseError(fmt.Errorf("flush current session: %w", flushErr), newSess)
				rollback()
				return rollbackErr
			}
			oldCloseErr = sess.Close()
		}
		sess = newSess
		if newSess != nil {
			newStates = copyExtensionStates(newSess.ExtensionState)
		}
		sessBaselineMsgs = 0
		sessionTitlePending = false
		activeProvider = newProvider
		activeModel = newModel
		persistMu.Unlock()
		ag = newAg
		closeAgentLSP(currentAg)

		// Push the new state into the running Interactive.
		if iv != nil {
			startupPaths := instructionContextPaths(loadAgentsContext(absPath, ZutHome()))
			iv.ApplyChangedCWDWithStartupContext(newAg, newProvider, newModel, absPath, startupPaths)
		}

		// Re-scope the subagent dashboard to the new session.
		if newSess != nil {
			if iv != nil {
				iv.SetSubagentSessionScope(newSess.ID)
			} else {
				runtime.SetActiveSession(newSess.ID)
			}
		}
		if newSess != nil {
			extMgr.EmitSessionEvent("session_opened", sessionContext(newSess), newStates)
		}
		if oldCloseErr != nil {
			return fmt.Errorf("close current session: %w", oldCloseErr)
		}
		return nil
	}

	devBuild := isDevVersion(version)
	if devBuild {
		fmt.Fprintln(os.Stderr, "zut:", devVersionNotice)
	}
	term := tui.NewProcTerm()

	var updateCh chan modes.UpdateInfo
	var changelogCh chan modes.ChangelogPayload
	if !devBuild {
		// Kick off the async update check so the banner can appear when the
		// http response eventually arrives (usually <1s on cached DNS). Map
		// agent.UpdateInfo -> modes.UpdateInfo here to avoid a cyclic import.
		updateCh = make(chan modes.UpdateInfo, 1)
		go func() {
			defer close(updateCh)
			src := <-CheckForUpdateAsync(ZutHome(), version)
			updateCh <- modes.UpdateInfo{
				Current:   src.Current,
				Latest:    src.Latest,
				Available: src.Available,
				URL:       src.URL,
			}
		}()

		// Changelog: when the running version differs from the last
		// version whose release notes the user dismissed, fetch the
		// release body from GitHub and have the TUI show it once. On
		// first-ever launch (no prior LastChangelogShown), seed the
		// stored version silently — don't dump release notes at someone
		// who just installed.
		changelogCh = make(chan modes.ChangelogPayload, 1)
		go func() {
			defer close(changelogCh)
			cfg, _ := LoadConfig()
			if cfg.LastChangelogShown == "" {
				SeedChangelogVersion(version)
				return
			}
			if !ShouldShowChangelog(version, cfg) {
				return
			}
			info := <-FetchChangelogAsync(version)
			if info.Body == "" {
				return
			}
			changelogCh <- modes.ChangelogPayload{
				Version: info.Version,
				Body:    info.Body,
				URL:     info.URL,
			}
		}()
	}

	fastMode := r.FastMode
	quickModelShortcuts := make([]modes.QuickModelShortcut, len(initialCfg.QuickModelShortcuts))
	for idx, s := range initialCfg.QuickModelShortcuts {
		quickModelShortcuts[idx] = modes.QuickModelShortcut{Provider: s.Provider, Model: s.Model}
	}
	theme, _, themeErr := tui.DetectThemeWithCustom(ZutHome(), initialCfg.Theme, 80*time.Millisecond)
	if themeErr != nil {
		fmt.Fprintln(os.Stderr, "theme load:", themeErr)
		if initialCfg.Theme != "" && !tui.ThemeExists(ZutHome(), initialCfg.Theme) {
			initialCfg.Theme = ""
			_ = SaveConfig(initialCfg)
		}
	}

	// Scope the dashboard to the active host session. The runtime owns
	// shutdown separately and closes the supervisor exactly once.
	if sess != nil {
		runtime.SetActiveSession(sess.ID)
	}

	var startupSkills []*skills.Skill
	if r.SkillTool != nil {
		startupSkills = r.SkillTool.Skills()
	}
	homeDir, _ := os.UserHomeDir()
	discoveredSubagents, _ := subagents.Discover(r.CWD, homeDir)
	subagentsAddendum := subagents.SystemPromptAddendum(discoveredSubagents)
	autoSubagentsToolAllowedForSession := autoSubagentsToolAllowed(args)
	autoSubagentsStatusToolAllowedForSession := autoSubagentsStatusToolAllowed(args)
	autoSubagentsStopToolAllowedForSession := autoSubagentsStopToolAllowed(args)
	autoSubagentsResumeToolAllowedForSession := autoSubagentsResumeToolAllowed(args)
	webSearchToolAllowedForInvocation := webSearchToolAllowedForSession(args)
	webSearchInvocationOverride := webSearchExplicitlyEnabledForSession(args)
	setWebSearchAvailable := func(available bool) {
		webSearchGuard.setAvailable(available)
		if subagentsMgr == nil {
			return
		}
		policy := subagents.WebSearchDeny
		if available {
			policy = subagents.WebSearchAllow
		}
		runtime.SetWebSearchPolicy(policy)
	}

	iv = modes.NewInteractive(modes.InteractiveConfig{
		Terminal:                       term,
		Theme:                          theme,
		InlineImagesEnabled:            initialCfg.InlineImagesEnabled,
		TerminalAlertsEnabled:          initialCfg.TerminalAlertsEnabled,
		TerminalTitleEnabled:           initialCfg.TerminalTitleEnabled,
		AutoSubagentsEnabled:           initialCfg.AutoSubagentsEnabled,
		PonytailEnabled:                initialCfg.PonytailEnabled,
		WebSearchEnabled:               initialCfg.WebSearchEnabled,
		WebSearchToolAllowed:           &webSearchToolAllowedForInvocation,
		WebSearchInvocationOverride:    webSearchInvocationOverride,
		AutoSubagentsToolAllowed:       &autoSubagentsToolAllowedForSession,
		AutoSubagentsStatusToolAllowed: &autoSubagentsStatusToolAllowedForSession,
		AutoSubagentsStopToolAllowed:   &autoSubagentsStopToolAllowedForSession,
		AutoSubagentsResumeToolAllowed: &autoSubagentsResumeToolAllowedForSession,
		FastMode:                       &fastMode,
		LSPEnabled:                     initialCfg.LSPEnabled,
		SubagentLSPEnabled:             initialCfg.SubagentLSPEnabled,
		AutoCompactThreshold:           initialCfg.AutoCompactThreshold,
		JailByDefault:                  initialCfg.JailByDefault,
		QuickModelShortcuts:            quickModelShortcuts,
		RecursiveFileSuggest:           initialCfg.RecursiveFileSuggest,
		RespectGitignore:               initialCfg.RespectGitignore,
		CompactMode:                    initialCfg.CompactMode,
		TUIInputStyle:                  initialCfg.TUIInputStyle,
		TUIStatusPosition:              initialCfg.TUIStatusPosition,
		TUIWorkingPosition:             initialCfg.TUIWorkingPosition,
		TUISubagentPosition:            initialCfg.TUISubagentPosition,
		ThemeName:                      initialCfg.Theme,
		FlatTools:                      initialCfg.FlatToolRender(),
		CompactUser:                    initialCfg.CompactUserInput(),
		ExtensionThemes:                extMgr.ThemeOptions,
		AutoSubagentsSystemAddendum: AutoSubagentsSystemAddendumFor(
			autoSubagentsToolAllowedForSession,
			autoSubagentsStopToolAllowedForSession,
			autoSubagentsResumeToolAllowedForSession,
		),
		OnDemandSubagentsSystemAddendum: OnDemandSubagentsSystemAddendum,
		SubagentsSystemAddendum:         subagentsAddendum,
		SettingsStore:                   configSettingsStore{},
		Model:                           r.Model,
		Provider:                        r.Provider,
		AuthMethod:                      r.AuthMethod,
		BaseURL:                         r.BaseURL,
		Reasoning:                       r.Reasoning,
		OnReasoningChanged:              runtime.SetReasoning,
		SystemPrompt:                    r.SystemPrompt,
		Tools:                           r.ToolRegistry,
		MaxSteps:                        r.MaxSteps,
		CWD:                             r.CWD,
		StartupAgentName:                args.AgentName,
		StartupContextPaths:             instructionContextPaths(r.ContextFiles),
		StartupExtensionNames:           startupExtensionNames(extMgr.All()),
		StartupExtensionErrors:          startupExtensionErrors,
		StartupSkillNames:               startupSkillNames(startupSkills),
		ShowInstructionsAtStartup:       initialCfg.ShowInstructionsAtStartup,
		ZutHome:                         ZutHome(),
		SessionsRoot:                    agentSessionsRoot(ZutHome(), args),
		Version:                         version,
		UpdateInfoChan:                  updateCh,
		Sandbox:                         sharedSandbox,
		Agent:                           ag,
		InitialCompactHandoff:           currentCompactHandoff(),
		InitialSessionTitle: func() string {
			if sess == nil || sessionTitlePending {
				return ""
			}
			return sess.Title
		}(),
		InitialSessionTitlePending: sessionTitlePending,
		InitialInput:               args.Prompt,
		StartupPre:                 args.StartupPre,
		OnStartupPreDone: func() {
			// entry.pre often installs skills/extensions; rediscover them
			// before the user starts a model turn.
			current := liveInteractiveAgent(iv, ag)
			if extMgr != nil {
				_ = extMgr.Reload(context.Background(), 2*time.Second)
			}
			if current != nil {
				if _, err := refreshAgentToolsAndPrompt(args, sharedSandbox, extToolAdapter, current, prepareInteractiveRegistry, iv); err != nil {
					iv.ReportError(err)
				}
			}
		},
		RefreshTools: func() error {
			current := liveInteractiveAgent(iv, ag)
			webSearchPolicy, err := refreshAgentToolsAndPrompt(args, sharedSandbox, extToolAdapter, current, prepareInteractiveRegistry, iv)
			if err != nil {
				if !errors.Is(err, errInteractiveAgentChanged) {
					setWebSearchAvailable(false)
				}
				return err
			}
			setWebSearchAvailable(webSearchPolicy == subagents.WebSearchAllow)
			return nil
		},
		SetWebSearchAvailable: setWebSearchAvailable,
		RefreshPrompt: func() error {
			current := liveInteractiveAgent(iv, ag)
			webSearchPolicy, err := refreshAgentToolsAndPrompt(args, sharedSandbox, extToolAdapter, current, prepareInteractiveRegistry, iv)
			if err != nil {
				return err
			}
			setWebSearchAvailable(webSearchPolicy == subagents.WebSearchAllow)
			return nil
		},
		AuthManager:                mgr,
		LlamaCPPConfig:             ResolveLlamaCPPConfig,
		RefreshLlamaCPPModels:      RefreshLlamaCPPModels,
		BuildAgent:                 buildAgent,
		SetKimiCLIFallbackDisabled: SetKimiCLIFallbackDisabled,
		BuildAgentFor:              buildAgentFor,
		BuildAgentForRescue:        buildAgentForRescue,
		LoggedInProviders: func() []string {
			var out []string
			seen := map[string]bool{}
			for _, p := range knownProviders {
				if CredentialAvailable(p) && !seen[p] {
					out = append(out, p)
					seen[p] = true
				}
			}
			// Include custom providers that have credentials stored.
			for p := range provider.CustomProviders() {
				if CredentialAvailable(p) && !seen[p] {
					out = append(out, p)
					seen[p] = true
				}
			}
			// Ollama models are always available (no auth needed).
			if !seen["ollama"] {
				out = append(out, "ollama")
			}
			return out
		},
		LoadSession:           loadSession,
		ChangeCWD:             changeCWD,
		PersistCompactHandoff: persistCompactHandoff,
		CurrentCompactHandoff: currentCompactHandoff,
		CurrentSessionPath: func() string {
			persistMu.Lock()
			defer persistMu.Unlock()
			if sess == nil {
				return ""
			}
			return sess.Path
		},
		FlushSession: func() {
			sessionTransitionMu.RLock()
			defer sessionTransitionMu.RUnlock()
			// Append any not-yet-persisted agent messages to the
			// current session file, then advance the baseline so
			// the final WriteNewTranscript at exit doesn't write
			// duplicates. Per-message persistence keeps the on-
			// disk file current already, so this is mostly a
			// defensive flush — still needed for /session export
			// to guarantee the exported bytes include the very
			// last in-flight turn.
			currentAg := iv.Agent()
			if currentAg == nil {
				return
			}
			persistMu.Lock()
			defer persistMu.Unlock()
			if sess == nil {
				return
			}
			next, flushErr := writeNewTranscriptLocked(currentAg, sess, sessBaselineMsgs)
			sessBaselineMsgs = next
			if flushErr != nil {
				iv.ReportError(fmt.Errorf("flush session: %w", flushErr))
				return
			}
		},
		SessionTransition: newSessionTransition(&sessionTransitionMu),
		Extensions:        extMgr,
		Supervisor:        subagentsMgr,
		ResolveSubagent:   resolveSubagent,
		ChangelogChan:     changelogCh,
		OnChangelogDismiss: func() {
			// For development builds, store the actual release version
			// so the same changelog doesn't show again next launch.
			// For real builds, store the binary version.
			v := version
			if devBuild {
				if iv != nil && iv.ChangelogVersion() != "" {
					v = iv.ChangelogVersion()
				}
			}
			_ = MarkChangelogShown(v)
		},
		SkillSnapshot: func() []*skills.Skill {
			if args.NoSkill {
				// --no-skill: nothing for the picker to show.
				return nil
			}
			// Re-discover so the picker reflects edits made during
			// the session. Cheap; SKILL.md files are small. Filter
			// out built-in skills — they're hidden from user-facing
			// surfaces because they're implementation detail; the
			// model still sees them through the system-prompt
			// manifest and the skill tool.
			userHome, _ := os.UserHomeDir()
			list, _ := skills.Discover(ZutHome(), r.CWD, userHome, args.WithSkills)
			list = mergeExtensionSkills(skills.NewTool(list), extMgr.Skills())
			return skills.VisibleSkills(list)
		},
		NoYolo:      args.NoYolo,
		ConfirmGate: confirmGate,
		PersistModel: newPersistModelCallback(&persistMu, &sess, &activeProvider, &activeModel, func(message string) {
			if iv != nil {
				iv.Notify("session", "error", message)
			}
		}),
		CurrentSessionTitle: func() string {
			sessionTransitionMu.RLock()
			defer sessionTransitionMu.RUnlock()
			persistMu.Lock()
			defer persistMu.Unlock()
			if sess == nil || sessionTitlePending {
				return ""
			}
			return sess.Title
		},
		CurrentSessionTitlePending: func() bool {
			sessionTransitionMu.RLock()
			defer sessionTransitionMu.RUnlock()
			persistMu.Lock()
			defer persistMu.Unlock()
			return sessionTitlePending
		},
		PersistTitle: func(title string) error {
			persistMu.Lock()
			defer persistMu.Unlock()
			if sess == nil {
				return nil
			}
			if err := sess.UpdateTitle(title); err != nil {
				return err
			}
			sessionTitlePending = false
			return nil
		},
		OnSessionTitleChanged: func(title string) {
			sessionTransitionMu.RLock()
			defer sessionTransitionMu.RUnlock()
			persistMu.Lock()
			defer persistMu.Unlock()
			if sess != nil {
				sess.Title = title
				sessionTitlePending = false
			}
		},
	})
	extHooks.attachInteractive(iv)
	go func() {
		<-reloadDone
		for _, reloadErr := range reloadErrs {
			if reloadErr != nil {
				iv.ReportError(fmt.Errorf("reload subagents: %w", reloadErr))
			}
		}
	}()

	// Bind the interactive TUI as the Confirmer. We deferred this
	// until now because the gate is constructed before the TUI
	// (the BeforeToolExecute closure captures it). SetConfirmer
	// is mutex-guarded on the gate so this is safe.
	if confirmGate != nil {
		confirmGate.SetConfirmer(&confirmationEventConfirmer{
			inner: iv,
			emit:  extMgr.EmitEvent,
		})
	}

	// Signal-driven flush: a SIGTERM / SIGHUP to the zut process
	// (closed terminal window, system shutdown, kill) used to lose
	// the entire in-memory transcript because the deferred post-Run
	// flush below never ran. Per-message persistence above covers
	// most of it; this handler writes any in-flight remainder and
	// then exits the process so we don't double-paint over a
	// broken terminal that the TUI's restore deferreds can no
	// longer fix from a signal context.
	//
	// SIGINT is intentionally NOT handled here — the TUI consumes
	// Ctrl+C as a regular key event for cancel/clear semantics, and
	// installing a SIGINT notifier here would swallow it.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)
	go func() {
		_, ok := <-sigCh
		if !ok {
			return
		}
		finalAg := iv.Agent()
		if finalAg != nil {
			persistMu.Lock()
			if sess != nil {
				next, flushErr := writeNewTranscriptLocked(finalAg, sess, sessBaselineMsgs)
				sessBaselineMsgs = next
				if flushErr != nil {
					fmt.Fprintf(os.Stderr, "flush session: %v\n", flushErr)
				}
				if closeErr := sess.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "close session: %v\n", closeErr)
				}
				sess = nil
			}
			persistMu.Unlock()
		}
		closeAgentLSP(finalAg)
		closeResolvedLSP(r)
		// Stop supervised workers before exiting. os.Exit skips deferred
		// cleanup, so wait for reload and close the shared runtime explicitly.
		<-reloadDone
		_ = closeSubagentRuntimeFresh(runtime)
		// Exit cleanly. Re-raising the signal would skip os.Exit's
		// at-exit hooks; explicit exit is fine because we've already
		// flushed the only at-risk state (the session file) and stopped
		// supervised workers.
		os.Exit(0)
	}()

	runErr = iv.Run(ctx)

	// Flush final transcript to session (only if we had / ended up with an agent).
	if finalAg := iv.Agent(); finalAg != nil {
		persistMu.Lock()
		if sess != nil {
			next, flushErr := writeNewTranscriptLocked(finalAg, sess, sessBaselineMsgs)
			sessBaselineMsgs = next
			if flushErr != nil {
				runErr = errors.Join(runErr, fmt.Errorf("flush session: %w", flushErr))
			}
		}
		persistMu.Unlock()
	}
	return runErr
}

func resumableSessionID(root string, sess *core.Session) string {
	if sess == nil || sess.ID == "" || sess.Path == "" {
		return ""
	}
	path, err := core.FindManagedSessionByID(context.Background(), root, sess.ID)
	if err != nil || !sameSessionPath(path, sess.Path) {
		return ""
	}
	snapshot, err := core.ReadSessionSnapshot(path)
	if err != nil || len(snapshot.Messages) == 0 {
		return ""
	}
	return sess.ID
}

func sameSessionPath(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr == nil && rightErr == nil {
		return filepath.Clean(leftAbs) == filepath.Clean(rightAbs)
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func resumeSessionHint(id string) string {
	return fmt.Sprintf("Resume this session with: zut --resume %s\n", id)
}

func agentSessionsRoot(root string, args Args) string {
	if args.AgentName == "" {
		return root
	}
	return filepath.Join(root, "sessions", "agents", safeAgentName(args.AgentName))
}

// openOrCreateSession returns a session for the run. sess may be nil
// with a nil error if session persistence is disabled.
func openOrCreateSession(ctx context.Context, args Args, r Resolved, ag *core.Agent, version string) (*core.Session, error) {
	if args.NoSess {
		return nil, nil
	}
	// Sweep meta-only files left over from older zut versions (and from
	// any session that crashed before its first AppendMessage) for the
	// cwd-scoped picker paths. Explicit UUID lookup stays strict so it
	// can report metadata failures instead of turning them into misses.
	sessionsRoot := agentSessionsRoot(ZutHome(), args)
	if args.ResumeSessionID == "" {
		core.PruneEmptySessions(sessionsRoot, args.CWD)
	}
	var (
		s    *core.Session
		msgs []provider.Message
		err  error
	)
	switch {
	case args.Session != "":
		s, msgs, err = core.OpenSession(args.Session)
		// The subagent-worker child passes a fixed --session path that
		// may not exist yet on first Spawn. Treat ENOENT as "create
		// a fresh session AT THIS PATH" so the conversation actually
		// gets persisted; without this fallback the subagent child runs
		// with sess==nil and every Resume re-starts with no memory
		// of the prior turns. Other openers (--continue / --resume /
		// the picker) never see ENOENT here because they only choose
		// paths that already exist on disk.
		if err != nil && errors.Is(err, os.ErrNotExist) {
			s, err = core.NewSessionAtPath(args.Session, args.CWD, r.Provider, r.Model, version)
			msgs = nil
		}
	case args.Continue:
		latest := core.LatestSession(sessionsRoot, args.CWD)
		if latest != "" {
			s, msgs, err = core.OpenSession(latest)
		}
	case args.Resume:
		if args.ResumeSessionID != "" {
			picked, lookupErr := core.FindManagedSessionByID(ctx, ZutHome(), args.ResumeSessionID)
			if lookupErr != nil {
				return nil, lookupErr
			}
			if picked == "" {
				return nil, fmt.Errorf("session %q not found", args.ResumeSessionID)
			}
			s, msgs, err = core.OpenSession(picked)
			break
		}
		picked, perr := pickSession(sessionsRoot, args.CWD)
		if perr != nil {
			return nil, perr
		}
		if picked != "" {
			s, msgs, err = core.OpenSession(picked)
		}
	}
	if err != nil {
		return nil, err
	}
	if s != nil {
		ag.SetMessages(msgs)
		if cum, last, uerr := core.SessionUsageDetail(s.Path); uerr == nil {
			ag.SeedCost(cum)
			ag.SeedLastTurnUsage(last)
		}
		return s, nil
	}
	return core.NewSession(sessionsRoot, args.CWD, r.Provider, r.Model, version)
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

// WriteNewTranscript appends only messages after index `from` from the
// agent's transcript to the session. Used by callers that don't hold
// the persistMu (non-interactive print/json modes which run a single
// turn under their own goroutine).
func WriteNewTranscript(ag *core.Agent, sess *core.Session, from int) error {
	_, err := writeNewTranscriptLocked(ag, sess, from)
	return err
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
	msgs := ag.Messages()
	for i := from; i < len(msgs); i++ {
		if err := sess.AppendMessage(msgs[i]); err != nil {
			return next, fmt.Errorf("append transcript message %d: %w", i, err)
		}
		next = i + 1
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

func readAllStdin() (string, error) {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return "", err
	}
	if (fi.Mode() & os.ModeCharDevice) != 0 {
		return "", nil
	}
	return readAllStdinFrom(os.Stdin)
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
