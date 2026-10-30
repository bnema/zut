package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zut/packages/agent/extensions"
	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/skills"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/auth"
)

// NewSessionDeps assembles the terminal-free resources of one daemon-owned
// session from a client's CreateSpec. It mirrors the interactive assembly in
// cli.go (Resolve, extensions, subagent runtime, MCP, confirmation gate and
// session file) but takes the working directory and environment from the spec
// rather than the process. The returned cleanup releases every resource and
// is safe to call more than once.
//
// ctx bounds startup work only (Resolve, extension readiness, opening the
// session file). Long-lived pieces (agent tool/assistant hooks, MCP
// connections and extension processes) use context.WithoutCancel(ctx), so a
// cancelled create request never tears down a running session; cleanup owns
// their lifetime. Provider credentials and endpoints are read from spec.Env,
// as the CLI would from that shell; the auth store keeps its priority.
// Interactive pickers are never used: --resume needs an ID or ResumePath.
//
// Scheduler, Directory, DeferredStore and Clock are left for the daemon
// factory. The factory must call BindSessionHooks once the controller exists
// so extension hooks, confirmations and questions reach it.
func NewSessionDeps(ctx context.Context, spec session.CreateSpec, version string) (session.Deps, func(), error) {
	if !filepath.IsAbs(spec.CWD) {
		return session.Deps{}, nil, fmt.Errorf("session cwd must be absolute: %q", spec.CWD)
	}
	args, err := ParseArgs(spec.Args)
	if err != nil {
		return session.Deps{}, nil, fmt.Errorf("parse session args: %w", err)
	}
	// The creating client owns cwd and env; ParseArgs' process defaults are
	// replaced so no process state leaks into the session.
	args.CWD = filepath.Clean(spec.CWD)
	args.Env = append([]string{}, spec.Env...)
	if spec.ResumePath != "" {
		args.Session = spec.ResumePath
		args.Continue, args.Resume, args.ResumeSessionID = false, false, ""
	}
	if args.Resume && args.ResumeSessionID == "" {
		return session.Deps{}, nil, errors.New("resume requires a session id or ResumePath; pick on the client")
	}
	applyZutfileSpec(&args, spec.Zutfile)

	a := &sessionAssembly{args: args, version: version, bind: newSessionBindings()}
	ok := false
	defer func() {
		if !ok {
			a.cleanup()
		}
	}()
	if err := a.build(ctx, spec); err != nil {
		return session.Deps{}, nil, err
	}
	ok = true
	deps := a.deps()
	deps.Headless = spec.Headless
	deps.HeadlessOrchestrate = spec.Headless && args.Orchestrate
	if spec.Headless && a.fresh {
		pinArgs, bundled := a.args, a.extMgr.Skills()
		deps.FirstPromptTransform = func(prompt string) string {
			return preloadSkillPins(pinArgs, true, prompt, bundled, os.Stderr)
		}
	}
	return deps, a.cleanup, nil
}

// BindSessionHooks connects late-bound placeholders to the controller built
// from deps. hooks receives extension host callbacks (buffered until bound),
// confirmer answers the confirmation gate and asker answers ask_user_question.
func BindSessionHooks(deps session.Deps, hooks extensions.HostHooks, confirmer core.Confirmer, asker tools.QuestionAsker) error {
	host, ok := deps.Extensions.(*sessionExtensionHost)
	if !ok || host == nil || host.bind == nil {
		return errors.New("session deps were not built by NewSessionDeps")
	}
	if confirmer != nil && deps.ConfirmGate != nil {
		deps.ConfirmGate.SetConfirmer(confirmer)
	}
	host.bind.setAsker(asker)
	host.bind.setHooks(hooks)
	return nil
}

// sessionAssembly owns the per-session state shared by the Deps adapters. mu
// mirrors runInteractive's persistMu and transition its sessionTransitionMu.
type sessionAssembly struct {
	daemonID string
	args     Args
	version  string
	bind     *sessionBindings

	r          Resolved
	sandbox    *tools.Sandbox
	initialCfg Config
	authMgr    *auth.Manager
	extMgr     *extensions.Manager
	extAdapter *extToolAdapter
	runtime    *subagentRuntime
	gate       *core.ConfirmGate
	webGuard   *webSearchSessionGuard
	stopMCP    func()
	reloadDone chan struct{}

	transition sync.RWMutex
	mu         sync.Mutex
	sess       *core.Session
	baseline   int
	fresh      bool
	live       *core.Agent
	next       *core.Agent
	provider   string
	model      string
	planSnaps  map[string]core.PlanUpdate

	webSearchAllowed                                                   bool
	autoAllowed, autoStatusAllowed, autoStopAllowed, autoResumeAllowed bool
	autoInterruptAllowed, fastMode                                     bool

	cleanupOnce sync.Once
}

