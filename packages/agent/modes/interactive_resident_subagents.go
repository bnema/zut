package modes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/internal/orchestration"
	"github.com/bnema/zut/packages/agent/modes/telegram"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

func (i *Interactive) TrackResidentSubagent(childID, turnID string) {
	if i == nil {
		return
	}
	if !i.ensureCompletionTracker().TrackResident(childID, turnID) {
		return
	}
	i.registerCoordinatorWorker(childID)
	i.invalidate()
	i.requestCompletionDelivery()
}
func (i *Interactive) ReportResidentSubagent(completion subagents.ResidentCompletion) {
	if i == nil {
		return
	}
	status, errText := "completed", ""
	if completion.Err != nil {
		status, errText = "failed", completion.Err.Error()
		if errors.Is(completion.Err, context.Canceled) {
			status = "interrupted"
		}
	}
	if !i.ensureCompletionTracker().Report(subagents.Completion{AgentID: completion.ChildID, TurnID: completion.TurnID, Status: status, Task: completion.Task, Error: errText, Summary: completion.Summary}) {
		return
	}
	i.reloadOpenResidentChildSession(completion.ChildID)
	i.invalidate()
	i.requestCompletionDelivery()
}
func (i *Interactive) reloadOpenResidentChildSession(childID string) {
	if i == nil {
		return
	}
	i.mu.Lock()
	session := i.residentChildSession
	i.mu.Unlock()
	if session == nil || session.childID != childID || !session.RequestRecentReload() {
		return
	}
	i.reloadResidentChildSession(session)
}
func (i *Interactive) reloadResidentChildSession(session *residentChildSession) {
	if i == nil || session == nil {
		return
	}
	go func() {
		err := session.ReloadAll(200, i.invalidate)
		if session.FinishLoad(err) {
			if session.RequestRecentReload() {
				i.reloadResidentChildSession(session)
			}
		}
		i.invalidate()
	}()
}
func (i *Interactive) ensureCompletionTracker() *subagents.CompletionTracker {
	// Tests and lightweight embedders may construct Interactive with a struct
	// literal instead of NewInteractive, so retain this lazy fallback.
	i.completionDeliveryMu.Lock()
	defer i.completionDeliveryMu.Unlock()
	if i.completionTracker == nil {
		i.completionTracker = subagents.NewCompletionTracker()
	}
	return i.completionTracker
}
func (i *Interactive) ensureCoordinatorLocked() *orchestration.Coordinator {
	if i.turnCoordinator == nil {
		i.turnCoordinator = orchestration.New()
	}
	return i.turnCoordinator
}
func (i *Interactive) coordinatorAcceptsUserInput() bool {
	if i == nil {
		return false
	}
	i.completionDeliveryMu.Lock()
	accepts := i.ensureCoordinatorLocked().AcceptsUserInput()
	i.completionDeliveryMu.Unlock()
	return accepts
}
func (i *Interactive) coordinatorHasPendingWorkers() bool {
	if i == nil {
		return false
	}
	i.completionDeliveryMu.Lock()
	pending := i.ensureCoordinatorLocked().HasPendingWorkers()
	i.completionDeliveryMu.Unlock()
	return pending
}
func (i *Interactive) applyCoordinator(event orchestration.Event) []orchestration.Action {
	if i == nil {
		return nil
	}
	i.completionDeliveryMu.Lock()
	result := i.ensureCoordinatorLocked().Apply(event)
	i.completionDeliveryMu.Unlock()
	return result.Actions
}
func (i *Interactive) registerCoordinatorWorker(workerID string) {
	if i == nil || workerID == "" {
		return
	}
	i.completionDeliveryMu.Lock()
	coordinator := i.ensureCoordinatorLocked()
	implicitWave := i.completionDeliveryHolds == 0
	if implicitWave {
		coordinator.Apply(orchestration.Event{Kind: orchestration.EventManagerStarted})
	}
	i.coordinatorWorkerSeq++
	registrationID := fmt.Sprintf("%s#%d", workerID, i.coordinatorWorkerSeq)
	if i.coordinatorWorkerIDs == nil {
		i.coordinatorWorkerIDs = make(map[string][]string)
	}
	i.coordinatorWorkerIDs[workerID] = append(i.coordinatorWorkerIDs[workerID], registrationID)
	coordinator.Apply(orchestration.Event{Kind: orchestration.EventWorkerRegistered, WorkerID: registrationID})
	var actions []orchestration.Action
	if implicitWave {
		actions = coordinator.Apply(orchestration.Event{Kind: orchestration.EventManagerFinished}).Actions
	}
	i.completionDeliveryMu.Unlock()
	i.executeCoordinatorActions(actions)
}
func (i *Interactive) cancelCoordinator() {
	if i == nil {
		return
	}
	i.completionDeliveryMu.Lock()
	coordinator := i.ensureCoordinatorLocked()
	actions := coordinator.Apply(orchestration.Event{Kind: orchestration.EventCancelled}).Actions
	// A later user turn starts a fresh wave. Drop resident registrations as
	// well as coordinator identities so late outcomes cannot match a new child.
	if i.completionTracker != nil {
		i.completionTracker.Reset()
	}
	i.turnCoordinator = orchestration.New()
	i.coordinatorWorkerIDs = nil
	i.completionDeliveryMu.Unlock()
	i.executeCoordinatorActions(actions)
}
func (i *Interactive) takeCoordinatorWorkerID(agentID string) string {
	if i == nil || agentID == "" {
		return ""
	}
	i.completionDeliveryMu.Lock()
	defer i.completionDeliveryMu.Unlock()
	ids := i.coordinatorWorkerIDs[agentID]
	if len(ids) == 0 {
		return ""
	}
	workerID := ids[0]
	if len(ids) == 1 {
		delete(i.coordinatorWorkerIDs, agentID)
	} else {
		i.coordinatorWorkerIDs[agentID] = ids[1:]
	}
	return workerID
}
func (i *Interactive) executeCoordinatorActions(actions []orchestration.Action) {
	for _, action := range actions {
		if action.Kind != orchestration.ActionRunManager {
			continue
		}
		prompt := action.Text
		if len(action.Completions) != 0 {
			instruction := "Briefly summarise the collective outcome for the user. Reference the agents by id. If any failed, suggest a follow-up; otherwise confirm completion. Do not spawn new sub-agents unless the user asks."
			update := subagents.FormatCompletionUpdate(action.Completions, instruction)
			if prompt != "" {
				prompt = update + "\n\nQueued user request:\n" + prompt
			} else {
				prompt = update
			}
		}
		if prompt != "" || len(action.Images) != 0 {
			i.submitOrQueue(prompt, action.Images, false)
		} else if action.Reason == orchestration.WakeGoal {
			parent := i.runCtx
			if parent == nil {
				parent = context.Background()
			}
			i.requestGoalContinuationIfIdle(parent)
		}
	}
}
func (i *Interactive) requestCompletionDelivery() {
	if i == nil {
		return
	}
	i.ensureCompletionTracker()
	i.completionDeliveryMu.Lock()
	i.completionDeliveryRequest = true
	if i.completionDeliveryRunning || i.completionDeliveryHolds != 0 {
		i.completionDeliveryMu.Unlock()
		return
	}
	i.completionDeliveryRunning = true
	i.completionDeliveryMu.Unlock()

	go i.deliverCompletionUpdates()
}
func (i *Interactive) beginCompletionDeliveryHold() func() {
	if i == nil {
		return func() {}
	}
	i.completionDeliveryMu.Lock()
	i.ensureCoordinatorLocked().Apply(orchestration.Event{Kind: orchestration.EventManagerStarted})
	i.completionDeliveryHolds++
	i.completionDeliveryMu.Unlock()
	return i.releaseCompletionDeliveryHold
}
func (i *Interactive) releaseCompletionDeliveryHold() {
	if i == nil {
		return
	}
	start := false
	var actions []orchestration.Action
	i.completionDeliveryMu.Lock()
	if i.completionDeliveryHolds > 0 {
		i.completionDeliveryHolds--
	}
	if i.completionDeliveryHolds == 0 {
		// The coordinator must observe the goal before sealing the wave, so a
		// pending worker wins the wake decision instead of a direct continuation.
		_, goalActive := i.goalContinuationMessage()
		coordinator := i.ensureCoordinatorLocked()
		coordinator.Apply(orchestration.Event{Kind: orchestration.EventGoalChanged, GoalActive: goalActive})
		actions = coordinator.Apply(orchestration.Event{Kind: orchestration.EventManagerFinished}).Actions
	}
	if i.completionDeliveryHolds == 0 && i.completionDeliveryRequest && !i.completionDeliveryRunning {
		i.completionDeliveryRunning = true
		start = true
	}
	i.completionDeliveryMu.Unlock()
	i.executeCoordinatorActions(actions)
	if start {
		go i.deliverCompletionUpdates()
	}
}
func (i *Interactive) completionWaitContext() context.Context {
	i.mu.Lock()
	ctx := i.runCtx
	i.mu.Unlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
func (i *Interactive) deliverCompletionUpdates() {
	for {
		i.completionDeliveryMu.Lock()
		i.completionDeliveryRequest = false
		tracker := i.completionTracker
		i.completionDeliveryMu.Unlock()

		batch, err := tracker.WaitIdle(i.completionWaitContext())
		if err == nil && len(batch) != 0 {
			var actions []orchestration.Action
			for _, completion := range batch {
				workerID := i.takeCoordinatorWorkerID(completion.AgentID)
				if workerID == "" {
					continue
				}
				actions = append(actions, i.applyCoordinator(orchestration.Event{
					Kind:       orchestration.EventWorkerFinished,
					WorkerID:   workerID,
					Completion: completion,
				})...)
			}
			i.executeCoordinatorActions(actions)
		}

		i.completionDeliveryMu.Lock()
		if i.completionDeliveryRequest {
			i.completionDeliveryMu.Unlock()
			continue
		}
		i.completionDeliveryRunning = false
		i.completionDeliveryMu.Unlock()
		return
	}
}
func autoSubagentsAddenda(cfg InteractiveConfig, orchestrating bool) []string {
	addenda := make([]string, 0, 2)
	if cfg.ResidentManager != nil && autoSubagentsToolAllowedConfig(cfg) {
		if addendum := strings.TrimSpace(cfg.SubagentsSystemAddendum); addendum != "" {
			addenda = append(addenda, addendum)
		}
	}
	if !orchestrating {
		if cfg.ResidentManager != nil && autoSubagentsAnyToolAllowedConfig(cfg) {
			if addendum := strings.TrimSpace(cfg.OnDemandSubagentsSystemAddendum); addendum != "" {
				addenda = append(addenda, addendum)
			}
		}
		return addenda
	}

	if addendum := strings.TrimSpace(cfg.AutoSubagentsSystemAddendum); addendum != "" {
		addenda = append(addenda, addendum)
	}
	return addenda
}
func removeLastAutoSubagentsAddendum(system, addendum string) (string, bool) {
	idx := strings.LastIndex(system, addendum)
	if idx < 0 {
		return system, false
	}
	return system[:idx] + system[idx+len(addendum):], true
}
func (i *Interactive) applyAutoSubagentsSystemPrompt(orchestrating bool) {
	// i.mu is sufficient here: this path updates the existing agent's system
	// prompt and managed addenda in place; it does not replace the agent or
	// its tool registry, so agentMu is not required.
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.agent == nil {
		return
	}

	sys, _ := i.agent.PromptConfig()
	changed := false
	if guidance := strings.TrimSpace(i.cfg.WritingGuidance); guidance != "" {
		var removed bool
		sys, removed = removeLastAutoSubagentsAddendum(sys, guidance)
		changed = changed || removed
	}
	for idx := len(i.managedAutoSubagentsAddenda) - 1; idx >= 0; idx-- {
		var removed bool
		sys, removed = removeLastAutoSubagentsAddendum(sys, i.managedAutoSubagentsAddenda[idx])
		changed = changed || removed
	}
	i.managedAutoSubagentsAddenda = nil

	addenda := autoSubagentsAddenda(i.cfg, orchestrating)
	if changed {
		sys = strings.TrimRight(sys, "\n")
	}
	for _, addendum := range addenda {
		if sys != "" {
			sys += "\n\n"
		}
		sys += addendum
		i.managedAutoSubagentsAddenda = append(i.managedAutoSubagentsAddenda, addendum)
		changed = true
	}
	if guidance := strings.TrimSpace(i.cfg.WritingGuidance); guidance != "" {
		if sys != "" {
			sys += "\n\n"
		}
		sys += guidance
		changed = true
	}
	if !changed {
		return
	}
	i.agent.SetSystemPrompt(sys)
}
func (i *Interactive) autoSubagentsAvailable() bool {
	return i.cfg.ResidentManager != nil && autoSubagentsAnyToolAllowedConfig(i.cfg)
}
func (i *Interactive) autoSubagentsUnavailableHint() string {
	var hints []string
	if i.cfg.ResidentManager == nil {
		hints = append(hints, "resident subagent manager is unavailable in this mode")
	}
	if !autoSubagentsAnyToolAllowedConfig(i.cfg) {
		hints = append(hints, "launch-time tool policy excludes subagent manager tools")
	}
	return strings.Join(hints, "; ")
}
func autoSubagentsToolAllowedConfig(cfg InteractiveConfig) bool {
	return cfg.AutoSubagentsToolAllowed == nil || *cfg.AutoSubagentsToolAllowed
}
func autoSubagentsStatusToolAllowedConfig(cfg InteractiveConfig) bool {
	if cfg.AutoSubagentsStatusToolAllowed != nil {
		return *cfg.AutoSubagentsStatusToolAllowed
	}
	// Preserve the old single-boolean embedding contract: when callers have
	// not supplied the new field, status follows the existing delegation gate.
	return autoSubagentsToolAllowedConfig(cfg)
}
func autoSubagentsStopToolAllowedConfig(cfg InteractiveConfig) bool {
	if cfg.AutoSubagentsStopToolAllowed != nil {
		return *cfg.AutoSubagentsStopToolAllowed
	}
	return autoSubagentsToolAllowedConfig(cfg)
}
func autoSubagentsResumeToolAllowedConfig(cfg InteractiveConfig) bool {
	if cfg.AutoSubagentsResumeToolAllowed != nil {
		return *cfg.AutoSubagentsResumeToolAllowed
	}
	return autoSubagentsToolAllowedConfig(cfg)
}
func autoSubagentsAnyToolAllowedConfig(cfg InteractiveConfig) bool {
	return autoSubagentsToolAllowedConfig(cfg) ||
		autoSubagentsStatusToolAllowedConfig(cfg) ||
		autoSubagentsStopToolAllowedConfig(cfg) ||
		autoSubagentsResumeToolAllowedConfig(cfg)
}
func (i *Interactive) autoSubagentsToolAllowed() bool {
	return autoSubagentsToolAllowedConfig(i.cfg)
}
func (i *Interactive) autoSubagentsStatusToolAllowed() bool {
	return autoSubagentsStatusToolAllowedConfig(i.cfg)
}
func (i *Interactive) autoSubagentsStopToolAllowed() bool {
	return autoSubagentsStopToolAllowedConfig(i.cfg)
}
func (i *Interactive) autoSubagentsResumeToolAllowed() bool {
	return autoSubagentsResumeToolAllowedConfig(i.cfg)
}
func (i *Interactive) autoSubagentsEnabledLocked() bool {
	return i.cfg.AutoSubagentsEnabled != nil && *i.cfg.AutoSubagentsEnabled && i.autoSubagentsAvailable()
}
func (i *Interactive) applyAutoSubagentsTool() {
	i.agentMu.Lock()
	defer i.agentMu.Unlock()
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.agent == nil {
		return
	}
	current := i.agent.ToolsSnapshot()
	next := core.Registry{}
	for name, t := range current {
		if name == "subagent_spawn" || name == "subagent_status" || name == "subagent_stop" || name == "subagent_resume" {
			continue
		}
		next[name] = t
	}
	if i.autoSubagentsAvailable() {
		if i.autoSubagentsToolAllowed() {
			canonical := &tools.SubagentSpawnTool{
				ResidentManager:   i.cfg.ResidentManager,
				BuildResidentSpec: i.cfg.BuildResidentSpec,
				Enabled:           func() bool { return true },
				DefaultModel:      func() string { return i.cfg.Model },
				DefaultProvider:   func() string { return i.cfg.Provider },
				DefaultReasoning:  func() string { return i.cfg.Reasoning },
				ResolveSubagent:   i.cfg.ResolveSubagent,
			}
			next[canonical.Name()] = canonical
		}
		if i.autoSubagentsStatusToolAllowed() {
			statusTool := &tools.SubagentStatusTool{
				ResidentManager: i.cfg.ResidentManager,
				Enabled:         func() bool { return true },
			}
			next[statusTool.Name()] = statusTool
		}
		if i.autoSubagentsStopToolAllowed() {
			stopTool := &tools.SubagentStopTool{
				ResidentManager: i.cfg.ResidentManager,
				Enabled:         func() bool { return true },
			}
			next[stopTool.Name()] = stopTool
		}
		if i.autoSubagentsResumeToolAllowed() {
			resumeTool := &tools.SubagentResumeTool{
				ResidentManager: i.cfg.ResidentManager,
				Enabled:         func() bool { return true },
			}
			next[resumeTool.Name()] = resumeTool
		}
	}
	i.agent.SetTools(next)
}
func (i *Interactive) applyTelegramTools(active bool) {
	if active {
		// The child ceiling must be in place even when no live main agent exists,
		// and before Bridge.Start can accept an external prompt.
		i.setWebSearchAvailable(false)
	}
	i.agentMu.Lock()
	defer i.agentMu.Unlock()
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.agent == nil {
		return
	}
	current := i.agent.ToolsSnapshot()
	next := core.Registry{}
	for name, t := range current {
		if name == "telegram_send_image" || name == "telegram_send_file" {
			continue
		}
		if active && name == "web_search" {
			// External Telegram prompts have no per-request confirmation
			// surface, so V1 does not expose native web search while paired.
			continue
		}
		next[name] = t
	}
	if active {
		sender := telegramSenderAdapter{bridge: i.telegramBridge}
		next["telegram_send_image"] = &tools.TelegramSendImageTool{
			CWD: i.cfg.CWD, Sandbox: i.cfg.Sandbox, Sender: sender,
		}
		next["telegram_send_file"] = &tools.TelegramSendFileTool{
			CWD: i.cfg.CWD, Sandbox: i.cfg.Sandbox, Sender: sender,
		}
	}
	i.agent.SetTools(next)
	_, webSearchAvailable := next["web_search"]
	i.setWebSearchAvailable(webSearchAvailable && !active)
}
func (i *Interactive) telegramStatus() {
	var msg string
	if i.telegramBridge != nil && i.telegramBridge.Active() {
		s := i.telegramBridge.State()
		msg = "telegram: connected (tui bridge)"
		if s.Username != "" {
			msg += " as @" + s.Username
		}
		if s.PairedID != 0 {
			msg += fmt.Sprintf(" - paired with user %d", s.PairedID)
		} else {
			msg += " - awaiting pairing"
		}
	} else if pid, alive, _ := telegram.IsRunning(i.cfg.ZutHome); alive && pid > 0 {
		msg = fmt.Sprintf("telegram: background daemon running (pid %d) - /telegram connect won't work until you stop it", pid)
	} else {
		cfg, _ := telegram.LoadConfig(i.cfg.ZutHome)
		if cfg.BotToken == "" {
			msg = "telegram: not configured. run `zut telegram-bot setup` first."
		} else {
			msg = "telegram: disconnected"
			if cfg.BotUsername != "" {
				msg += " (@" + cfg.BotUsername + " ready to connect)"
			}
		}
	}
	i.mu.Lock()
	i.statusOK = msg
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
}
func (h *telegramHost) SubmitOrQueue(prompt string, images []provider.ImageBlock) {
	h.iv.SubmitOrQueue(prompt, images)
}
func (h *telegramHost) CancelTurn() { h.iv.CancelTurn() }
func (h *telegramHost) Status() string {
	h.iv.mu.Lock()
	providerName := h.iv.cfg.Provider
	model := h.iv.cfg.Model
	cwd := h.iv.cfg.CWD
	usage := h.iv.cumUsage
	subscription := h.iv.cfg.AuthMethod == "oauth"
	ctxUsed := h.iv.lastCtxInput
	busy := h.iv.busy
	queued := len(h.iv.queued)
	h.iv.mu.Unlock()

	ctxMax := 0
	if m, err := provider.FindModel(providerName, model); err == nil {
		ctxMax = m.ContextWindow
	}
	return telegram.FormatStatus(telegram.StatusSnapshot{
		Provider:     providerName,
		Model:        model,
		CWD:          cwd,
		Usage:        usage,
		Subscription: subscription,
		ContextUsed:  ctxUsed,
		ContextMax:   ctxMax,
		Busy:         busy,
		Queued:       queued,
	})
}
func (h *telegramHost) Notify(level, message string) {
	h.iv.mu.Lock()
	switch level {
	case "error", "warn":
		h.iv.statusErr = message
		h.iv.statusOK = ""
	default:
		h.iv.statusOK = message
		h.iv.statusErr = ""
	}
	h.iv.mu.Unlock()
	h.iv.invalidate()
}
func (i *Interactive) openSessionOpsDialog() {
	items := []sessionOpsItem{
		{label: "export", action: "export", hint: "write the current session to a .zutsession file"},
		{label: "import", action: "import", hint: "load a .zutsession file into this directory"},
		{label: "fork", action: "fork", hint: "branch from a past user message into a new session"},
		{label: "tree", action: "tree", hint: "switch between branches in this directory"},
	}
	i.sessionOpsDialog.Open(items)
	i.invalidate()
}
func (i *Interactive) doSessionOp(action, arg string) {
	switch action {
	case "export":
		i.doSessionExport(arg)
	case "import":
		i.doSessionImport(arg)
	case "fork":
		i.doSessionFork()
	case "tree":
		i.doSessionTree()
	default:
		i.mu.Lock()
		i.statusErr = "unknown /session action: " + action + " (use export, import, fork, or tree)"
		i.mu.Unlock()
		i.invalidate()
	}
}
func (i *Interactive) doSessionExport(dst string) {
	src := i.currentSessionPath()
	if src == "" {
		i.mu.Lock()
		i.statusErr = "export: no session is active (running with --no-session?)"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	// Persist any in-memory agent messages to the session file so
	// the export carries the full conversation. Without this, the
	// default lazy-flush-at-exit strategy leaves most of a running
	// session unwritten and the export ends up with only the meta.
	if i.cfg.FlushSession != nil {
		i.cfg.FlushSession()
	}
	dst = unquotePath(dst)
	if dst == "" {
		dst = defaultExportDir()
	} else {
		dst = expandTilde(dst)
	}
	out, err := core.ExportSession(src, dst)
	if err != nil {
		i.mu.Lock()
		i.statusErr = "export: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.mu.Lock()
	i.statusOK = "exported session to " + friendlyPath(out)
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
}
func (i *Interactive) doSessionImport(src string) {
	src = unquotePath(src)
	if src == "" {
		i.mu.Lock()
		i.statusErr = "import: pass a path — e.g. /session import ~/Downloads/work.zutsession"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	src = expandTilde(src)
	if _, err := os.Stat(src); err != nil {
		i.mu.Lock()
		i.statusErr = "import: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	newPath, err := core.ImportSession(src, i.sessionsRoot(), i.cfg.CWD, i.cfg.Version)
	if err != nil {
		i.mu.Lock()
		i.statusErr = "import: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if i.cfg.LoadSession == nil {
		i.mu.Lock()
		i.statusOK = "imported session at " + friendlyPath(newPath) + " (run /sessions to resume it)"
		i.statusErr = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.markSessionTitleSwitching()
	if err := i.cfg.LoadSession(newPath); err != nil {
		i.restoreFailedSessionTitle()
		i.mu.Lock()
		i.statusErr = "import: load failed: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.restoreLoadedSessionTitle()
	state := i.restoreCurrentCompactHandoff()
	i.mu.Lock()
	i.statusOK = "imported and switched to session " + friendlyPath(newPath)
	i.statusErr = ""
	if i.agent != nil {
		i.view.Messages = filterHiddenTranscriptMessages(i.agent.Messages())
		i.cumUsage = i.agent.Cost()
		last := i.agent.LastTurnUsage()
		i.lastCtxInput = last.InputTokens + last.CacheReadTokens + last.CacheWriteTokens
		if len(i.view.Messages) > initialResumeTailLimit {
			i.view.TailLimit = initialResumeTailLimit
		} else {
			i.view.TailLimit = 0
		}
		i.view.InvalidateRenderCache()
	}
	i.mu.Unlock()
	i.invalidate()
	if state.reason != compactContinuationNone {
		i.startRestoredCompactHandoff(i.runCtx)
	}
}
func defaultExportDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return os.TempDir()
	}
	downloads := filepath.Join(home, "Downloads")
	if fi, err := os.Stat(downloads); err == nil && fi.IsDir() {
		return downloads
	}
	return home
}
func expandTilde(p string) string {
	if p == "" || p[0] != '~' {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if len(p) == 1 {
		return home
	}
	if p[1] == '/' || p[1] == filepath.Separator {
		return filepath.Join(home, p[2:])
	}
	return p
}
func unquotePath(p string) string {
	p = strings.TrimSpace(p)
	if len(p) >= 2 {
		first := p[0]
		last := p[len(p)-1]
		if (first == '\'' && last == '\'') || (first == '"' && last == '"') {
			p = p[1 : len(p)-1]
		}
	}
	return p
}
func friendlyPath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + p[len(home):]
	}
	return p
}
func (i *Interactive) doSessionFork() {
	path := i.currentSessionPath()
	if path == "" {
		i.mu.Lock()
		i.statusErr = "fork: no session is active (running with --no-session?)"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.mu.Lock()
	busy := i.busy || i.shellRunning || i.compacting || i.autoCompacting || i.sessionLoading
	queued := len(i.queued) != 0
	ag := i.agent
	i.mu.Unlock()
	if busy || queued || ag == nil || ag.QueuedMessageCount() != 0 {
		i.mu.Lock()
		i.statusErr = "fork: wait until the current turn and queued messages finish"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if i.cfg.FlushSession != nil {
		i.cfg.FlushSession()
	}
	snapshot, err := core.ReadSessionSnapshot(path)
	if err != nil {
		i.mu.Lock()
		i.statusErr = "fork: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	msgs := snapshot.Messages
	if len(msgs) == 0 {
		i.mu.Lock()
		i.statusErr = "fork: transcript is empty; nothing to fork from"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.pendingFork = true
	i.jumpDialog.Open(msgs, "")
	i.invalidate()
}
func (i *Interactive) doSessionTree() {
	i.openSessionTree()
}
func (i *Interactive) applySessionTreeMessageSelection(src string, msgIdx, turnNo int, role provider.Role, prompt string) {
	target := sessionTreeTarget{
		SourcePath:        src,
		EffectiveIndex:    msgIdx,
		SelectionBoundary: msgIdx + 1,
		Role:              role,
		UserDraft:         prompt,
		Boundary:          sessionTreeMessageBoundary,
	}
	if role == provider.RoleUser {
		target.SelectionBoundary = msgIdx
	}
	i.applySessionTreeTarget(target, turnNo)
}
func (i *Interactive) applySessionTreeTarget(target sessionTreeTarget, turnNo int) {
	// Selection is a state-changing operation too. Use the selection gate so
	// a queued/busy turn cannot race the hidden branch or accidentally submit
	// a provider turn while the session is being swapped. The open gate cannot
	// be reused here because the tree dialog itself owns the keyboard.
	if !i.canCommitSessionTreeSelection() {
		i.setSessionTreeError("tree: session branching is not available in this build")
		return
	}
	src := target.SourcePath
	if src == "" {
		src = i.currentSessionPath()
	}
	if src == "" {
		i.setSessionTreeError("tree: no session is active")
		return
	}

	// Read and validate the selected row before flushing or creating anything.
	// This gives tool-call/result pairs an atomic boundary and prevents a
	// malformed selection from changing the active session. Historical rows
	// come from a pre-compaction segment; current rows use the effective
	// snapshot that the running agent resumes from.
	var msgs []provider.Message
	var historicalSegment core.SessionHistorySegment
	if target.Historical {
		history, err := core.ReadSessionHistory(src)
		if err != nil {
			i.setSessionTreeError("tree: read selection: " + err.Error())
			return
		}
		if target.HistorySegment < 0 || target.HistorySegment >= len(history.Segments) {
			i.setSessionTreeError("tree: selected history segment is unavailable")
			return
		}
		historicalSegment = history.Segments[target.HistorySegment]
		msgs = historicalSegment.Messages
	} else {
		sess, current, err := core.OpenSession(src)
		if err != nil {
			i.setSessionTreeError("tree: read selection: " + err.Error())
			return
		}
		if err := sess.Close(); err != nil {
			i.setSessionTreeError("tree: close selection: " + err.Error())
			return
		}
		msgs = current
	}
	selection, err := sessionTreeSelection(msgs, target)
	if err != nil {
		i.setSessionTreeError("tree: " + err.Error())
		return
	}
	if i.cfg.FlushSession != nil {
		i.cfg.FlushSession()
	}
	var newPath string
	if target.Historical {
		newPath, err = core.BranchSessionHiddenFromHistory(src, i.sessionsRoot(), i.cfg.CWD, i.cfg.Version, historicalSegment, selection.upTo)
	} else {
		newPath, err = core.BranchSessionHidden(src, i.sessionsRoot(), i.cfg.CWD, i.cfg.Version, selection.upTo)
	}
	if err != nil {
		i.setSessionTreeError("tree: " + err.Error())
		return
	}
	i.markSessionTitleSwitching()
	if err := i.cfg.LoadSession(newPath); err != nil {
		i.restoreFailedSessionTitle()
		i.setSessionTreeError("tree: checkout failed: " + err.Error())
		return
	}
	state := i.restoreCurrentCompactHandoff()

	// LoadSession is intentionally the old func(string) error callback: the
	// CLI and embedders already own agent/session swapping. Refresh the view
	// from the swapped agent here instead of requiring a new callback result.
	i.resetSessionTitleForFreshBranch()
	i.scrollToBottom()
	i.mu.Lock()
	i.lastCtxInput = 0
	if i.agent != nil {
		i.view.Messages = filterHiddenTranscriptMessages(i.agent.Messages())
		i.cumUsage = i.agent.Cost()
		last := i.agent.LastTurnUsage()
		i.lastCtxInput = last.InputTokens + last.CacheReadTokens + last.CacheWriteTokens
		i.view.InvalidateRenderCache()
	}
	i.toolCalls = map[string]*tui.ToolCallView{}
	i.toolOrder = nil
	i.toolGate = map[string]int{}
	i.extNotes = nil
	if selection.restoreDraft {
		i.ed.SetValue(selection.draftText)
		i.clipboardImages = selection.images
		i.statusOK = fmt.Sprintf("checked out before turn %d; edit and send to branch", turnNo)
	} else {
		i.ed.Clear()
		i.clipboardImages = nil
		i.statusOK = fmt.Sprintf("checked out turn %d into a new branch", turnNo)
	}
	i.statusErr = ""
	i.mu.Unlock()
	i.scrollToBottom()
	// Selection does not submit, start, or queue the restored user draft; only
	// a later explicit Enter does that. An inherited compact handoff may resume
	// its hidden continuation below when this branch has the complete effective
	// transcript, without submitting the restored draft.
	i.invalidate()
	if state.reason != compactContinuationNone {
		i.startRestoredCompactHandoff(i.runCtx)
	}
}
func (i *Interactive) applyForkSelection(msgIdx int) {
	i.pendingFork = false
	src := i.currentSessionPath()
	if src == "" {
		i.mu.Lock()
		i.statusErr = "fork: no session is active"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if i.cfg.FlushSession != nil {
		i.cfg.FlushSession()
	}
	// msgIdx is 0-indexed message position; copy msgIdx+1 rows so
	// the selected user message is included.
	upTo := msgIdx + 1
	newPath, err := core.BranchSession(src, i.sessionsRoot(), i.cfg.CWD, i.cfg.Version, upTo)
	if err != nil {
		i.mu.Lock()
		i.statusErr = "fork: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if i.cfg.LoadSession == nil {
		i.mu.Lock()
		i.statusOK = "forked at message " + formatInt(upTo) + " (run /sessions to resume)"
		i.statusErr = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.markSessionTitleSwitching()
	if err := i.cfg.LoadSession(newPath); err != nil {
		i.restoreFailedSessionTitle()
		i.mu.Lock()
		i.statusErr = "fork: switch failed: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	state := i.restoreCurrentCompactHandoff()
	i.resetSessionTitleForFreshBranch()
	i.mu.Lock()
	i.statusOK = "forked and switched to new branch at " + friendlyPath(newPath)
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
	if state.reason != compactContinuationNone {
		i.startRestoredCompactHandoff(i.runCtx)
	}
}
func formatInt(n int) string {
	return fmt.Sprintf("%d", n)
}
func assistantText(m provider.Message) string {
	var sb strings.Builder
	for _, c := range m.Content {
		if tb, ok := c.(provider.TextBlock); ok {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}
func (i *Interactive) resetTranscriptRenderLocked() {
	i.view.InvalidateRenderCache()
	i.chatCacheValid = false
	i.prevChatLen = 0
	i.prevChatCols = 0
	i.prevChatRows = 0
	i.prevScrollOffset = 0
	i.requestRendererInvalidate()
}
func (i *Interactive) resetStreamingStateLocked() {
	i.streaming.Reset()
	i.streamPending = i.streamPending[:0]
	i.streamFlushPending = false
	i.streamOn = false
	i.openAllToolGatesLocked()
}
func (i *Interactive) openAllToolGatesLocked() {
	for id := range i.toolGate {
		i.toolGate[id] = 0
	}
}
func (i *Interactive) gateToolLocked(id string) {
	if _, ok := i.toolGate[id]; ok {
		return
	}
	if !i.streamOn {
		i.toolGate[id] = 0
		return
	}
	i.toolGate[id] = i.streaming.Len() + len(i.streamPending)
}
func (i *Interactive) toolGateOpenLocked(id string) bool {
	gate, ok := i.toolGate[id]
	if !ok || gate == 0 {
		return true
	}
	return i.streaming.Len() >= gate
}
func (i *Interactive) assistantMessageSideEffects(m provider.Message) {
	if i.cfg.OnAssistant != nil {
		i.cfg.OnAssistant(m)
	}
	if i.telegramBridge != nil && i.telegramBridge.Active() {
		var sb strings.Builder
		for _, c := range m.Content {
			if tb, ok := c.(provider.TextBlock); ok {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(tb.Text)
			}
		}
		if text := sb.String(); strings.TrimSpace(text) != "" {
			go i.telegramBridge.OnAssistantText(text)
		}
	}
}
func (i *Interactive) runStreamPacer(ctx context.Context) {
	t := time.NewTicker(paintPaceInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			i.mu.Lock()
			if len(i.streamPending) == 0 {
				// EvAssistantMessage already fired but the pacer
				// was still draining a tick ago. Everything is now
				// painted; clear the streaming flags so the next
				// redraw shows the finalised transcript message
				// and hides the streaming overlay.
				if i.streamFlushPending {
					i.streamFlushPending = false
					i.streaming.Reset()
					i.streamOn = false
					i.openAllToolGatesLocked()
					i.mu.Unlock()
					i.invalidate()
					continue
				}
				i.mu.Unlock()
				continue
			}
			n := paintPaceRate
			if n > len(i.streamPending) {
				n = len(i.streamPending)
			}
			i.streaming.WriteString(string(i.streamPending[:n]))
			i.streamPending = i.streamPending[n:]
			i.mu.Unlock()
			i.invalidate()
		}
	}
}
