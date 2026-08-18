package modes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

func (i *Interactive) armCtrlCExit() {
	i.mu.Lock()
	i.lastCtrlC = time.Now()
	i.mu.Unlock()
}
func (i *Interactive) ctrlCExitArmed() bool {
	i.mu.Lock()
	t := i.lastCtrlC
	i.mu.Unlock()
	return !t.IsZero() && time.Since(t) <= ctrlCExitWindow
}
func (i *Interactive) clearFileSuggestQuery() {
	val := i.ed.Value()
	if idx := strings.LastIndex(val, "@"); idx >= 0 {
		i.ed.SetValue(val[:idx+1])
	}
}
func (i *Interactive) setToolExpansion(expanded bool) {
	i.mu.Lock()
	i.view.ExpandAll = expanded
	i.requestRendererClear()
	i.mu.Unlock()
	i.invalidate()
}
func (i *Interactive) toggleToolExpansion() {
	i.mu.Lock()
	expanded := !i.view.ExpandAll
	i.mu.Unlock()
	i.setToolExpansion(expanded)
}
func (i *Interactive) toggleBtwToolExpansion() {
	i.btwDialog.ToggleToolExpansion()
	i.mu.Lock()
	i.requestRendererClear()
	i.mu.Unlock()
	i.invalidate()
}
func (i *Interactive) confirmChildActive() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.confirmChildActiveLocked()
}
func (i *Interactive) confirmChildActiveLocked() bool {
	return len(i.helpBlock) > 0 ||
		len(i.extNotes) > 0 ||
		i.dialog.Active() ||
		i.modelDialog.Active() ||
		i.llamaDialog.Active() ||
		i.rescueDialog.Active() ||
		i.sessionDialog.Active() ||
		i.residentSubagentsDialog.Active() ||
		i.residentChildSession != nil ||
		i.jumpDialog.Active() ||
		i.btwDialog.Active() ||
		i.skillsDialog.Active() ||
		i.changelogDialog.Active() ||
		i.logoutDialog.Active() ||
		i.telegramDialog.Active() ||
		i.settingsDialog.Active() ||
		i.sessionOpsDialog.Active() ||
		i.sessionTreeDialog.Active() ||
		i.extPanel.Active()
}
func (i *Interactive) restoreConfirmFocus() {
	if !i.confirmDialog.Active() || i.confirmDialog.Focused() {
		return
	}
	if !i.ed.IsEmpty() || i.confirmChildActive() {
		return
	}
	i.confirmDialog.Focus()
	i.invalidate()
}
func (i *Interactive) handleKey(ctx context.Context, k tui.Key) (done bool) {
	defer i.restoreConfirmFocus()

	var ok bool
	if k, ok = i.prepareInputKey(k); !ok {
		return false
	}

	// Dialogs consume keys while open (except ctrl+c, which always closes them).

	// Confirmation owns input by default. Pressing / deliberately moves
	// focus to the main editor without answering the pending request, so
	// slash commands and the dialogs they open can run first.
	if i.confirmDialog.Focused() {
		if k.Kind == tui.KeyRune && k.Rune == '/' {
			i.confirmDialog.Blur()
			i.ed.Insert("/")
			i.suggest.Reset()
			i.invalidate()
			return false
		}
		if k.Kind == tui.KeyCtrlO {
			if i.btwDialog != nil && i.btwDialog.Active() {
				i.toggleBtwToolExpansion()
			} else {
				i.toggleToolExpansion()
			}
			return false
		}
		i.confirmDialog.HandleKey(k)
		i.invalidate()
		return false
	}
	if i.confirmDialog.Active() && !i.confirmChildActive() && k.Kind == tui.KeyEsc {
		i.ed.Clear()
		i.suggest.Reset()
		i.fileSuggest.Reset()
		i.confirmDialog.Focus()
		i.invalidate()
		return false
	}
	if i.dialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.dialog.Close()
			if i.cfg.AuthManager != nil {
				i.cfg.AuthManager.CancelOAuth()
			}
			return false
		}
		act := i.dialog.HandleKey(k)
		if act.StartAPIKey {
			i.startAPIKeyFlow(act.Provider)
		}
		if act.StartOAuth {
			i.startOAuthFlow(act.Provider)
		}
		if act.StartManual {
			i.startManualOAuthFlow(act.Provider)
		}
		if act.SubmitCode != "" {
			i.submitManualOAuthCode(act.SubmitCode)
		}
		if act.SaveLlama {
			i.saveLlamaCPPLogin(act.LlamaURL, act.LlamaAPIKey)
		}
		return false
	}
	if i.modelDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.modelDialog.Close()
			i.quickModelAssign = 0
			return false
		}
		act := i.modelDialog.HandleKey(k)
		if act.Close {
			i.quickModelAssign = 0
		}
		if act.ReasoningChanged {
			i.applyReasoningSetting(act.Reasoning)
		}
		if act.Select {
			if i.quickModelAssign > 0 {
				i.applyQuickModelSelection(i.quickModelAssign, act.Provider, act.Model)
				i.quickModelAssign = 0
			} else {
				i.applyModelSelection(act.Provider, act.Model)
			}
		}
		return false
	}
	if i.llamaDialog.Active() {
		i.llamaDialog.HandleKey(k)
		i.invalidate()
		return false
	}
	if i.rescueDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.rescueDialog.Close()
			i.invalidate()
			return false
		}
		act := i.rescueDialog.HandleKey(k)
		if act.Select {
			i.applyRescueSelection(act.Provider, act.Model, act.Prompt)
		}
		i.invalidate()
		return false
	}
	i.mu.Lock()
	sessionDialogActive := i.sessionDialog.Active()
	i.mu.Unlock()
	if sessionDialogActive {
		if k.Kind == tui.KeyCtrlC {
			i.mu.Lock()
			i.sessionDialog.Close()
			i.sessionLoads = nil
			i.mu.Unlock()
			i.invalidate()
			return false
		}
		manualRenamePath := ""
		i.mu.Lock()
		if k.Kind == tui.KeyEnter && i.sessionDialog.renaming && core.NormalizeSessionTitle(i.sessionDialog.rename) != "" && i.sessionDialog.cursor >= 0 && i.sessionDialog.cursor < len(i.sessionDialog.sessions) && i.cfg.CurrentSessionPath != nil {
			manualRenamePath = i.sessionDialog.sessions[i.sessionDialog.cursor].Path
		}
		i.mu.Unlock()
		manualRenameCurrent := manualRenamePath != "" && i.cfg.CurrentSessionPath() == manualRenamePath
		if manualRenameCurrent {
			i.markSessionTitleSwitching()
		}
		i.mu.Lock()
		act := i.sessionDialog.HandleKey(k)
		if act.Select || act.Close {
			i.sessionLoads = nil
		}
		i.mu.Unlock()
		if act.Select {
			i.applySessionSelection(act.Path)
		}
		if act.Err != nil {
			if manualRenameCurrent {
				i.restoreFailedSessionTitle()
			}
			i.mu.Lock()
			i.statusErr = "rename: " + act.Err.Error()
			i.statusOK = ""
			i.mu.Unlock()
		} else if act.Renamed && act.Path != "" && i.cfg.CurrentSessionPath != nil && i.cfg.CurrentSessionPath() == act.Path {
			i.setManualSessionTitle(act.RenameTitle)
		}
		// Always request a redraw after handling a key here: when esc
		// closes the picker, the overlay-close detection in the render
		// pass needs to run so the tall dialog rows get repainted from
		// the chat (otherwise VS Code's retained scrollback leaves a
		// duplicate frame on screen).
		i.invalidate()
		return false
	}
	i.mu.Lock()
	residentDialog := i.residentSubagentsDialog
	residentDialogActive := residentDialog != nil && residentDialog.Active()
	residentSession := i.residentChildSession
	i.mu.Unlock()
	if residentDialogActive {
		if k.Kind == tui.KeyCtrlC || k.Kind == tui.KeyEsc {
			i.mu.Lock()
			if i.residentSubagentsDialog == residentDialog {
				residentDialog.Close()
			}
			i.mu.Unlock()
			i.invalidate()
			return false
		}
		i.mu.Lock()
		childID := residentDialog.HandleKey(k)
		i.mu.Unlock()
		if childID != "" {
			i.openResidentChildSession(childID)
		}
		i.invalidate()
		return false
	}
	if residentSession != nil {
		switch k.Kind {
		case tui.KeyCtrlC, tui.KeyEsc:
			i.mu.Lock()
			if i.residentChildSession == residentSession {
				i.residentChildSession = nil
			}
			i.mu.Unlock()
			residentSession.Close()
			i.setResidentChildMouseReporting(false)
		case tui.KeyPageUp:
			residentSession.Scroll(10)
		case tui.KeyPageDown:
			residentSession.Scroll(-10)
		case tui.KeyUp:
			if !residentSession.HandleNavigation(k) {
				residentSession.Scroll(1)
			}
		case tui.KeyDown:
			if !residentSession.HandleNavigation(k) {
				residentSession.Scroll(-1)
			}
		case tui.KeyMouseWheelUp:
			residentSession.Scroll(1)
		case tui.KeyMouseWheelDown:
			residentSession.Scroll(-1)
		default:
			session := residentSession
			if prompt, submit := session.HandleKey(k); submit {
				go func() {
					err := i.cfg.ResidentManager.Resume(ctx, session.childID, prompt)
					session.FinishSubmission(err)
					i.invalidate()
				}()
			}
		}
		i.invalidate()
		return false
	}
	if i.logoutDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.logoutDialog.Close()
			i.invalidate()
			return false
		}
		act := i.logoutDialog.HandleKey(k)
		if act.Select {
			i.doLogout(act.Target)
		}
		i.invalidate()
		return false
	}
	if i.telegramDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.telegramDialog.Close()
			i.invalidate()
			return false
		}
		act := i.telegramDialog.HandleKey(k)
		if act.Select {
			i.doTelegram(act.Action)
		}
		i.invalidate()
		return false
	}
	if i.settingsDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.settingsDialog.Close()
			i.invalidate()
			return false
		}
		act := i.settingsDialog.HandleKey(k)
		if act.ModelShortcutSlot > 0 {
			i.openQuickModelPicker(act.ModelShortcutSlot)
		}
		if act.Toggle {
			i.applySettingChange(act)
		}
		i.invalidate()
		return false
	}
	if i.sessionOpsDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.sessionOpsDialog.Close()
			i.invalidate()
			return false
		}
		act := i.sessionOpsDialog.HandleKey(k)
		if act.Select {
			i.doSessionOp(act.Action, "")
		}
		i.invalidate()
		return false
	}
	if i.sessionTreeDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.sessionTreeDialog.Close()
			i.invalidate()
			return false
		}
		act := i.sessionTreeDialog.HandleKey(k)
		if act.Select {
			if act.Target.Role != "" || act.Target.SourcePath != "" || act.Target.IsBoundary() || act.Target.UserDraft != "" {
				i.applySessionTreeTarget(act.Target, act.TurnNo)
			} else {
				// Keep package embedders that still return the scalar action
				// source-compatible while the dialog migrates to Target.
				i.applySessionTreeMessageSelection(act.Path, act.MessageIdx, act.TurnNo, act.Role, act.Prompt)
			}
		}
		i.invalidate()
		return false
	}
	if i.extPanel.Active() {
		if k.Kind == tui.KeyCtrlC || k.Kind == tui.KeyEsc {
			if i.cfg.Extensions != nil {
				_ = i.cfg.Extensions.SendPanelClose(i.extPanel.ext, i.extPanel.id)
			}
			i.extPanel.Close()
			i.invalidate()
			return false
		}
		if i.cfg.Extensions != nil {
			_ = i.cfg.Extensions.SendPanelKey(i.extPanel.ext, i.extPanel.id, panelKeyName(k), panelKeyText(k))
		}
		return false
	}
	if i.jumpDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.jumpDialog.Close()
			i.pendingFork = false
			return false
		}
		act := i.jumpDialog.HandleKey(k)
		if act.Select {
			if i.pendingFork {
				i.applyForkSelection(act.MessageIdx)
			} else {
				i.applyJumpSelection(act.MessageIdx, act.TurnNo)
			}
		}
		// If the user dismissed the dialog without selecting, also
		// clear the pending-fork flag so a later plain /jump isn't
		// hijacked.
		if act.Close {
			i.pendingFork = false
		}
		return false
	}
	if i.btwDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.btwDialog.Close()
			i.invalidate()
			return false
		}
		if k.Kind == tui.KeyCtrlO {
			i.toggleBtwToolExpansion()
			return false
		}
		i.btwDialog.HandleKey(k, i.invalidate)
		return false
	}
	if i.skillsDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.skillsDialog.Close()
			i.invalidate()
			return false
		}
		i.skillsDialog.HandleKey(k)
		i.invalidate()
		return false
	}
	if i.changelogDialog.Active() {
		if closed := i.changelogDialog.HandleKey(k); closed {
			// User dismissed; let the parent persist the
			// LastChangelogShown marker via the close callback.
			if i.cfg.OnChangelogDismiss != nil {
				i.cfg.OnChangelogDismiss()
			}
		}
		i.invalidate()
		return false
	}

	if slot := quickModelShortcutSlot(k); slot > 0 {
		i.applyQuickModelShortcut(slot)
		return false
	}

	// The first eligible bare Escape is deliberately left to the existing
	// Escape path (which clears idle input or cancels a turn). A second tap
	// within the gesture window only gets first refusal while the cheap,
	// local eligibility rules still hold. If a turn started, a message was
	// queued, or the input became ineligible between taps, reset the detector
	// and fall through so Escape retains its normal cancellation behavior.
	// Once those rules still hold, the full tree gate owns the second tap:
	// family/read failures are fail-closed but are nevertheless consumed so
	// they cannot fall through into ordinary editor handling.
	if isUnmodifiedEscape(k) {
		now := i.sessionTreeEscapeNow()
		i.mu.Lock()
		consumed := i.doubleEscape.Consume(now)
		i.mu.Unlock()
		if consumed {
			if i.canArmSessionTreeEscape() {
				i.openSessionTree()
				return false
			}
			i.mu.Lock()
			i.doubleEscape.Reset()
			i.mu.Unlock()
		} else if i.canArmSessionTreeEscape() {
			i.mu.Lock()
			i.doubleEscape.Arm(now)
			i.mu.Unlock()
		}
	}

	// Global keys.
	switch k.Kind {
	case tui.KeyCtrlC:
		i.mu.Lock()
		loadingSession := i.sessionLoading
		i.mu.Unlock()
		if loadingSession {
			i.markCtrlCExit()
			return true
		}
		// While busy: do NOT cancel the turn. ctrl+c during a
		// running turn is almost always reflex muscle memory
		// ("be quiet" in a shell) rather than a deliberate
		// decision to kill a multi-minute model call that's
		// already cost tokens. Use esc to interrupt a turn; use
		// a deliberate double-ctrl+c to exit zut entirely. First
		// press arms the exit hint, second press within
		// ctrlCExitWindow quits.
		if i.busy {
			if i.ctrlCExitArmed() {
				i.markCtrlCExit()
				return true
			}
			i.mu.Lock()
			i.statusOK = "press ctrl+c again to exit, esc to cancel the turn"
			i.statusErr = ""
			i.mu.Unlock()
			i.armCtrlCExit()
			return false
		}
		// Idle: first press clears the editor (and any queued
		// follow-up messages); a second press within ctrlCExitWindow
		// exits. With both an empty editor and no queue the first
		// press still just arms — require a deliberate double-tap.
		ag := i.agent
		pending := 0
		if ag != nil {
			pending = ag.QueuedMessageCount()
		}
		hadInput := !i.ed.IsEmpty() || len(i.queued) > 0 || pending > 0
		if hadInput {
			i.ed.Clear()
			i.clipboardImages = nil
			i.suggest.Reset()
			if ag != nil {
				ag.DrainQueuedMessages()
			}
			i.mu.Lock()
			i.queued = nil
			i.statusOK = "input cleared"
			i.statusErr = ""
			i.mu.Unlock()
			i.armCtrlCExit()
			return false
		}
		if i.ctrlCExitArmed() {
			i.markCtrlCExit()
			return true
		}
		i.mu.Lock()
		i.statusOK = "press ctrl+c again to exit"
		i.statusErr = ""
		i.mu.Unlock()
		i.armCtrlCExit()
		return false
	case tui.KeyEsc:
		// Esc interrupts a running turn — but only when nothing
		// else on screen wants to consume the key first. The slash
		// popup has its own Esc behaviour (close + clear editor),
		// and transient overlays like the /help block and extension
		// notes should dismiss on Esc before we even consider the
		// turn. Without these guards, a casual Esc press after
		// running /help on a busy turn rips the turn away.
		if i.suggest.Active(i.ed.Value()) || i.fileSuggest.Active(i.ed.Value()) {
			break
		}
		i.mu.Lock()
		hadHelp := len(i.helpBlock) > 0
		hadNotes := len(i.extNotes) > 0
		if hadHelp {
			i.helpBlock = nil
		}
		if hadNotes {
			i.extNotes = nil
		}
		i.mu.Unlock()
		if hadHelp || hadNotes {
			i.invalidate()
			return false
		}
		i.mu.Lock()
		busyCancel := i.busy && i.cancelTurn != nil
		cancelTurn := i.cancelTurn
		var restoreQueued core.QueuedMessage
		var hasRestoreQueued bool
		var handoff json.RawMessage
		var persistHandoff bool
		if busyCancel {
			// Keep the most recently queued follow-up as an editable draft
			// instead of losing it with the cancelled turn's stale queue.
			if i.agent != nil {
				restoreQueued, hasRestoreQueued = i.agent.PopQueuedMessage()
			}
			if !hasRestoreQueued && len(i.queued) > 0 {
				n := len(i.queued) - 1
				restoreQueued = i.queued[n]
				i.queued = i.queued[:n]
				hasRestoreQueued = true
			}
			handoff, persistHandoff = i.resetCompactContinuationLocked()
		}
		i.mu.Unlock()
		if busyCancel {
			if hasRestoreQueued {
				i.restoreQueuedMessageToEditor(restoreQueued)
			}
			i.updateActiveGoal(core.GoalPaused, "interrupted by user")
			cancelTurn()
			// If a confirm dialog is pending, refuse it so the agent
			// goroutine unblocks and the context cancellation can
			// actually take effect.
			i.confirmDialog.CancelAll("turn cancelled")
			if persistHandoff {
				i.persistCompactHandoff(handoff)
			}
			return false
		}
	case tui.KeyCtrlD:
		if i.ed.IsEmpty() && !i.busy {
			return true
		}
	case tui.KeyCtrlB:
		i.mu.Lock()
		i.rightBarHidden = !i.rightBarHidden
		i.mu.Unlock()
		i.requestRendererInvalidate()
		i.invalidate()
		return false
	case tui.KeyCtrlL:
		i.requestRendererClear()
		i.invalidate()
		return false
	case tui.KeyPasteClipboard:
		i.pasteClipboard(ctx)
		return false
	case tui.KeyCtrlO:
		i.toggleToolExpansion()
		return false
	case tui.KeyPageUp:
		i.scrollBy(+i.chatPage())
		return false
	case tui.KeyPageDown:
		i.scrollBy(-i.chatPage())
		return false
	case tui.KeyUp:
		// Alt/Option+Up: pop the most recently queued ("sliding in")
		// message back into the editor so the user can edit and
		// resend it. Repeated presses keep peeling messages off the
		// tail of the queue; each press *replaces* the editor
		// contents (we don't append/push). When the queue is empty
		// the keypress falls through to the normal scroll behavior.
		if k.Alt {
			i.mu.Lock()
			var message core.QueuedMessage
			var ok bool
			if i.agent != nil {
				message, ok = i.agent.PopQueuedMessage()
			}
			if !ok {
				if n := len(i.queued); n > 0 {
					message = i.queued[n-1]
					i.queued = i.queued[:n-1]
					ok = true
				}
			}
			i.mu.Unlock()
			if ok && i.restoreQueuedMessageToEditor(message) {
				i.invalidate()
				return false
			}
		}
		// In multi-line / wrapped input, Up first moves inside the editor.
		// At the editor's top edge, plain Up can browse input history when
		// history browsing is safe/active; otherwise it falls back to chat
		// scrolling, preserving the old single-line scroll behavior.
		if !i.suggest.Active(i.ed.Value()) && !i.fileSuggest.Active(i.ed.Value()) {
			if i.ed.MoveVertical(-1) {
				i.invalidate()
				return false
			}
			if !k.Alt && i.handleInputHistoryKey(k) {
				return false
			}
			i.scrollBy(+3)
			return false
		}
	case tui.KeyDown:
		if !i.suggest.Active(i.ed.Value()) && !i.fileSuggest.Active(i.ed.Value()) {
			if i.ed.MoveVertical(+1) {
				i.invalidate()
				return false
			}
			if !k.Alt && i.inputHistoryIndex < 0 && i.cfg.ResidentManager != nil {
				if _, total := i.cfg.ResidentManager.RecentSnapshotPage(0, 1); total > 0 {
					i.runResidentSubagents(ctx, nil)
					return false
				}
			}
			if !k.Alt && i.handleInputHistoryKey(k) {
				return false
			}
			if i.scrollOffset > 0 {
				i.scrollBy(-3)
			}
			return false
		}
	}

	// Note: we intentionally do NOT gate the editor on i.busy here.
	// Typing while the agent is working is supported — submitted
	// messages are queued and delivered as follow-up turns when the
	// current turn ends. See the submit handler below.

	if k.Kind == tui.KeyEnter && (k.Alt || k.Shift) {
		i.ed.HandleKey(k)
		return false
	}

	// Slash suggestions: intercept up/down/tab/enter when the popup is visible.
	if i.suggest.Active(i.ed.Value()) {
		switch k.Kind {
		case tui.KeyUp:
			i.suggest.Up()
			return false
		case tui.KeyDown:
			i.suggest.Down()
			return false
		case tui.KeyPageUp:
			i.suggest.PageUp()
			return false
		case tui.KeyPageDown:
			i.suggest.PageDown()
			return false
		case tui.KeyTab:
			if name := i.suggest.Selection(i.ed.Value()); name != "" {
				i.ed.SetValue(name)
				i.suggest.Reset()
			}
			return false
		case tui.KeyEnter:
			// Enter on an ambiguous or partial slash prefix: complete to the
			// currently highlighted command and run it. That way typing
			// "/lo" + enter picks whichever of /login or /logout is selected
			// in the popup instead of submitting "/lo" as unknown. Also
			// clear the editor so the command doesn't linger after the
			// dialog opens/closes.
			if name := i.suggest.Selection(i.ed.Value()); name != "" {
				i.ed.Clear()
				i.suggest.Reset()
				return i.runSlash(ctx, name)
			}
		case tui.KeyEsc:
			i.ed.Clear()
			i.suggest.Reset()
			return false
		}
	}

	// File suggestions: intercept up/down/tab/enter when the @-popup is visible.
	if i.fileSuggest.Active(i.ed.Value()) {
		switch k.Kind {
		case tui.KeyUp:
			i.fileSuggest.Up()
			return false
		case tui.KeyDown:
			i.fileSuggest.Down()
			return false
		case tui.KeyRight:
			// Open selected directory. The filter the user typed picked
			// that directory at the current level; once we descend it no
			// longer applies to the directory's contents, so clear it.
			// Otherwise typing "@eda" then right would re-filter inside
			// eda/ by "eda" and show nothing.
			if i.fileSuggest.Right() {
				i.clearFileSuggestQuery()
			}
			return false
		case tui.KeyLeft:
			// Go back to parent directory. Clear the filter for the same
			// reason as Right: it was scoped to the level we just left.
			if i.fileSuggest.Left() {
				i.clearFileSuggestQuery()
			}
			return false
		case tui.KeyEnter:
			if entry, ok := i.fileSuggest.SelectedEntry(i.ed.Value()); ok {
				var chip string
				if entry.isDir {
					chip = "[dir:" + entry.rel + "/]"
				} else {
					chip = "[file:" + entry.rel + "]"
				}
				val := i.ed.Value()
				if idx := strings.LastIndex(val, "@"); idx >= 0 {
					val = val[:idx]
				}
				i.ed.SetValue(val + chip + " ")
				i.fileSuggest.Reset()
			}
			return false
		case tui.KeyEsc:
			val := i.ed.Value()
			if idx := strings.LastIndex(val, "@"); idx >= 0 {
				i.ed.SetValue(val[:idx])
			}
			i.fileSuggest.Reset()
			return false
		}
	}

	// Tab-complete a path token in the editor when no popup is open.
	// Recognises tokens that look like paths (start with ~, /, ./, ../
	// or contain a slash); shell-style completion expands ~, lists the
	// parent dir, and completes the basename to the longest common
	// prefix. Single match: full replace and trailing / for dirs.
	// No match: no-op. Plain bare words (no slash, no tilde) fall
	// through so Tab keeps its current no-op behaviour outside paths.
	if k.Kind == tui.KeyTab && !i.suggest.Active(i.ed.Value()) && !i.fileSuggest.Active(i.ed.Value()) {
		if i.tryPathTabComplete() {
			return false
		}
	}

	if i.inputHistoryIndex >= 0 && k.Kind != tui.KeyUp && k.Kind != tui.KeyDown {
		i.inputHistoryIndex = -1
	}

	if k.Kind == tui.KeyEsc {
		i.clipboardImages = nil
	}
	if submit := i.ed.HandleKey(k); submit {
		// SubmitValue() expands any [pasted text #N +L lines]
		// placeholders back into their bodies; the raw Value()
		// is only what the user sees on screen.
		text := strings.TrimRight(i.ed.SubmitValue(), "\n")
		// Expand [file:name] and [dir:name/] chips to full paths.
		text = expandFileChips(text, i.cfg.CWD)
		text, images := preparePromptWithClipboardImages(text, i.clipboardImages)
		if text == "" && len(images) == 0 {
			return false
		}
		clearSubmittedInput := func() {
			i.clipboardImages = nil
			i.ed.Clear()
			i.inputHistoryIndex = -1
			i.suggest.Reset()
			i.fileSuggest.Reset()
		}

		// Shell escapes and slash commands are text-only. An image-bearing
		// draft remains a normal provider prompt instead of silently dropping
		// its attachments.
		if len(images) == 0 {
			if cmd, ok := shellEscapeCommand(text); ok {
				clearSubmittedInput()
				i.startShellEscape(ctx, cmd)
				return false
			}
		}

		if len(images) == 0 && looksLikeSlashCommand(text) {
			clearSubmittedInput()
			head := text
			rest := ""
			if idx := strings.IndexAny(text, " \t"); idx >= 0 {
				head = text[:idx]
				rest = strings.TrimSpace(text[idx:])
			}
			if !isKnownSlashCommand(text) {
				// Try extensions before giving up. Extensions register
				// commands by bare name (no leading slash); strip it here.
				extName := strings.TrimPrefix(head, "/")
				if i.cfg.Extensions != nil && i.cfg.Extensions.HasCommand(extName) {
					go i.invokeExtensionCommand(ctx, extName, rest)
					return false
				}
				i.mu.Lock()
				i.statusErr = "unknown command " + head + " — type /help to see the list"
				i.statusOK = ""
				i.mu.Unlock()
				return false
			}
			// Slash commands run regardless of busy state. Commands that
			// would mutate the transcript or replace the agent (/clear,
			// /compact, /logout, /login, /model, /fork, /session fork)
			// cancel the active turn
			// first and wait for the goroutine to wind down so they don't
			// race with a streaming response. Safe commands (/help,
			// /jump, /sessions, /jail, /unjail, /exit) run immediately
			// without disturbing the active turn.
			if slashCommandCancelsTurn(text) {
				i.cancelAndWaitForIdle()
			}
			return i.runSlash(ctx, text)
		}

		if i.agent == nil {
			i.mu.Lock()
			i.statusErr = "not logged in. type /login first."
			i.mu.Unlock()
			return false
		}
		// Mirror the user's typed prompt into the paired Telegram
		// chat (when the bridge is active) so the Telegram thread
		// stays a complete record of the session, not just the half
		// that originated on the phone. On a goroutine so the
		// network write doesn't delay the local turn.
		if i.telegramBridge != nil && i.telegramBridge.Active() {
			go i.telegramBridge.OnUserTyped(text)
		}
		i.maybeStartSessionTitle(ctx, text)
		// If a turn is already in flight, queue this prompt inside the
		// agent loop so it is delivered at the next safe model-call
		// boundary instead of waiting for the whole run to finish.
		i.mu.Lock()
		busy := i.busy
		compacting := i.compacting
		ag := i.agent
		i.mu.Unlock()
		if busy {
			clearSubmittedInput()
			var handoff json.RawMessage
			var persistHandoff bool
			if ag != nil && !compacting {
				i.mu.Lock()
				handoff, persistHandoff = i.resetCompactContinuationLocked()
				ag.QueueMessage(text, images)
				i.mu.Unlock()
			} else {
				i.mu.Lock()
				handoff, persistHandoff = i.resetCompactContinuationLocked()
				i.queued = append(i.queued, core.QueuedMessage{Text: text, Images: images})
				i.mu.Unlock()
			}
			if persistHandoff {
				i.persistCompactHandoff(handoff)
			}
			i.invalidate()
			return false
		}
		clearSubmittedInput()
		i.startTurnWithImages(ctx, text, images)
	}
	return false
}
func (i *Interactive) pasteClipboard(ctx context.Context) {
	image, ok, _ := tui.ReadClipboardImage(ctx)
	if ok {
		i.mu.Lock()
		marker := fmt.Sprintf("[clipboard image #%d]", len(i.clipboardImages)+1)
		i.clipboardImages = append(i.clipboardImages, clipboardImageAttachment{
			Marker: marker,
			Image:  provider.ImageBlock{MimeType: image.MimeType, Data: image.Data},
		})
		i.ed.Insert(marker + " ")
		i.statusOK = ""
		i.statusErr = ""
		i.mu.Unlock()
		return
	}
	key, pastedText, err := resolveClipboardText(tui.Key{Kind: tui.KeyPasteClipboard}, tui.ReadClipboardText)
	i.mu.Lock()
	defer i.mu.Unlock()
	if err != nil {
		i.statusErr = "clipboard paste failed: " + err.Error()
		i.statusOK = ""
		return
	}
	if pastedText {
		i.ed.HandleKey(key)
		i.statusErr = ""
		i.statusOK = ""
		return
	}
	i.statusErr = "clipboard does not contain text or an image"
	i.statusOK = ""
}
func (i *Interactive) handleInputHistoryKey(k tui.Key) bool {
	if k.Kind != tui.KeyUp && k.Kind != tui.KeyDown {
		return false
	}
	// Do not steal normal vertical cursor movement. History browsing can only
	// start from an empty editor; once active, Up/Down keep walking
	// the ring so repeated presses work even though the editor now
	// contains the selected historical prompt.
	if i.inputHistoryIndex < 0 && !i.ed.IsEmpty() {
		return false
	}
	hist := i.inputHistory()
	if len(hist) == 0 {
		return false
	}

	if i.inputHistoryIndex < 0 {
		// Start just after the newest item so Up lands on the most
		// recent user prompt. A lone Down from an empty editor is not
		// history navigation; let the caller fall through to normal UI
		// behavior instead.
		if k.Kind != tui.KeyUp {
			return false
		}
		i.inputHistoryIndex = len(hist)
	}

	switch k.Kind {
	case tui.KeyUp:
		if i.inputHistoryIndex > 0 {
			i.inputHistoryIndex--
		}
	case tui.KeyDown:
		if i.inputHistoryIndex < len(hist) {
			i.inputHistoryIndex++
		}
	}

	if i.inputHistoryIndex >= len(hist) {
		i.ed.Clear()
	} else {
		i.ed.SetValue(hist[i.inputHistoryIndex])
	}
	return true
}
func (i *Interactive) inputHistory() []string {
	if i.agent == nil {
		return nil
	}
	msgs := i.agent.Messages()
	hist := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if !isForkableUserMessage(m) {
			continue
		}
		text := userMessageText(m)
		if strings.TrimSpace(text) == "" {
			continue
		}
		hist = append(hist, text)
	}
	return hist
}
func userMessageText(m provider.Message) string {
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