func (a *sessionAssembly) build(ctx context.Context, spec session.CreateSpec) error {
	life := context.WithoutCancel(ctx)
	a.initialCfg, _ = LoadConfig()
	if a.initialCfg.Goals.MaxTokenBudget != nil && *a.initialCfg.Goals.MaxTokenBudget == 0 {
		return errors.New("goals.max_token_budget must be greater than zero")
	}

	manager, stopMCP, err := startSessionMCP(life, a.args, a.args.Env)
	if err != nil {
		return err
	}
	a.stopMCP = stopMCP
	a.args.mcpManager = manager

	a.r, err = Resolve(a.args, false)
	if err != nil {
		return err
	}
	if a.args.PermissionSet != nil && (a.r.Sandbox == nil || a.r.Sandbox.Permissions != a.args.PermissionSet) {
		return errors.New("zutfile permissions could not be applied to the session sandbox")
	}
	if resolvedCfg, err := LoadConfig(); err == nil {
		a.initialCfg.QuickModelShortcuts = resolvedCfg.QuickModelShortcuts
		a.initialCfg.ActiveModelProfile = resolvedCfg.ActiveModelProfile
	}
	a.authMgr = auth.NewManager(AuthStoreFor())
	a.sandbox = a.r.Sandbox
	a.provider, a.model = a.r.Provider, a.r.Model

	a.attachQuestionAsker(a.r.ToolRegistry)
	a.extMgr = extensions.New(ZutHome(), a.r.CWD, a.version, a.r.Provider, a.r.Model, a.bind.hooks(),
		extensions.WithEnv(a.args.Env), extensions.WithCWD(a.r.CWD))
	var startupErrs []error
	startupErrs = append(startupErrs, a.extMgr.LoadExplicit(life, a.args.Exts)...)
	if !a.args.NoExt {
		startupErrs = append(startupErrs, a.extMgr.Discover(life)...)
	}
	a.extMgr.WaitForReady(3 * time.Second)
	for _, e := range startupErrs {
		a.bind.hooks().Notify("extensions", "error", e.Error())
	}
	for _, msg := range a.args.mcpStartupErrors {
		a.bind.hooks().Notify("mcp", "error", msg)
	}
	a.extAdapter = &extToolAdapter{mgr: a.extMgr}
	a.r.MergeExtensionTools(a.extAdapter)

	a.webGuard = &webSearchSessionGuard{}
	a.runtime = newSubagentRuntime(subagentRuntimeConfig{
		Args:            a.args,
		Root:            filepath.Join(ZutHome(), "subagents"),
		RepoRoot:        a.r.CWD,
		Provider:        a.r.Provider,
		Model:           a.r.Model,
		Reasoning:       a.r.Reasoning,
		BaseURL:         a.r.BaseURL,
		InsecureTLS:     a.r.InsecureTLS,
		FastMode:        a.r.FastMode,
		ContextWindow:   a.r.ContextWindow,
		Policy:          subagentPolicyFromConfig(a.initialCfg.Subagents),
		WebSearchPolicy: webSearchPolicyForRegistry(a.r.WebSearchPolicy, a.r.ToolRegistry),
		WebSearchGuard:  a.webGuard,
		ActiveProvider:  func() string { a.mu.Lock(); defer a.mu.Unlock(); return a.provider },
		ActiveModel:     func() string { a.mu.Lock(); defer a.mu.Unlock(); return a.model },
	})
	a.reloadDone = make(chan struct{})
	if resident := a.runtime.ResidentManager(); resident != nil {
		go func() {
			defer close(a.reloadDone)
			for _, e := range resident.Reconcile() {
				if e != nil {
					a.bind.hooks().Notify("subagents", "error", "reload subagents: "+e.Error())
				}
			}
		}()
	} else {
		close(a.reloadDone)
	}
	a.prepareResolvedRegistry(a.r.ToolRegistry, a.r.WebSearchPolicy)
	initialPolicy := webSearchPolicyForRegistry(a.r.WebSearchPolicy, a.r.ToolRegistry)
	a.webGuard.setAvailable(initialPolicy == subagents.WebSearchAllow)
	a.runtime.SetWebSearchPolicy(initialPolicy)

	// The gate always exists so the controller can bind a confirmer; NoYolo
	// alone decides whether a tool call consults it (cli.go:1781–1831).
	a.gate = core.NewConfirmGate(nil)

	a.extMgr.SetOnReload(func() {
		if current := a.liveAgent(); current != nil {
			if _, err := refreshAgentToolsAndPrompt(a.args, a.sandbox, a.extAdapter, current, a.prepareRegistry, nil); err != nil {
				a.bind.hooks().Notify("extensions", "error", err.Error())
			}
		}
	})

	if err := a.openSession(ctx, life, spec); err != nil {
		return err
	}
	return a.runStartupPre(life)
}

// applyZutfileSpec copies `zut run` settings onto args parsed from the spec.
func applyZutfileSpec(args *Args, z *session.ZutfileSpec) {
	if z == nil {
		return
	}
	args.AgentName, args.AgentDataDir, args.StartupPre = z.Name, z.DataDir, strings.TrimSpace(z.Pre)
	if z.Permissions != nil {
		perms := *z.Permissions
		args.PermissionSet = &perms
	}
	args.ExtraSkillDirs = append(append([]string{}, z.SkillDirs...), args.ExtraSkillDirs...)
}

// runStartupPre runs zutfile entry.pre once on the prepared agent, as the
// in-process modes did, then reloads extensions, skills and tools it may
// have installed. Output reaches the transcript, not a terminal.
func (a *sessionAssembly) runStartupPre(ctx context.Context) error {
	if a.args.StartupPre == "" {
		return nil
	}
	if err := runZutfileStartupPreEnv(ctx, a.args.StartupPre, a.r.CWD, a.args.Env, a.sandbox, a.next, nil, nil); err != nil {
		return err
	}
	if a.next == nil {
		return nil
	}
	_ = a.extMgr.Reload(ctx, 2*time.Second)
	return sessionToolRefresher{a}.RefreshTools()
}

// openSession creates, opens, resumes or forks the session file as
// runInteractive does, leaving the prepared agent for the first BuildAgent.
func (a *sessionAssembly) openSession(ctx, life context.Context, spec session.CreateSpec) error {
	if spec.Fork != nil {
		if spec.ResumePath != "" {
			return fmt.Errorf("fork cannot include resume_path: %w", session.ErrInvalidFork)
		}
		root := agentSessionsRoot(ZutHome(), a.args)
		path, err := core.BranchSessionChecked(ctx, spec.Fork.ParentPath, root, a.args.CWD, a.version, spec.Fork.UpToMessage, spec.Fork.Hidden, spec.Fork.HistorySegment, spec.Fork.ParentFile)
		if err != nil {
			return fmt.Errorf("fork session: %w", err)
		}
		a.args.Session = path
		a.args.Continue, a.args.Resume, a.args.ResumeSessionID = false, false, ""
	}
	var ag *core.Agent
	if a.r.HasCredential() {
		ag = a.wireAgentExt(life, a.r.newInteractiveAgent())
	}
	a.fresh = a.args.NoSess // like openOrCreateSessionState without a session file
	if !a.args.NoSess && ag != nil {
		sess, fresh, err := openOrCreateSessionState(ctx, a.args, a.r, ag, a.version)
		if err != nil {
			return err
		}
		a.fresh = fresh
		a.baseline = len(ag.Messages())
		if sess != nil {
			candidate, err := applySessionResume(sess, ag, a.r.Provider, a.r.Model,
				strings.TrimSpace(a.args.Provider) != "", strings.TrimSpace(a.args.Model) != "",
				func(p, m string) (*core.Agent, string, string, error) { return a.buildFor(life, p, m, false) })
			if err != nil {
				return err
			}
			sess, ag = candidate.session, candidate.agent
			a.baseline = candidate.fullMessageCount
			a.planSnaps = candidate.planSnapshots
			a.provider, a.model = candidate.provider, candidate.model
			a.r.Provider, a.r.Model = candidate.provider, candidate.model
		}
		a.sess = sess
	}
	a.wireAgentPersist(ag)
	a.next = ag
	announceSession(a.extMgr, a.sess)
	if a.sess != nil {
		a.runtime.SetActiveSession(a.sess.ID)
	}
	return nil
}

