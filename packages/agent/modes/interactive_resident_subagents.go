package modes

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bnema/zut/packages/agent/modes/telegram"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

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

// residentInfoChangedLocked refreshes the open child view from session info
// events, the only trigger (no timer). The session publishes one coalesced
// info event per resident update, so a queued or running child reloads on
// each; otherwise only a changed summary (state transition) reloads. Reloads
// coalesce in the child session: at most one in flight plus one pending.
func (i *Interactive) residentInfoChangedLocked() {
	i.residentSubagentsDialog.Invalidate()
	child := i.residentChildSession
	if child == nil {
		return
	}
	summary, ok := residentSummaryFor(i.portInfo.Residents, child.childID)
	changed := child.SetSummary(summary, ok)
	active := ok && (summary.State == string(subagents.ResidentQueued) || summary.State == string(subagents.ResidentRunning))
	if changed || active {
		go i.reloadOpenResidentChildSession(child.childID)
	}
}

func residentSummaryFor(residents []session.ResidentSummary, childID string) (session.ResidentSummary, bool) {
	for _, r := range residents {
		if r.ID == childID {
			return r, true
		}
	}
	return session.ResidentSummary{}, false
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

// residents reports whether a session port (which owns the resident manager)
// is attached.
func autoSubagentsAddenda(cfg InteractiveConfig, residents, orchestrating bool) []string {
	addenda := make([]string, 0, 2)
	if residents && autoSubagentsToolAllowedConfig(cfg) {
		if addendum := strings.TrimSpace(cfg.SubagentsSystemAddendum); addendum != "" {
			addenda = append(addenda, addendum)
		}
	}
	if !orchestrating {
		if residents && autoSubagentsAnyToolAllowedConfig(cfg) {
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

	addenda := autoSubagentsAddenda(i.cfg, i.portMode.Load(), orchestrating)
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
	// The session behind the port owns the resident manager.
	return i.portMode.Load() && autoSubagentsAnyToolAllowedConfig(i.cfg)
}
func (i *Interactive) autoSubagentsUnavailableHint() string {
	var hints []string
	if !i.portMode.Load() {
		hints = append(hints, "resident subagents require an attached session")
	}
	if !autoSubagentsAnyToolAllowedConfig(i.cfg) {
		hints = append(hints, "launch-time tool policy excludes every subagent action")
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
func autoSubagentsInterruptToolAllowedConfig(cfg InteractiveConfig) bool {
	if cfg.AutoSubagentsInterruptToolAllowed != nil {
		return *cfg.AutoSubagentsInterruptToolAllowed
	}
	return autoSubagentsToolAllowedConfig(cfg)
}
func autoSubagentsAnyToolAllowedConfig(cfg InteractiveConfig) bool {
	return autoSubagentsToolAllowedConfig(cfg) ||
		autoSubagentsStatusToolAllowedConfig(cfg) ||
		autoSubagentsStopToolAllowedConfig(cfg) ||
		autoSubagentsResumeToolAllowedConfig(cfg) ||
		autoSubagentsInterruptToolAllowedConfig(cfg)
}
func (i *Interactive) autoSubagentsEnabledLocked() bool {
	return i.cfg.AutoSubagentsEnabled != nil && *i.cfg.AutoSubagentsEnabled && i.autoSubagentsAvailable()
}

// stripLocalSubagentTools removes any subagent tool from the local agent.
// Subagent tools run in the session behind the port, which owns the resident
// manager; a local agent never carries a resident facade.
func (i *Interactive) stripLocalSubagentTools() {
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
		if name == tools.SubagentToolName {
			continue
		}
		next[name] = t
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
		if active && tools.IsWebCapabilityName(name) {
			// External Telegram prompts have no per-request confirmation
			// surface, so V1 does not expose public-web navigation while paired.
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
		{label: "timeline", action: "timeline", hint: "inspect context, messages, and tool calls"},
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
	case "timeline":
		i.openTimeline()
	case "export":
		i.doSessionExport(arg)
	case "import":
		i.doSessionImport(arg)
	case "fork":
		// Same route as /fork: the daemon owns the transcript and queue, so
		// the local gate (which needs the legacy agent) would always refuse.
		if i.portMode.Load() {
			i.portOpenFork()
			break
		}
		i.doSessionFork()
	case "tree":
		i.doSessionTree()
	default:
		i.mu.Lock()
		i.statusErr = "unknown /session action: " + action + " (use timeline, export, import, fork, or tree)"
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
	busy := i.busy || i.shellRunning || i.autoCompacting || i.sessionLoading
	queued := i.hasQueuedInputLocked()
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
	if i.portMode.Load() {
		cwd := i.SessionInfo().CWD
		i.portSwitchWithTreeSelection("tree", func(ctx context.Context, client SessionClient) (session.ID, error) {
			fork := &session.ForkSpec{ParentPath: src, UpToMessage: selection.upTo, Hidden: true}
			if target.Historical {
				segment := target.HistorySegment
				fork.HistorySegment = &segment
			}
			created, err := client.Create(ctx, session.CreateSpec{CWD: cwd, Fork: fork})
			return session.ID(created.ID), err
		}, &selection)
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
	i.stream.Reset()
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
	// a later explicit Enter does that.
	i.invalidate()
}
func (i *Interactive) applyForkSelection(msgIdx int) {
	i.pendingFork = false
	if i.portMode.Load() {
		i.portForkSession(msgIdx + 1)
		return
	}
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
	i.resetSessionTitleForFreshBranch()
	i.mu.Lock()
	i.statusOK = "forked and switched to new branch at " + friendlyPath(newPath)
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
}
func formatInt(n int) string {
	return fmt.Sprintf("%d", n)
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
