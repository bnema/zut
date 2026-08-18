package modes

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/bnema/zut/packages/agent/internal/orchestration"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

func (i *Interactive) invokeExtensionCommand(ctx context.Context, name, args string) {
	resp, err := i.cfg.Extensions.Invoke(ctx, name, args, 30*time.Second)
	if err != nil {
		i.mu.Lock()
		i.statusErr = "extension /" + name + ": " + err.Error()
		i.statusOK = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if resp.Error != "" {
		i.mu.Lock()
		i.statusErr = "extension /" + name + ": " + resp.Error
		i.statusOK = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	switch resp.Action {
	case "open_panel":
		if resp.OpenPanel != nil {
			extName := name
			if i.cfg.Extensions != nil {
				if owner := i.cfg.Extensions.CommandOwner(name); owner != "" {
					extName = owner
				}
			}
			i.OpenPanel(extName, *resp.OpenPanel)
		}
	case "prompt":
		if strings.TrimSpace(resp.Prompt) == "" {
			return
		}
		i.startTurn(i.runCtx, resp.Prompt)
	case "insert":
		i.ed.Insert(resp.Insert)
		i.invalidate()
	case "display":
		i.appendExtensionNote(name, resp.Display, "info")
	case "noop", "":
		// nothing
	default:
		i.mu.Lock()
		i.statusErr = "extension /" + name + ": unknown action " + resp.Action
		i.mu.Unlock()
		i.invalidate()
	}
}
func (i *Interactive) appendExtensionNote(extName, msg, level string) {
	if msg == "" {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	color := i.cfg.Theme.Muted
	switch level {
	case "warn":
		color = i.cfg.Theme.Warning
	case "error":
		color = i.cfg.Theme.Error
	case "success":
		color = i.cfg.Theme.Tool
	}
	prefix := i.cfg.Theme.FGColor(i.cfg.Theme.Accent, "["+extName+"] ")
	for _, line := range strings.Split(msg, "\n") {
		i.statusOK = "" // clear any stale ok
		i.statusErr = ""
		i.extNotes = append(i.extNotes, prefix+i.cfg.Theme.FGColor(color, line))
	}
	// Extension slash commands complete asynchronously. If their result is
	// displayed while confirmation is waiting, keep Esc assigned to the note
	// until it is dismissed rather than letting it answer the tool call.
	i.confirmDialog.Blur()
}
func (i *Interactive) ApplySessionAgent(ag *core.Agent, providerName, model string) {
	i.ApplySessionAgentWithCompactHandoff(ag, providerName, model, nil)
}
func (i *Interactive) ApplySessionAgentWithCompactHandoff(ag *core.Agent, providerName, model string, compactHandoff json.RawMessage) {
	if ag == nil {
		return
	}
	i.agentMu.Lock()
	defer i.agentMu.Unlock()
	i.mu.Lock()
	i.prepareReplacementAgentLocked(ag)
	i.compactContinuation = decodeCompactHandoff(compactHandoff)
	i.agent = ag
	i.cfg.Provider = providerName
	i.cfg.Model = model
	i.view.Messages = filterHiddenTranscriptMessages(ag.Messages())
	i.cumUsage = ag.Cost()
	last := ag.LastTurnUsage()
	i.lastCtxInput = last.InputTokens + last.CacheReadTokens + last.CacheWriteTokens
	if len(i.view.Messages) > initialResumeTailLimit {
		i.view.TailLimit = initialResumeTailLimit
	} else {
		i.view.TailLimit = 0
	}
	i.view.InvalidateRenderCache()
	i.toolCalls = map[string]*tui.ToolCallView{}
	i.toolOrder = nil
	i.toolGate = map[string]int{}
	i.helpBlock = nil
	i.sessionInfoBlock = nil
	i.extNotes = nil
	i.parkedTurn = 0
	i.parkedTotal = 0
	i.mu.Unlock()
	i.invalidate()
}
func (i *Interactive) SetSubagentSessionScope(_ string) {
	i.invalidate()
}
func (i *Interactive) ApplyChangedCWD(ag *core.Agent, provider, model, cwd string) {
	i.applyChangedCWD(ag, provider, model, cwd, nil)
}
func (i *Interactive) ApplyChangedCWDWithStartupContext(ag *core.Agent, provider, model, cwd string, startupContextPaths []string) {
	i.applyChangedCWD(ag, provider, model, cwd, startupContextPaths)
}
func (i *Interactive) applyChangedCWD(ag *core.Agent, provider, model, cwd string, startupContextPaths []string) {
	home, _ := os.UserHomeDir()
	profiles, _ := subagents.Discover(cwd, home)
	subagentsAddendum := subagents.SystemPromptAddendum(profiles)

	i.agentMu.Lock()
	defer i.agentMu.Unlock()
	i.mu.Lock()
	i.prepareReplacementAgentLocked(ag)
	i.resetCompactContinuationLocked()
	i.agent = ag
	i.cfg.CWD = cwd
	i.cfg.SubagentsSystemAddendum = subagentsAddendum
	i.managedAutoSubagentsAddenda = autoSubagentsAddenda(i.cfg, i.autoSubagentsEnabledLocked())
	i.cfg.StartupContextPaths = append([]string(nil), startupContextPaths...)
	i.view.StartupContextPaths = nil
	i.view.InvalidateRenderCache()
	if i.cfg.ShowInstructionsAtStartup != nil && *i.cfg.ShowInstructionsAtStartup {
		i.view.StartupContextPaths = append(i.view.StartupContextPaths, startupContextPaths...)
	}
	// Re-report the working directory to the terminal so "new tab here"
	// tracks the /cd change (OSC 7, see issue #38).
	if i.cfg.Terminal != nil {
		if seq := tui.ReportCWD(cwd); seq != "" {
			_, _ = i.cfg.Terminal.Write([]byte(seq))
		}
	}
	i.cfg.Provider = provider
	i.cfg.Model = model
	titleCancel := i.titleCancel
	i.titleCancel = nil
	i.titleVersion++
	i.sessionTitle = ""
	i.titleRealPromptSeen = false
	i.titleGenerationStarted = false
	i.writeTerminalTitleLocked("")
	i.toolCalls = map[string]*tui.ToolCallView{}
	i.toolOrder = nil
	i.toolGate = map[string]int{}
	i.helpBlock = nil
	i.sessionInfoBlock = nil
	i.parkedTurn = 0
	i.statusErr = ""
	i.mu.Unlock()
	if titleCancel != nil {
		titleCancel()
	}
	i.fileSuggest.Reset()
	i.fileSuggest.SetCWD(cwd)
	i.invalidate()
}
func (i *Interactive) SubmitSlash(text string) {
	i.mu.Lock()
	i.doubleEscape.Reset()
	i.mu.Unlock()
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return
	}
	if slashCommandCancelsTurn(text) {
		i.cancelAndWaitForIdle()
	}
	i.runSlash(i.runCtx, text)
	i.invalidate()
}
func (i *Interactive) SubmitOrQueue(text string, images []provider.ImageBlock) {
	i.submitOrQueue(text, images, true)
}

// SubmitFollowUp submits a scheduler-owned prompt without interpreting shell
// escapes or steering an active agent loop. A busy interactive waits for its
// current turn to end, then starts this prompt as a distinct follow-up turn.
func (i *Interactive) SubmitFollowUp(ctx context.Context, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("scheduled prompt is empty")
	}
	i.mu.Lock()
	if i.agent == nil {
		i.mu.Unlock()
		return fmt.Errorf("no agent running; log in first")
	}
	if i.busy {
		accepted := make(chan error, 1)
		i.scheduled = append(i.scheduled, scheduledFollowUp{text: text, accepted: accepted})
		i.mu.Unlock()
		i.invalidate()
		select {
		case err := <-accepted:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	i.mu.Unlock()
	i.startTurn(i.runCtx, text)
	return nil
}
func (i *Interactive) submitOrQueue(text string, images []provider.ImageBlock, userInput bool) {
	if cmd, ok := shellEscapeCommand(text); ok {
		i.startShellEscape(i.runCtx, cmd)
		return
	}
	i.mu.Lock()
	if i.agent == nil {
		i.statusErr = "not logged in. type /login first."
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.mu.Unlock()
	if userInput && i.cfg.EnsureMission != nil {
		if err := i.cfg.EnsureMission(text); err != nil {
			i.ReportError(fmt.Errorf("persist mission: %w", err))
			return
		}
	}
	// User input is an orchestration event, not merely a queue append. When
	// the manager is dormant behind a sealed worker wave, the coordinator makes
	// this the next manager turn and carries any completed worker summary with it.
	if userInput && i.coordinatorAcceptsUserInput() {
		actions := i.applyCoordinator(orchestration.Event{Kind: orchestration.EventUserInput, Text: text, Images: images})
		i.executeCoordinatorActions(actions)
		// The coordinator has accepted the input for its sealed worker wave.
		// It may intentionally defer an action until pending workers finish;
		// falling through would start a normal turn and discard that wave.
		return
	}
	i.maybeStartSessionTitle(i.runCtx, text)
	i.mu.Lock()
	if i.busy {
		// Keep the interactive mutex held through Agent.QueueMessage so turn teardown
		// cannot inspect the agent queue between the busy check and enqueue.
		// Compaction uses the host queue because it has no active agent loop
		// to drain Agent.QueueMessage entries.
		var handoff json.RawMessage
		var persistHandoff bool
		if i.agent != nil && !i.compacting {
			handoff, persistHandoff = i.resetCompactContinuationLocked()
			i.agent.QueueMessage(text, images)
		} else {
			handoff, persistHandoff = i.resetCompactContinuationLocked()
			i.queued = append(i.queued, core.QueuedMessage{Text: text, Images: images})
		}
		i.mu.Unlock()
		if persistHandoff {
			i.persistCompactHandoff(handoff)
		}
		i.invalidate()
		return
	}
	i.mu.Unlock()
	i.startTurnWithImages(i.runCtx, text, images)
}
func (i *Interactive) ChangelogVersion() string {
	if i.changelogDialog != nil {
		return i.changelogDialog.version
	}
	return ""
}
func (i *Interactive) CancelTurn() {
	i.mu.Lock()
	cancel := i.cancelTurn
	i.mu.Unlock()
	if cancel != nil {
		i.cancelCoordinator()

		i.mu.Lock()
		handoff, persistHandoff := i.resetCompactContinuationLocked()
		i.mu.Unlock()
		cancel()
		i.confirmDialog.CancelAll("turn cancelled")
		if persistHandoff {
			i.persistCompactHandoff(handoff)
		}
	}
}
func (i *Interactive) Insert(text string) {
	i.ed.Insert(text)
	i.invalidate()
}
func (i *Interactive) Display(extName, text string) {
	i.appendExtensionNote(extName, text, "info")
	i.invalidate()
}
func (i *Interactive) ReportError(err error) {
	if err == nil {
		return
	}
	i.mu.Lock()
	i.statusOK = ""
	i.statusErr = err.Error()
	i.mu.Unlock()
	i.invalidate()
}
func (i *Interactive) SetStatus(extName, key, level, text string) {
	if strings.TrimSpace(extName) == "" || strings.TrimSpace(key) == "" {
		return
	}
	i.mu.Lock()
	if i.extStatuses == nil {
		i.extStatuses = map[string]map[string]extensionStatus{}
	}
	if strings.TrimSpace(text) == "" {
		if items := i.extStatuses[extName]; items != nil {
			delete(items, key)
			if len(items) == 0 {
				delete(i.extStatuses, extName)
			}
		}
	} else {
		if i.extStatuses[extName] == nil {
			i.extStatuses[extName] = map[string]extensionStatus{}
		}
		i.extStatuses[extName][key] = extensionStatus{Level: level, Text: text}
	}
	i.mu.Unlock()
	i.invalidate()
}
func (i *Interactive) SetWidget(extName, id, position, title string, lines []string) {
	if strings.TrimSpace(extName) == "" || strings.TrimSpace(id) == "" {
		return
	}
	position = extproto.NormalizeWidgetPosition(position)
	i.mu.Lock()
	if i.extWidgets == nil {
		i.extWidgets = map[string]map[string]extensionWidget{}
	}
	if i.extWidgets[extName] == nil {
		i.extWidgets[extName] = map[string]extensionWidget{}
	}
	i.extWidgets[extName][id] = extensionWidget{
		Position: position,
		Title:    title,
		Lines:    append([]string(nil), lines...),
	}
	i.mu.Unlock()
	i.invalidate()
}
func (i *Interactive) ClearWidget(extName, id string) {
	i.mu.Lock()
	if items := i.extWidgets[extName]; items != nil {
		delete(items, id)
		if len(items) == 0 {
			delete(i.extWidgets, extName)
		}
	}
	i.mu.Unlock()
	i.invalidate()
}
func (i *Interactive) ClearExtensionChrome(extName string) {
	if strings.TrimSpace(extName) == "" {
		return
	}
	marker := "[" + extName + "] "
	i.mu.Lock()
	changed := false
	if _, ok := i.extStatuses[extName]; ok {
		delete(i.extStatuses, extName)
		changed = true
	}
	if _, ok := i.extWidgets[extName]; ok {
		delete(i.extWidgets, extName)
		changed = true
	}
	if len(i.extNotes) > 0 {
		kept := i.extNotes[:0:0]
		for _, line := range i.extNotes {
			if strings.Contains(line, marker) {
				changed = true
				continue
			}
			kept = append(kept, line)
		}
		i.extNotes = kept
	}
	if i.extPanel != nil && i.extPanel.Active() && i.extPanel.ext == extName {
		i.extPanel.Close()
		changed = true
	}
	i.mu.Unlock()
	if changed {
		i.invalidate()
	}
}
func (i *Interactive) OpenPanel(extName string, spec extproto.PanelSpec) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.extPanel.Open(extName, spec.ID, spec.Title, spec.Lines, spec.Footer)
	i.confirmDialog.Blur()
	if i.cfg.Extensions != nil {
		cols, rows := i.cfg.Terminal.Size()
		_ = cols
		_ = rows
	}
	i.invalidate()
}
func (i *Interactive) UpdatePanel(extName, panelID, title string, lines []string, footer string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.extPanel.Active() && i.extPanel.ext == extName && i.extPanel.id == panelID {
		i.extPanel.Update(title, lines, footer)
		i.invalidate()
	}
}
func (i *Interactive) ClosePanel(extName, panelID string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.extPanel.Active() && i.extPanel.ext == extName && i.extPanel.id == panelID {
		i.extPanel.Close()
		i.invalidate()
	}
}