func (a *sessionAssembly) attachQuestionAsker(reg core.Registry) {
	if tool, ok := reg[tools.AskUserQuestionToolName].(*tools.AskUserQuestionTool); ok {
		tool.Asker = a.bind.asker
	}
}

func (a *sessionAssembly) prepareRegistry(reg core.Registry) core.Registry {
	a.attachQuestionAsker(reg)
	return a.runtime.PrepareRegistry(reg)
}

func (a *sessionAssembly) prepareResolvedRegistry(reg core.Registry, policy subagents.WebSearchPolicy) {
	a.attachQuestionAsker(reg)
	a.runtime.PrepareResolvedRegistry(reg, policy)
}

// wireAgentExt mirrors runInteractive's extension and confirmation hooks.
func (a *sessionAssembly) wireAgentExt(ctx context.Context, ag *core.Agent) *core.Agent {
	if ag == nil {
		return nil
	}
	extMgr, gate, noYolo := a.extMgr, a.gate, a.args.NoYolo
	ag.BeforeToolExecute = func(call provider.ToolCallBlock) (bool, string, json.RawMessage) {
		r := extMgr.InterceptToolCall(ctx, call.ID, call.Name, call.Arguments)
		if r.Block {
			return false, r.Reason, nil
		}
		effectiveArgs := call.Arguments
		if len(r.ModifiedArgs) > 0 && json.Valid(r.ModifiedArgs) {
			effectiveArgs = r.ModifiedArgs
		}
		if noYolo && call.Name != tools.AskUserQuestionToolName && call.Name != tools.SessionsToolName {
			var content strings.Builder
			_, currentTools := ag.PromptConfig()
			key := confirmationKeyFor(currentTools, call.Name, effectiveArgs)
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
			ok, reason, _ := gate.CheckToolCall(core.ToolCallConfirmation{
				ID:      call.ID,
				Name:    call.Name,
				Key:     key,
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
	ag.BeforeTurnContext = func(turnCtx context.Context, step int) (bool, string, string) {
		r := extMgr.InterceptTurnStart(turnCtx, step)
		return !r.Block, r.Reason, r.Context
	}
	ag.BeforeAssistantMessage = func(text string) (bool, string, string) {
		r := extMgr.InterceptAssistantMessage(ctx, text)
		if r.Block {
			return false, r.Reason, ""
		}
		return true, "", r.ReplaceText
	}
	ag.OnEvent = func(ev core.AgentEvent) { fanoutAgentEvent(extMgr, ev) }
	return ag
}

// wireAgentPersist mirrors runInteractive's per-message persistence hooks and
// records the agent as the live one used by flush, plan and refresh paths.
func (a *sessionAssembly) wireAgentPersist(ag *core.Agent) *core.Agent {
	if ag == nil {
		return nil
	}
	a.mu.Lock()
	current := a.sess
	a.live = ag
	a.mu.Unlock()
	if current != nil {
		ag.SetSessionTimeContext(current.Meta.Started, current.Meta.Timezone, current.Meta.TimezoneOffset)
	}
	ag.OnMessageAppended = func(m provider.Message) {
		a.withSession(func(s *core.Session) {
			if err := s.AppendMessage(m); err == nil {
				a.baseline++
			}
		})
	}
	ag.OnUsage = func(cum provider.Usage) { a.withSession(func(s *core.Session) { _ = s.AppendUsage(cum, cum) }) }
	ag.OnRetryLifecycle = func(rec core.RetryLifecycleRecord) {
		a.withSession(func(s *core.Session) { s.RecordRetryLifecycle(rec) })
	}
	ag.OnTranscriptCompacted = func(messages []provider.Message) {
		var ctxInfo *extproto.SessionContext
		var states map[string]json.RawMessage
		a.withSession(func(s *core.Session) {
			if err := s.AppendCompaction(messages); err == nil {
				a.baseline = len(messages)
				ctxInfo = sessionContext(s)
				states = copyExtensionStates(s.ExtensionState)
			}
		})
		if ctxInfo != nil {
			a.extMgr.EmitSessionEvent("session_compacted", ctxInfo, states)
		}
	}
	a.bindToolResultSession(ag, current)
	return ag
}

func (a *sessionAssembly) bindToolResultSession(ag *core.Agent, bound *core.Session) {
	if ag == nil {
		return
	}
	budget := a.initialCfg.Goals.MaxTokenBudget
	ag.CommitToolResult = func(_ string, result core.ToolResult) error {
		a.mu.Lock()
		defer a.mu.Unlock()
		return persistToolResultState(a.extMgr, bound, result, budget)
	}
}

// withSession runs fn under the transition read lock and persistence lock
// when a session file is open.
func (a *sessionAssembly) withSession(fn func(*core.Session)) {
	a.transition.RLock()
	defer a.transition.RUnlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sess != nil {
		fn(a.sess)
	}
}

func (a *sessionAssembly) liveAgent() *core.Agent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.live
}

// buildFor resolves a fresh agent. rescue drops launch-time key/base-url
// overrides exactly as runInteractive's buildAgentForRescue.
func (a *sessionAssembly) buildFor(ctx context.Context, providerOverride, modelOverride string, rescue bool) (*core.Agent, string, string, error) {
	next := a.args
	if rescue {
		next.APIKey, next.BaseURL = "", ""
	}
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
	resolved.UseSandbox(a.sandbox)
	a.runtime.SetProvider(resolved.Provider)
	a.runtime.SetModel(resolved.Model)
	a.runtime.SetContextWindow(resolved.ContextWindow)
	a.runtime.SetProviderSettings(resolved.BaseURL, resolved.InsecureTLS)
	a.runtime.SetFastMode(resolved.FastMode)
	resolved.MergeExtensionTools(a.extAdapter)
	a.prepareResolvedRegistry(resolved.ToolRegistry, resolved.WebSearchPolicy)
	return a.wireAgentExt(ctx, resolved.newInteractiveAgent()), resolved.Provider, resolved.Model, nil
}

func (a *sessionAssembly) cleanup() {
	a.cleanupOnce.Do(func() {
		a.mu.Lock()
		live := a.live
		a.mu.Unlock()
		var errs []error
		if live != nil {
			if err := tools.CloseLSPManagers(live.ToolsSnapshot()); err != nil {
				errs = append(errs, fmt.Errorf("close agent lsp: %w", err))
			}
		}
		if err := tools.CloseLSPManagers(a.r.ToolRegistry); err != nil {
			errs = append(errs, fmt.Errorf("close lsp: %w", err))
		}
		if a.extMgr != nil {
			a.extMgr.Stop(2 * time.Second)
		}
		if a.reloadDone != nil {
			<-a.reloadDone
		}
		if err := closeSubagentRuntimeFresh(a.runtime); err != nil {
			errs = append(errs, fmt.Errorf("close subagents: %w", err))
		}
		a.transition.Lock()
		a.mu.Lock()
		if a.sess != nil {
			if err := a.sess.AddDaemonID(a.daemonID); err != nil {
				errs = append(errs, fmt.Errorf("persist daemon id: %w", err))
			}
			if live != nil {
				var err error
				if a.baseline, err = writeNewTranscriptLocked(live, a.sess, a.baseline); err != nil {
					errs = append(errs, fmt.Errorf("flush session: %w", err))
				}
			}
			if err := a.sess.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close session: %w", err))
			}
			a.sess = nil
		}
		a.mu.Unlock()
		a.transition.Unlock()
		if err := errors.Join(errs...); err != nil {
			a.bind.hooks().Notify("session", "error", "cleanup: "+err.Error())
		}
		if a.stopMCP != nil {
			a.stopMCP()
		}
		if a.authMgr != nil {
			a.authMgr.Close()
		}
	})
}

func (a *sessionAssembly) deps() session.Deps {
	a.webSearchAllowed = webSearchToolAllowedForSession(a.args)
	a.autoAllowed = autoSubagentsToolAllowed(a.args)
	a.autoStatusAllowed = autoSubagentsStatusToolAllowed(a.args)
	a.autoStopAllowed = autoSubagentsStopToolAllowed(a.args)
	a.autoResumeAllowed = autoSubagentsResumeToolAllowed(a.args)
	a.autoInterruptAllowed = autoSubagentsInterruptToolAllowed(a.args)
	a.fastMode = a.r.FastMode
	homeDir, _ := os.UserHomeDir()
	discovered, _ := subagents.Discover(a.r.CWD, homeDir)
	shortcuts := make([]session.QuickModelShortcut, len(a.initialCfg.QuickModelShortcuts))
	for i, s := range a.initialCfg.QuickModelShortcuts {
		p := normalizedModelProfile(s)
		shortcuts[i] = session.QuickModelShortcut{Provider: p.Provider, Model: p.Model, Reasoning: p.Reasoning, FastMode: p.FastMode}
	}
	d := session.Deps{
		AgentBuilder:                      sessionAgentBuilder{a},
		SessionStore:                      sessionFileStore{a},
		ResidentBuilder:                   sessionResidentBuilder{a},
		SkillStore:                        sessionSkillStore{a},
		ToolRefresher:                     sessionToolRefresher{a},
		Extensions:                        &sessionExtensionHost{Manager: a.extMgr, bind: a.bind},
		ConfirmGate:                       a.gate,
		SettingsStore:                     configSettingsStore{},
		AuthManager:                       a.authMgr,
		Logger:                            slog.New(slog.DiscardHandler),
		Env:                               append([]string{}, a.args.Env...),
		QuickModelShortcuts:               shortcuts,
		Model:                             a.r.Model,
		Provider:                          a.r.Provider,
		Reasoning:                         a.r.Reasoning,
		CWD:                               a.r.CWD,
		SystemPrompt:                      a.r.SystemPrompt,
		WritingGuidance:                   WritingGuidance(),
		MaxSteps:                          a.r.MaxSteps,
		Version:                           a.version,
		ZutHome:                           ZutHome(),
		SessionsRoot:                      agentSessionsRoot(ZutHome(), a.args),
		NoYolo:                            a.args.NoYolo,
		AutoCompactThreshold:              a.initialCfg.AutoCompactThreshold,
		GoalMaxTokenBudget:                a.initialCfg.Goals.MaxTokenBudget,
		AutoSubagentsEnabled:              a.initialCfg.AutoSubagentsEnabled,
		WebSearchEnabled:                  a.initialCfg.WebSearchEnabled,
		LSPEnabled:                        a.initialCfg.LSPEnabled,
		SubagentLSPEnabled:                a.initialCfg.SubagentLSPEnabled,
		WebSearchToolAllowed:              &a.webSearchAllowed,
		AutoSubagentsToolAllowed:          &a.autoAllowed,
		AutoSubagentsStatusToolAllowed:    &a.autoStatusAllowed,
		AutoSubagentsStopToolAllowed:      &a.autoStopAllowed,
		AutoSubagentsResumeToolAllowed:    &a.autoResumeAllowed,
		AutoSubagentsInterruptToolAllowed: &a.autoInterruptAllowed,
		FastMode:                          &a.fastMode,
		JailByDefault:                     a.initialCfg.JailByDefault,
		AutoSubagentsSystemAddendum:       ProactiveSubagentsSystemAddendumFor(subagentActionsAllowed(a.args)),
		OnDemandSubagentsSystemAddendum:   OnDemandSubagentsSystemAddendum,
		SubagentsSystemAddendum:           subagents.SystemPromptAddendum(discovered),
	}
	// Avoid a typed-nil interface when packaged agents disable residents.
	if resident := a.runtime.ResidentManager(); resident != nil {
		d.ResidentManager = resident
	}
	return d
}

// sessionAgentBuilder implements session.AgentBuilder. The first BuildAgent
// returns the agent prepared while opening the session file; later builds and
// the agent staged by LoadSession/ChangeCWD are handed over the same way.
type sessionAgentBuilder struct{ a *sessionAssembly }

func (b sessionAgentBuilder) BuildAgent() (*core.Agent, string, string, error) {
	a := b.a
	a.mu.Lock()
	staged := a.next
	a.next = nil
	providerName, model := a.provider, a.model
	a.mu.Unlock()
	if staged != nil {
		return staged, providerName, model, nil
	}
	ag, p, m, err := a.buildFor(context.Background(), "", "", false)
	return a.wireAgentPersist(ag), p, m, err
}

func (b sessionAgentBuilder) BuildAgentFor(providerOverride, modelOverride string) (*core.Agent, string, string, error) {
	ag, p, m, err := b.a.buildFor(context.Background(), providerOverride, modelOverride, false)
	return b.a.wireAgentPersist(ag), p, m, err
}

func (b sessionAgentBuilder) BuildAgentForRescue(providerOverride, modelOverride string) (*core.Agent, string, string, error) {
	ag, p, m, err := b.a.buildFor(context.Background(), providerOverride, modelOverride, true)
	return b.a.wireAgentPersist(ag), p, m, err
}

// sessionFileStore implements session.SessionStore over the open core.Session.
type sessionFileStore struct{ a *sessionAssembly }

func (s sessionFileStore) PersistModel(providerName, model string) {
	a := s.a
	newPersistModelCallback(&a.mu, &a.sess, &a.provider, &a.model, func(msg string) {
		a.bind.hooks().Notify("session", "error", msg)
	})(providerName, model)
}

func (s sessionFileStore) PersistTitle(title string) error {
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	if s.a.sess == nil {
		return nil
	}
	return s.a.sess.UpdateTitle(title)
}

func (s sessionFileStore) CurrentGoal() *core.SessionGoal {
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	if s.a.sess == nil || s.a.sess.Meta.Goal == nil {
		return nil
	}
	goal := *s.a.sess.Meta.Goal
	return &goal
}

func (s sessionFileStore) CurrentGoalHistory() []core.SessionGoal {
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	if s.a.sess == nil {
		return nil
	}
	return append([]core.SessionGoal(nil), s.a.sess.Meta.GoalHistory...)
}

var errSessionPersistenceDisabled = errors.New("session persistence is disabled")

func (s sessionFileStore) write(fn func(*core.Session) error) error {
	err := errSessionPersistenceDisabled
	s.a.withSession(func(sess *core.Session) { err = fn(sess) })
	return err
}

func (s sessionFileStore) EnsureMission(objective string) error {
	return s.write(func(sess *core.Session) error { return sess.EnsureMission(objective, core.MissionSourceUser) })
}

func (s sessionFileStore) PersistGoal(goal *core.SessionGoal) error {
	return s.write(func(sess *core.Session) error { return sess.UpdateGoal(goal) })
}

func (s sessionFileStore) PersistGoalRuntime(goal *core.SessionGoal) error {
	return s.write(func(sess *core.Session) error { return sess.UpdateGoalRuntime(goal) })
}

// InitialPlanSnapshots returns the checklist snapshots rebuilt from the full,
// untrimmed transcript when the current session file was opened.
func (s sessionFileStore) InitialPlanSnapshots() map[string]core.PlanUpdate {
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	out := make(map[string]core.PlanUpdate, len(s.a.planSnaps))
	for k, v := range s.a.planSnaps {
		out[k] = v
	}
	return out
}

func (s sessionFileStore) CurrentPlan() []core.PlanStep { return s.a.liveAgent().CurrentPlan() }

func (s sessionFileStore) PersistPlan(plan *core.SessionPlan) error {
	return s.write(func(sess *core.Session) error { return sess.UpdatePlan(plan) })
}

func (s sessionFileStore) PersistedPlan() *core.SessionPlan {
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	if s.a.sess == nil || s.a.sess.Meta.Plan == nil {
		return nil
	}
	plan := *s.a.sess.Meta.Plan
	plan.Steps = append([]core.PlanStep(nil), s.a.sess.Meta.Plan.Steps...)
	return &plan
}

func (s sessionFileStore) SessionTransition(fn func()) { newSessionTransition(&s.a.transition)(fn) }

func (s sessionFileStore) FlushSession() {
	live := s.a.liveAgent()
	if live == nil {
		return
	}
	s.a.withSession(func(sess *core.Session) {
		next, err := writeNewTranscriptLocked(live, sess, s.a.baseline)
		s.a.baseline = next
		if err != nil {
			s.a.bind.hooks().Notify("session", "error", fmt.Sprintf("flush session: %v", err))
		}
	})
}

func (s sessionFileStore) CurrentSessionPath() string {
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	if s.a.sess == nil {
		return ""
	}
	return s.a.sess.Path
}

// LoadSession mirrors runInteractive's loadSessionContext. The prepared agent
// is staged for the next BuildAgent so the controller installs it.
func (s sessionFileStore) LoadSession(path string) (loadErr error) {
	a := s.a
	meta, err := core.ManagedSessionMeta(context.Background(), agentSessionsRoot(ZutHome(), a.args), path)
	if err != nil {
		return fmt.Errorf("validate selected session: %w", err)
	}
	if info, statErr := os.Stat(meta.CWD); statErr != nil || !info.IsDir() {
		if statErr != nil {
			return fmt.Errorf("selected session cwd: %w", statErr)
		}
		return fmt.Errorf("selected session cwd is not a directory: %s", meta.CWD)
	}
	a.transition.Lock()
	defer a.transition.Unlock()
	current := a.liveAgent()
	if current == nil {
		return errors.New("no agent running; log in first")
	}
	a.mu.Lock()
	currentProvider, currentModel, oldSess := a.provider, a.model, a.sess
	if oldSess != nil {
		next, flushErr := writeNewTranscriptLocked(current, oldSess, a.baseline)
		a.baseline = next
		if flushErr != nil {
			a.mu.Unlock()
			return fmt.Errorf("flush current session: %w", flushErr)
		}
	}
	a.mu.Unlock()

	workspaceChanged := filepath.Clean(meta.CWD) != filepath.Clean(a.args.CWD)
	rollback := a.stageWorkspace(meta.CWD, workspaceChanged)
	committed := false
	var candidate sessionResumeCandidate
	defer func() {
		if !committed {
			rollback()
			loadErr = joinSessionCloseError(loadErr, candidate.session)
		}
	}()
	build := func(p, m string) (*core.Agent, string, string, error) {
		return a.buildFor(context.Background(), p, m, false)
	}
	if workspaceChanged {
		candidate, err = prepareSessionResumeForWorkspace(path, current, currentProvider, currentModel, build)
	} else {
		candidate, err = prepareSessionResume(path, current, currentProvider, currentModel, build)
	}
	if err != nil {
		return err
	}
	a.wireAgentPersist(candidate.agent)

	a.mu.Lock()
	newStates := copyExtensionStates(candidate.session.ExtensionState)
	var oldCloseErr error
	if err := moveDaemonAlias(oldSess, candidate.session, a.daemonID); err != nil {
		a.mu.Unlock()
		return fmt.Errorf("move daemon alias: %w", err)
	}
	if oldSess != nil {
		oldCloseErr = oldSess.Close()
	}
	a.sess = candidate.session
	a.bindToolResultSession(candidate.agent, candidate.session)
	candidate.agent.SetPlan(candidate.session.Meta.PlanSteps())
	candidate.agent.SetSessionTimeContext(candidate.session.Meta.Started, candidate.session.Meta.Timezone, candidate.session.Meta.TimezoneOffset)
	a.baseline = candidate.fullMessageCount
	if !candidate.rebuilt {
		current.SetMessages(candidate.messages)
		current.ResetRepetitionGuard()
		current.SeedCost(candidate.cumulative)
		current.SeedLastTurnUsage(candidate.lastTurn)
	}
	a.live, a.next = candidate.agent, candidate.agent
	a.provider, a.model = candidate.provider, candidate.model
	a.planSnaps = candidate.planSnapshots
	a.runtime.SetActiveSession(candidate.session.ID)
	committed = true
	a.mu.Unlock()
	if candidate.session.Meta.Parent != "" {
		a.extMgr.EmitSessionEvent("session_forked", sessionContext(candidate.session), newStates)
	}
	a.extMgr.EmitSessionEvent("session_switched", sessionContext(candidate.session), newStates)
	if oldCloseErr != nil {
		return fmt.Errorf("close current session: %w", oldCloseErr)
	}
	return nil
}

// stageWorkspace re-points cwd, sandbox, permissions and the subagent root,
// returning a rollback that restores every staged value.
func (a *sessionAssembly) stageWorkspace(cwd string, changed bool) func() {
	oldCWD, oldResolved, oldPerms := a.args.CWD, a.r.CWD, a.args.PermissionSet
	runtimeConfig := a.runtime.snapshotConfiguration()
	a.mu.Lock()
	oldProvider, oldModel := a.provider, a.model
	a.mu.Unlock()
	oldRoot, locked := "", false
	if a.sandbox != nil {
		oldRoot, locked = a.sandbox.Root, a.sandbox.Locked()
	}
	setSandbox := func(root string) {
		if a.sandbox == nil {
			return
		}
		a.sandbox.Root = root
		if locked {
			a.sandbox.Lock()
		} else {
			a.sandbox.Unlock()
		}
	}
	if changed {
		a.args.CWD, a.r.CWD = cwd, cwd
		a.runtime.SetRepoRoot(cwd)
		if a.args.PermissionSet != nil {
			expanded := a.args.PermissionSet.Expand(cwd, a.args.AgentDataDir)
			a.args.PermissionSet = &expanded
		}
		setSandbox(cwd)
	}
	return func() {
		a.args.CWD, a.r.CWD, a.args.PermissionSet = oldCWD, oldResolved, oldPerms
		a.runtime.restoreConfiguration(runtimeConfig)
		a.mu.Lock()
		a.provider, a.model = oldProvider, oldModel
		a.mu.Unlock()
		setSandbox(oldRoot)
	}
}

// moveDaemonAlias stages ownership before closing the old transcript. On
// failure both open sessions retain their original aliases.
func moveDaemonAlias(old, next *core.Session, id string) error {
	if id == "" || old == next {
		return nil
	}
	if err := next.AddDaemonID(id); err != nil {
		return err
	}
	if err := old.RemoveDaemonID(id); err != nil {
		return errors.Join(err, next.RemoveDaemonID(id))
	}
	return nil
}

// ChangeCWD mirrors runInteractive's changeCWD: a relative path resolves
// against the session cwd, never the process cwd, and a fresh session file
// opens in the new workspace. The rebuilt agent is staged for BuildAgent.
func (s sessionFileStore) ChangeCWD(path string) error {
	a := s.a
	a.transition.Lock()
	defer a.transition.Unlock()
	if path == "" {
		return errors.New("empty path")
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return fmt.Errorf("cannot expand ~: %v", err)
		}
		path = filepath.Join(home, strings.TrimPrefix(path[1:], "/"))
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(a.args.CWD, path)
	}
	absPath := filepath.Clean(path)
	info, err := os.Stat(absPath)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory: %s", absPath)
	}
	current := a.liveAgent()
	if current == nil {
		return errors.New("no agent running; log in first")
	}
	rollback := a.stageWorkspace(absPath, true)
	newAg, newProvider, newModel, err := a.buildFor(context.Background(), "", "", false)
	if err != nil {
		rollback()
		return fmt.Errorf("rebuild agent: %w", err)
	}
	var newSess *core.Session
	if !a.args.NoSess {
		root := agentSessionsRoot(ZutHome(), a.args)
		core.PruneEmptySessions(root, absPath)
		if newSess, err = core.NewSession(root, absPath, newProvider, newModel, a.version); err != nil {
			rollback()
			closeAgentLSP(newAg)
			return fmt.Errorf("open session in %s: %w", absPath, err)
		}
	}
	var oldCloseErr error
	var newStates map[string]json.RawMessage
	a.mu.Lock()
	if a.sess != nil {
		next, flushErr := writeNewTranscriptLocked(current, a.sess, a.baseline)
		a.baseline = next
		if flushErr != nil {
			a.mu.Unlock()
			err := joinSessionCloseError(fmt.Errorf("flush current session: %w", flushErr), newSess)
			rollback()
			closeAgentLSP(newAg)
			return err
		}
	}
	if err := moveDaemonAlias(a.sess, newSess, a.daemonID); err != nil {
		a.mu.Unlock()
		err = joinSessionCloseError(fmt.Errorf("move daemon alias: %w", err), newSess)
		rollback()
		closeAgentLSP(newAg)
		return err
	}
	if a.sess != nil {
		oldCloseErr = a.sess.Close()
	}
	a.sess = newSess
	if newSess != nil {
		newStates = copyExtensionStates(newSess.ExtensionState)
	}
	a.baseline = 0
	a.provider, a.model = newProvider, newModel
	a.planSnaps = nil
	a.mu.Unlock()
	a.wireAgentPersist(newAg)
	a.mu.Lock()
	a.next = newAg
	a.mu.Unlock()
	closeAgentLSP(current)
	if newSess != nil {
		a.runtime.SetActiveSession(newSess.ID)
		a.extMgr.EmitSessionEvent("session_opened", sessionContext(newSess), newStates)
	}
	if oldCloseErr != nil {
		return fmt.Errorf("close current session: %w", oldCloseErr)
	}
	return nil
}

