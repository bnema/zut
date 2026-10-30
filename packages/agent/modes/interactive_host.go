package modes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/bnema/zut/packages/agent/session"
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
		i.portSubmit(i.runCtx, resp.Prompt, nil)
	case "insert":
		if err := i.queueEditorInsert(resp.Insert); err != nil {
			i.mu.Lock()
			i.statusErr = "extension /" + name + ": " + err.Error()
			i.statusOK = ""
			i.mu.Unlock()
			i.invalidate()
		}
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

	// The new cwd starts a fresh conversation: discover its skills and load its
	// pins before taking any lock. The host has already switched its own cwd.
	i.skillPins.ioMu.Lock()
	defer i.skillPins.ioMu.Unlock()
	pinFetch := i.skillPins.fetch(cwd, true)

	i.agentMu.Lock()
	defer i.agentMu.Unlock()
	i.mu.Lock()
	i.prepareReplacementAgentLocked(ag)
	i.agent = ag
	i.cfg.CWD = cwd
	i.cfg.SubagentsSystemAddendum = subagentsAddendum
	i.managedAutoSubagentsAddenda = autoSubagentsAddenda(i.cfg, i.autoSubagentsEnabledLocked())
	i.cfg.StartupContextPaths = append([]string(nil), startupContextPaths...)
	i.view.StartupContextPaths = nil
	i.armSkillPinsLocked(pinFetch)
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
	titleCancel := i.resetSessionTitleLocked()
	i.toolCalls = map[string]*tui.ToolCallView{}
	i.toolOrder = nil
	i.stream.Reset()
	i.helpBlock = nil
	i.sessionInfoBlocks = nil
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

func (i *Interactive) submitOrQueue(text string, images []provider.ImageBlock, userInput bool) {
	i.submitOrQueueMessage(core.QueuedMessage{Text: text, Images: images}, userInput)
}
func (i *Interactive) submitOrQueueMessage(message core.QueuedMessage, userInput bool) {
	if cmd, ok := shellEscapeCommand(message.Text); ok {
		i.startShellEscape(i.runCtx, cmd)
		return
	}
	origin := session.InputOriginResident
	if userInput {
		origin = session.InputOriginUser
	}
	i.portSubmitInput(i.runCtx, session.Input{Text: message.Text, Images: message.Images, HostEvent: message.HostEvent, Mode: session.SubmitModeAuto, Origin: origin})
}

// popQueuedUserMessageLocked leaves host reports queued for the agent.
// The caller must hold i.mu, as with the queue's enqueue and teardown paths.
func (i *Interactive) popQueuedUserMessageLocked() (core.QueuedMessage, bool) {
	if i.agent != nil {
		if message, ok := i.agent.PopQueuedUserMessage(); ok {
			return message, true
		}
	}
	for index := len(i.queued) - 1; index >= 0; index-- {
		if i.queued[index].HostEvent {
			continue
		}
		message := i.queued[index]
		copy(i.queued[index:], i.queued[index+1:])
		i.queued[len(i.queued)-1] = core.QueuedMessage{}
		i.queued = i.queued[:len(i.queued)-1]
		return message, true
	}
	return core.QueuedMessage{}, false
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

		cancel()
		i.confirmDialog.CancelAll("turn cancelled")
	}
}

// Insert is the extension host hook. It may be called from any goroutine,
// so the edit is applied on the Run loop. The HostHooks contract has no
// error return; a full queue is reported in the status line instead.
func (i *Interactive) Insert(text string) {
	if err := i.queueEditorInsert(text); err != nil {
		i.ReportError(fmt.Errorf("extension insert: %w", err))
	}
}

// loopCallsCap bounds pending off-loop editor mutations.
const loopCallsCap = 32

// errLoopCallsFull is returned when the Run loop has too many pending calls.
var errLoopCallsFull = errors.New("input loop busy, insert dropped")

// queueEditorInsert schedules text to be inserted at the cursor by the Run
// loop, which owns i.ed. It never blocks.
func (i *Interactive) queueEditorInsert(text string) error {
	select {
	case i.loopCalls <- func() {
		i.ed.Insert(text)
		i.invalidate()
	}:
		return nil
	default:
		return errLoopCallsFull
	}
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