// sessionResidentBuilder implements session.ResidentBuilder.
type sessionResidentBuilder struct{ a *sessionAssembly }

func (b sessionResidentBuilder) catalogue() core.Registry {
	if live := b.a.liveAgent(); live != nil {
		return live.ToolsSnapshot()
	}
	return b.a.r.ToolRegistry
}

func (b sessionResidentBuilder) SpawnResident(ctx context.Context, request tools.ResidentSpawnRequest) (string, error) {
	manager := b.a.runtime.ResidentManager()
	if manager == nil {
		return "", errors.New("resident runtime is unavailable")
	}
	spec, err := b.a.runtime.buildResidentChildSpec(ctx, request, b.catalogue())
	if err != nil {
		return "", err
	}
	if _, err := manager.Spawn(ctx, spec, request.Task); err != nil {
		return "", err
	}
	return spec.ID, nil
}

func (b sessionResidentBuilder) BuildResidentSpec(ctx context.Context, request tools.ResidentSpawnRequest) (subagents.ResidentChildSpec, error) {
	return b.a.runtime.buildResidentChildSpec(ctx, request, b.catalogue())
}

func (b sessionResidentBuilder) ResolveSubagent(name string) (*subagents.Profile, error) {
	return b.a.runtime.resolveSubagent(name)
}

// sessionSkillStore implements session.SkillStore.
type sessionSkillStore struct{ a *sessionAssembly }

func (s sessionSkillStore) SkillSnapshot() []*skills.Skill {
	return userSkillSnapshot(s.a.args, s.a.args.CWD, s.a.extMgr.Skills())
}

func (s sessionSkillStore) LoadSkillPins(cwd string) (skills.Pins, error) {
	if s.a.args.NoSkill {
		return skills.Pins{}, nil
	}
	return loadSkillPins(cwd)
}

func (s sessionSkillStore) ToggleSkillPin(cwd, name string, global bool) error {
	return toggleSkillPin(cwd, name, global)
}

// sessionToolRefresher implements session.ToolRefresher.
type sessionToolRefresher struct{ a *sessionAssembly }

func (t sessionToolRefresher) RefreshTools() error {
	policy, err := refreshAgentToolsAndPrompt(t.a.args, t.a.sandbox, t.a.extAdapter, t.a.liveAgent(), t.a.prepareRegistry, nil)
	if err != nil {
		t.SetWebSearchAvailable(false)
		return err
	}
	t.SetWebSearchAvailable(policy == subagents.WebSearchAllow)
	return nil
}

func (t sessionToolRefresher) SetWebSearchAvailable(available bool) {
	t.a.webGuard.setAvailable(available)
	policy := subagents.WebSearchDeny
	if available {
		policy = subagents.WebSearchAllow
	}
	t.a.runtime.SetWebSearchPolicy(policy)
}

// sessionExtensionHost is the session.ExtensionHost over the session's
// extension manager; it also carries the late-binding placeholders.
type sessionExtensionHost struct {
	*extensions.Manager
	bind *sessionBindings
}

// maxBufferedSessionHookCalls bounds non-Notify hook calls made before the
// controller binds. Notify calls are never dropped; when full, the oldest
// non-Notify call is dropped and the count is reported after binding.
const maxBufferedSessionHookCalls = 64

type pendingHookCall struct {
	notify bool
	call   func(extensions.HostHooks)
}

// sessionBindings holds placeholders filled once the controller exists.
type sessionBindings struct {
	mu        sync.Mutex
	target    extensions.HostHooks
	replaying bool
	pending   []pendingHookCall
	others    int
	dropped   int
	ask       tools.QuestionAsker
	adapter   *lateHostHooks
}

func newSessionBindings() *sessionBindings {
	b := &sessionBindings{}
	b.adapter = &lateHostHooks{b: b}
	return b
}

func (b *sessionBindings) hooks() *lateHostHooks { return b.adapter }

func (b *sessionBindings) asker() tools.QuestionAsker {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ask
}

func (b *sessionBindings) setAsker(asker tools.QuestionAsker) {
	b.mu.Lock()
	b.ask = asker
	b.mu.Unlock()
}

// setHooks replays buffered calls in order outside the lock. Calls arriving
// during replay keep appending to pending, so ordering is preserved and a hook
// that re-enters the adapter cannot deadlock.
func (b *sessionBindings) setHooks(target extensions.HostHooks) {
	if target == nil {
		return
	}
	b.mu.Lock()
	if b.target != nil || b.replaying {
		b.mu.Unlock()
		return
	}
	b.replaying = true
	if b.dropped > 0 {
		msg := fmt.Sprintf("dropped %d extension host calls made before the session was ready", b.dropped)
		b.pending = append(b.pending, pendingHookCall{notify: true, call: func(t extensions.HostHooks) { t.Notify("extensions", "warn", msg) }})
		b.dropped = 0
	}
	for {
		if len(b.pending) == 0 {
			b.target = target
			b.replaying = false
			b.others = 0
			b.mu.Unlock()
			return
		}
		next := b.pending[0]
		b.pending = b.pending[1:]
		if !next.notify {
			b.others--
		}
		b.mu.Unlock()
		next.call(target)
		b.mu.Lock()
	}
}

// lateHostHooks forwards extension callbacks to the bound controller hooks
// and buffers calls until binding.
type lateHostHooks struct{ b *sessionBindings }

func (h *lateHostHooks) enqueue(notify bool, call func(extensions.HostHooks)) {
	h.b.mu.Lock()
	target := h.b.target
	if target == nil {
		b := h.b
		if !notify && b.others >= maxBufferedSessionHookCalls {
			for i, p := range b.pending {
				if !p.notify {
					b.pending = append(b.pending[:i:i], b.pending[i+1:]...)
					b.others--
					b.dropped++
					break
				}
			}
		}
		b.pending = append(b.pending, pendingHookCall{notify: notify, call: call})
		if !notify {
			b.others++
		}
		b.mu.Unlock()
		return
	}
	h.b.mu.Unlock()
	call(target)
}

func (h *lateHostHooks) do(call func(extensions.HostHooks)) { h.enqueue(false, call) }

func (h *lateHostHooks) Notify(ext, level, msg string) {
	h.enqueue(true, func(t extensions.HostHooks) { t.Notify(ext, level, msg) })
}
func (h *lateHostHooks) Submit(text string) { h.do(func(t extensions.HostHooks) { t.Submit(text) }) }
func (h *lateHostHooks) SubmitSlash(text string) {
	h.do(func(t extensions.HostHooks) { t.SubmitSlash(text) })
}
func (h *lateHostHooks) Insert(text string) { h.do(func(t extensions.HostHooks) { t.Insert(text) }) }
func (h *lateHostHooks) Display(ext, text string) {
	h.do(func(t extensions.HostHooks) { t.Display(ext, text) })
}
func (h *lateHostHooks) ClearNotes(ext string) {
	h.do(func(t extensions.HostHooks) { t.ClearNotes(ext) })
}
func (h *lateHostHooks) OpenPanel(ext string, spec extproto.PanelSpec) {
	h.do(func(t extensions.HostHooks) { t.OpenPanel(ext, spec) })
}
func (h *lateHostHooks) UpdatePanel(ext, id, title string, lines []string, footer string) {
	lines = append([]string(nil), lines...)
	h.do(func(t extensions.HostHooks) { t.UpdatePanel(ext, id, title, lines, footer) })
}
func (h *lateHostHooks) ClosePanel(ext, id string) {
	h.do(func(t extensions.HostHooks) { t.ClosePanel(ext, id) })
}
func (h *lateHostHooks) SetStatus(ext, key, level, text string) {
	h.do(func(t extensions.HostHooks) { t.SetStatus(ext, key, level, text) })
}
func (h *lateHostHooks) SetWidget(ext, id, position, title string, lines []string) {
	lines = append([]string(nil), lines...)
	h.do(func(t extensions.HostHooks) { t.SetWidget(ext, id, position, title, lines) })
}
func (h *lateHostHooks) ClearWidget(ext, id string) {
	h.do(func(t extensions.HostHooks) { t.ClearWidget(ext, id) })
}

// SupportsInteractiveTools is false until bound hooks report an attached client.
func (h *lateHostHooks) SupportsInteractiveTools() bool {
	h.b.mu.Lock()
	target := h.b.target
	h.b.mu.Unlock()
	t, ok := target.(extensions.InteractiveToolHostHooks)
	return ok && t.SupportsInteractiveTools()
}

func (h *lateHostHooks) Alert(ext string, alert extproto.AlertRequest) {
	h.do(func(t extensions.HostHooks) {
		if a, ok := t.(extensions.AlertHostHooks); ok {
			a.Alert(ext, alert)
		}
	})
}

func (h *lateHostHooks) ClearExtensionChrome(ext string) {
	h.do(func(t extensions.HostHooks) {
		if c, ok := t.(extensions.ExtensionChromeHooks); ok {
			c.ClearExtensionChrome(ext)
		}
	})
}

var (
	_ session.AgentBuilder                = sessionAgentBuilder{}
	_ session.SessionStore                = sessionFileStore{}
	_ session.ResidentBuilder             = sessionResidentBuilder{}
	_ session.SkillStore                  = sessionSkillStore{}
	_ session.ToolRefresher               = sessionToolRefresher{}
	_ session.ExtensionHost               = (*sessionExtensionHost)(nil)
	_ session.ResidentHost                = (*subagents.ResidentManager)(nil)
	_ session.ConfirmGate                 = (*core.ConfirmGate)(nil)
	_ session.SettingsStore               = configSettingsStore{}
	_ extensions.HostHooks                = (*lateHostHooks)(nil)
	_ extensions.InteractiveToolHostHooks = (*lateHostHooks)(nil)
	_ extensions.AlertHostHooks           = (*lateHostHooks)(nil)
	_ extensions.ExtensionChromeHooks     = (*lateHostHooks)(nil)
)
