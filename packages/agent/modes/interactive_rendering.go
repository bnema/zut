package modes

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/bnema/zut/packages/agent/skills"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

func queuedMessageSummary(message core.QueuedMessage, maxWidth int) string {
	if len(message.Images) == 0 {
		return truncateLine(message.Text, maxWidth)
	}
	imageLabel := "[image]"
	if len(message.Images) > 1 {
		imageLabel = fmt.Sprintf("[%d images]", len(message.Images))
	}
	if message.Text == "" || maxWidth <= len(imageLabel) {
		return truncateLine(imageLabel, maxWidth)
	}
	text := truncateLine(message.Text, maxWidth-len(imageLabel)-1)
	if text == "" {
		return imageLabel
	}
	return text + " " + imageLabel
}

func (i *Interactive) cachedChatLocked(cols int) []string {
	// A busy frame contains mutable streaming/tool state. Keep the stable
	// transcript cache, but never return a full-frame cache entry that would
	// hide a newly arrived delta or completed result.
	if i.busy || i.streamOn || i.streamFlushPending {
		return i.buildChatLocked(cols)
	}
	key := i.chatCacheKeyLocked(cols)
	if i.chatCacheValid && i.chatCacheKey == key {
		return append([]string(nil), i.chatCache...)
	}
	chat := i.buildChatLocked(cols)
	key = i.chatCacheKeyLocked(cols)
	i.chatCache = append(i.chatCache[:0], chat...)
	i.chatCacheKey = key
	i.chatCacheValid = true
	return chat
}
func (i *Interactive) chatCacheKeyLocked(cols int) chatCacheKey {
	var rev uint64
	if i.agent != nil {
		rev = i.agent.Revision()
	}
	showVer := len(i.view.Messages) == 0 && !i.streamOn && len(i.toolOrder) == 0 && !i.welcomeStart.IsZero() && time.Since(i.welcomeStart) < welcomeVersionDuration
	return chatCacheKey{
		cols:                 cols,
		agentRev:             rev,
		statusOK:             i.statusOK,
		statusErr:            i.statusErr,
		help:                 strings.Join(i.helpBlock, "\n"),
		sessionInfo:          sessionInfoBlocksKey(i.sessionInfoBlocks),
		extNotes:             strings.Join(i.extNotes, "\n"),
		extStatuses:          i.extensionStatusesKeyLocked(),
		extWidgets:           i.extensionWidgetsKeyLocked(),
		reloadErrors:         strings.Join(i.reloadErrors, "\n"),
		updateAvailable:      i.updateInfo.Available,
		updateCurrent:        i.updateInfo.Current,
		updateLatest:         i.updateInfo.Latest,
		updateURL:            i.updateInfo.URL,
		welcomeShowVer:       showVer,
		expandAll:            i.view.ExpandAll,
		tailLimit:            i.view.TailLimit,
		renderedMessageCount: len(i.view.Messages),
		viewCacheRev:         i.view.RenderCacheRevision,
	}
}
func sortedNestedOuterKeys[T any](groups map[string]map[string]T) []string {
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
func sortedNestedInnerKeys[T any](groups map[string]map[string]T, outer string) []string {
	items := groups[outer]
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
func (i *Interactive) extensionStatusesKeyLocked() string {
	var sb strings.Builder
	extNames := sortedNestedOuterKeys(i.extStatuses)
	for _, extName := range extNames {
		keys := sortedNestedInnerKeys(i.extStatuses, extName)
		for _, key := range keys {
			status := i.extStatuses[extName][key]
			fmt.Fprintf(&sb, "%s/%s/%s/%s\n", extName, key, status.Level, status.Text)
		}
	}
	return sb.String()
}
func (i *Interactive) extensionWidgetsKeyLocked() string {
	var sb strings.Builder
	extNames := sortedNestedOuterKeys(i.extWidgets)
	for _, extName := range extNames {
		ids := sortedNestedInnerKeys(i.extWidgets, extName)
		for _, id := range ids {
			widget := i.extWidgets[extName][id]
			fmt.Fprintf(&sb, "%s/%s/%s/%s\n", extName, id, widget.Position, widget.Title)
			for _, line := range widget.Lines {
				sb.WriteString(line)
				sb.WriteByte('\n')
			}
		}
	}
	return sb.String()
}
func (i *Interactive) rightBarWidgetsLocked() []tui.RightBarWidget {
	var out []tui.RightBarWidget
	for _, extName := range sortedNestedOuterKeys(i.extWidgets) {
		for _, id := range sortedNestedInnerKeys(i.extWidgets, extName) {
			widget := i.extWidgets[extName][id]
			if extproto.NormalizeWidgetPosition(widget.Position) != extproto.WidgetPositionRightBar {
				continue
			}
			out = append(out, tui.RightBarWidget{
				Extension: extName,
				ID:        id,
				Title:     widget.Title,
				Lines:     append([]string(nil), widget.Lines...),
			})
		}
	}
	return out
}
func (i *Interactive) extensionChromeLinesLocked(cols int) []string {
	return i.extensionChromeLinesAtLocked(cols, false)
}
func (i *Interactive) extensionChromeLinesForLayoutLocked(cols int, rightBarActive, rightBarFallback bool) []string {
	lines := i.extensionChromeLinesAtLocked(cols, rightBarActive)
	if !rightBarFallback || len(lines) <= maxNarrowExtensionChromeRows {
		return lines
	}
	limit := maxNarrowExtensionChromeRows
	trimmed := append([]string(nil), lines[:limit-1]...)
	trimmed = append(trimmed, i.cfg.Theme.FGColor(i.cfg.Theme.Muted, truncateLine("  ... extension chrome truncated ...", cols)))
	return trimmed
}
func (i *Interactive) extensionChromeLinesAtLocked(cols int, rightBarActive bool) []string {
	var out []string
	bodyWidth := cols - 2
	if bodyWidth < 1 {
		bodyWidth = 1
	}
	widgetRows := 0
	widgetTruncated := false
	appendWidgetRow := func(line string) bool {
		if widgetRows >= maxExtensionWidgetRows-1 {
			widgetTruncated = true
			return false
		}
		out = append(out, line)
		widgetRows++
		return true
	}

	stopWidgets := false
	for _, extName := range sortedNestedOuterKeys(i.extWidgets) {
		if stopWidgets {
			break
		}
		for _, id := range sortedNestedInnerKeys(i.extWidgets, extName) {
			widget := i.extWidgets[extName][id]
			if rightBarActive && extproto.NormalizeWidgetPosition(widget.Position) == extproto.WidgetPositionRightBar {
				continue
			}
			label := "  [" + extName + "]"
			if widget.Title != "" {
				label += " " + widget.Title
			}
			if !appendWidgetRow(i.cfg.Theme.FGColor(i.cfg.Theme.Accent, truncateLine(label, cols))) {
				stopWidgets = true
				break
			}
			for _, line := range widget.Lines {
				if !appendWidgetRow("  " + truncateLine(line, bodyWidth)) {
					stopWidgets = true
					break
				}
			}
			if stopWidgets {
				break
			}
		}
	}
	if widgetTruncated {
		out = append(out, i.cfg.Theme.FGColor(i.cfg.Theme.Muted, truncateLine("  ... extension widgets truncated ...", cols)))
	}

	statusRows := 0
	statusTruncated := false
	for _, extName := range sortedNestedOuterKeys(i.extStatuses) {
		for _, key := range sortedNestedInnerKeys(i.extStatuses, extName) {
			if statusRows >= maxExtensionStatusRows-1 {
				statusTruncated = true
				break
			}
			status := i.extStatuses[extName][key]
			color := i.cfg.Theme.Muted
			switch status.Level {
			case "warn":
				color = i.cfg.Theme.Warning
			case "error":
				color = i.cfg.Theme.Error
			case "success":
				color = i.cfg.Theme.Tool
			}
			label := "  [" + extName + "] " + status.Text
			out = append(out, i.cfg.Theme.FGColor(color, truncateLine(label, cols)))
			statusRows++
		}
		if statusTruncated {
			break
		}
	}
	if statusTruncated {
		out = append(out, i.cfg.Theme.FGColor(i.cfg.Theme.Muted, truncateLine("  ... extension statuses truncated ...", cols)))
	}
	return out
}
func (i *Interactive) stableChatRowsLocked(cols int) []string {
	key := i.chatCacheKeyLocked(cols)
	if i.stableChatCacheValid && i.stableChatCacheKey == key {
		return append([]string(nil), i.stableChatCache...)
	}

	renderView := i.view.CloneForRender()
	renderView.Streaming = ""
	renderView.StreamingActive = false
	renderView.ToolCalls = nil
	renderView.Err = ""
	infoBlocks := append([]sessionInfoBlock(nil), i.sessionInfoBlocks...)
	if !i.renderOutsideLock {
		rows, anchors := renderView.BuildWithAnchors(cols)
		rows = insertSessionInfoBlocks(rows, anchors, infoBlocks)
		i.stableChatCache = append(i.stableChatCache[:0], rows...)
		i.stableChatCacheKey = key
		i.stableChatCacheValid = true
		return append([]string(nil), rows...)
	}
	// Markdown, syntax highlighting, and wrapping are the expensive part of
	// a frame. Build the immutable snapshot without holding the interactive
	// mutex so key processing can continue while a cold transcript renders.
	i.mu.Unlock()
	rows, anchors := renderView.BuildWithAnchors(cols)
	rows = insertSessionInfoBlocks(rows, anchors, infoBlocks)
	i.mu.Lock()
	if i.view.MessagesRevision == renderView.MessagesRevision &&
		i.view.RenderCacheRevision == renderView.RenderCacheRevision {
		i.view.AdoptRenderCacheFrom(renderView)
	}

	i.stableChatCache = append(i.stableChatCache[:0], rows...)
	i.stableChatCacheKey = key
	i.stableChatCacheValid = true
	return append([]string(nil), rows...)
}
func (i *Interactive) buildChatLocked(cols int) []string {
	if i.agent != nil {
		i.view.Messages = filterHiddenTranscriptMessages(i.agent.Messages())
		i.view.MessagesRevision = i.agent.Revision()
	} else {
		i.view.Messages = nil
		i.view.MessagesRevision = 0
	}
	// Transcript rewinds and compaction can remove messages that an info block
	// was anchored after. Clamp those anchors once so later turns still append
	// below the block instead of making it jump through the conversation.
	clampSessionInfoBlocks(i.sessionInfoBlocks, len(i.view.Messages))
	// Pacer flush: while the streaming pacer is still draining the
	// buffer (i.e. EvAssistantMessage already fired but more runes
	// are queued), the final assistant message is already in
	// i.agent.Messages() in full. Painting it in the transcript
	// AND the streaming block at the same time shows the user the
	// complete text immediately — which defeats the whole pacer.
	// Hide the last message until the pacer catches up; once the
	// flush-pending latch clears, the message is revealed (the
	// streaming block disappears the same frame because streamOn
	// flips off, so the transition is seamless).
	if i.streamFlushPending && len(i.view.Messages) > 0 {
		i.view.Messages = i.view.Messages[:len(i.view.Messages)-1]
	}
	i.view.Streaming = i.streaming.String()
	i.view.StreamingActive = i.streamOn
	// Guard against the narrow race where EvAssistantMessage has
	// just promoted a streaming reply into the transcript but a
	// render tick hasn't flipped streamOn off yet. Without the
	// guard, the same text would appear twice (once as the
	// in-flight streaming block, once as the last transcript
	// message). We detect the duplicate strictly: the last
	// assistant message's visible text must equal the streaming
	// buffer. Just matching on role is too broad — it also hides
	// the next round's typewriter streaming after a tool turn,
	// because the last transcript message is always assistant
	// (the tool-use block) until the follow-up summary lands.
	if i.streamOn && i.streaming.Len() > 0 {
		if n := len(i.view.Messages); n > 0 && i.view.Messages[n-1].Role == provider.RoleAssistant {
			if assistantText(i.view.Messages[n-1]) == i.streaming.String() {
				i.view.StreamingActive = false
			}
		}
	}
	// Live tool-call view: only shown while a turn is in flight. Once
	// the agent is idle, every tool call has already been folded into
	// the transcript (as assistant.ToolCallBlock + a tool-role message),
	// so showing v.ToolCalls a second time would duplicate them below
	// the final assistant text — which looks like the summary came
	// "before" the tools.
	i.view.ToolCalls = i.view.ToolCalls[:0]
	if i.busy {
		for _, id := range i.toolOrder {
			// Deterministic ordering: a tool block stays hidden until
			// the paced assistant text that preceded it has finished
			// typing out. toolOrder is append-only in arrival order,
			// so once one tool is still gated, every later tool is too,
			// so stop here to avoid showing a tool out of sequence.
			if !i.toolGateOpenLocked(id) {
				break
			}
			if tc, ok := i.toolCalls[id]; ok {
				i.view.ToolCalls = append(i.view.ToolCalls, *tc)
			}
		}
	}
	i.view.Err = i.statusErr
	// Live streaming/tool rows are appended to the chat buffer (not
	// hoisted into a separate live block above the editor). That keeps
	// the renderer's diff view append-only: when a tool finishes the
	// rows update in place at the end of the buffer, instead of the
	// whole bottom band shrinking and shifting chat lines around.
	i.liveBlock = nil
	stable := i.stableChatRowsLocked(cols)
	chat := append([]string(nil), stable...)
	chat = append(chat, i.view.BuildLive(cols)...)

	// Welcome banner: shown at the top of the chat area when there is
	// no transcript yet. Disappears after the first message is sent.
	// The version suffix is shown for welcomeVersionDuration after
	// startup, then drops off automatically.
	if len(i.view.Messages) == 0 && !i.streamOn && len(i.toolOrder) == 0 {
		showVer := !i.welcomeStart.IsZero() && time.Since(i.welcomeStart) < welcomeVersionDuration
		chat = append(welcomeBanner(i.cfg.Theme, i.cfg.Version, showVer), chat...)
	}

	// Update-available banner: prepended above everything else so it's
	// the first thing the user sees when opening a new zut session.
	// Once rendered, it stays until the user updates to a newer
	// version — we don't persist a "dismissed" flag because this is
	// cheap and re-showing it is how most users remember to update.
	if i.updateInfo.Available {
		banner := renderUpdateBanner(i.cfg.Theme, i.updateInfo, cols)
		chat = append(banner, chat...)
	}

	// /help block: appended to the transcript so it appears at the
	// bottom of the chat area (right above the status bar / editor).
	// Prepending it would push long conversations off the top of the
	// viewport, which users would miss entirely.
	if len(i.helpBlock) > 0 {
		chat = append(chat, i.helpBlock...)
	}

	if i.statusOK != "" {
		// Hard-truncate the OK line to the visible width so a long
		// session path ("resumed session: /Users/.../sessions/...")
		// doesn't overflow past the right edge and look broken on a
		// narrow terminal.
		line := "✓ " + i.statusOK
		if cols > 4 && len(line) > cols {
			line = line[:cols-3] + "..."
		}
		chat = append(chat, i.cfg.Theme.FGColor(i.cfg.Theme.Tool, line), "")
	}

	// Live shell-escape output (!command / entry.pre) streams into the
	// transcript area while the command runs, then is replaced by the
	// final user-context message when it finishes.
	if i.shellRunning && i.shellLive != "" {
		chat = append(chat, strings.Split(strings.TrimRight(i.shellLive, "\n"), "\n")...)
		chat = append(chat, "")
	}

	// Extension notes (notify / display) live just under the
	// transcript, above the dialog/editor band. Cleared by /clear.
	if len(i.extNotes) > 0 {
		chat = append(chat, i.extNotes...)
		chat = append(chat, "")
	}

	// Reload failures remain in the scrolling chat until /clear. They are
	// host-only display state, not provider messages or persisted context.
	// While the latest failure is still the temporary status error, hide its
	// stored copy to avoid painting the same message twice.
	reloadErrors := i.reloadErrors
	if n := len(reloadErrors); n > 0 && reloadErrors[n-1] == i.statusErr {
		reloadErrors = reloadErrors[:n-1]
	}
	if len(reloadErrors) > 0 {
		chat = append(chat, renderReloadErrors(i.cfg.Theme, reloadErrors, cols)...)
		chat = append(chat, "")
	}

	// Strip trailing blank rows so the chat content sits flush
	// against the new "blank above status bar" row added by the
	// bottom-region assembly. Build() ends every message with a
	// blank separator; without this trim, the final message in
	// the transcript would have its own trailing blank plus the
	// status block's leading blank, doubling the gap.
	for len(chat) > 0 && strings.TrimSpace(chat[len(chat)-1]) == "" {
		chat = chat[:len(chat)-1]
	}
	return chat
}
func (i *Interactive) sessionsRoot() string {
	if i.cfg.SessionsRoot != "" {
		return i.cfg.SessionsRoot
	}
	return i.cfg.ZutHome
}
func (i *Interactive) lastCols() int {
	cols, _ := i.cfg.Terminal.Size()
	return cols
}
func (i *Interactive) chatPage() int {
	_, rows := i.cfg.Terminal.Size()
	p := rows - 6 // rough reservation for status + editor + a dialog line
	if p < 4 {
		p = 4
	}
	return p
}
func (i *Interactive) scrollBy(delta int) {
	i.mu.Lock()
	i.scrollOffset += delta
	if i.scrollOffset < 0 {
		i.scrollOffset = 0
	}
	if i.scrollOffset == 0 {
		i.parkedTurn = 0
		i.parkedTotal = 0
	}
	if i.rend != nil {
		// VS Code's terminal is especially prone to leaving stray
		// wrapped-character fragments behind during scroll-driven
		// viewport changes. Force a full repaint on scroll, but
		// avoid a whole-screen clear because that visibly flickers.
		i.requestRendererInvalidate()
	}
	i.mu.Unlock()
	i.invalidate()
}
func anchorScrollOffset(offset, prevLen, newLen, prevRows, newRows int) int {
	adj := (newLen - prevLen) - (newRows - prevRows)
	offset += adj
	if offset < 0 {
		offset = 0
	}
	if offset > newLen {
		offset = newLen
	}
	return offset
}
func (i *Interactive) scrollToBottom() {
	i.mu.Lock()
	i.scrollOffset = 0
	i.parkedTurn = 0
	i.parkedTotal = 0
	// Reset the auto-follow baseline. scrollToBottom is the resume /
	// session-swap snap point, where the chat buffer changes length
	// wholesale. Without zeroing these, the next render's follow guard
	// compares the fresh transcript's length against a stale baseline
	// and nudges scrollOffset, which reads as a viewport jump right
	// after resume. See commit 43da5e5 for the same fix on new turns.
	i.prevChatLen = 0
	i.prevChatCols = 0
	i.requestRendererInvalidate()
	i.mu.Unlock()
	i.invalidate()
}
func (i *Interactive) redraw() {
	i.mu.Lock()
	renderRevision := i.renderRevision.Load()

	cols, rows := i.cfg.Terminal.Size()
	mainCols := cols
	rightBarWidgets := i.rightBarWidgetsLocked()
	rightBarActive := false
	var rightBarWidth int
	if !i.rightBarHidden && len(rightBarWidgets) > 0 {
		if width, rail, ok := tui.RightBarColumns(cols); ok {
			mainCols = width
			rightBarWidth = rail
			rightBarActive = true
		}
	}
	rightBarFallback := len(rightBarWidgets) > 0 && !rightBarActive
	i.renderOutsideLock = true
	chat := i.cachedChatLocked(mainCols)
	i.renderOutsideLock = false
	if i.renderRevision.Load() != renderRevision {
		i.mu.Unlock()
		return
	}

	// Independent views render into a floating pane over the live transcript.
	// The pane owns its geometry and viewport; each view retains its own local
	// selection, editor, and asynchronous state.
	paneMax := tui.FloatingPaneMaxRect(cols, rows)
	dialogWidth := paneMax.ContentWidth()
	var dialog []string
	var dialogID string
	switch {
	case i.dialog.Active():
		dialogID = "login"
		dialog = i.dialog.Render(i.cfg.Theme, dialogWidth)
	case i.modelDialog.Active():
		dialogID = "model"
		dialog = i.modelDialog.Render(i.cfg.Theme, dialogWidth)
	case i.llamaDialog.Active():
		dialogID = "llama"
		dialog = i.llamaDialog.Render(i.cfg.Theme, dialogWidth)
	case i.rescueDialog.Active():
		dialogID = "rescue"
		dialog = i.rescueDialog.Render(i.cfg.Theme, dialogWidth)
	case i.sessionDialog.Active():
		dialogID = "sessions"
		i.sessionDialog.MaxRows = max(3, paneMax.ContentHeight()-5)
		dialog = i.sessionDialog.Render(i.cfg.Theme, dialogWidth)
	case i.residentSubagentsDialog.Active():
		dialogID = "subagents"
		dialog = i.residentSubagentsDialog.Render(i.cfg.Theme, dialogWidth, paneMax.ContentHeight())
	case i.residentChildSession != nil:
		dialogID = "subagent-session"
		dialog = i.residentChildSession.Render(dialogWidth, paneMax.ContentHeight())
	case i.jumpDialog.Active():
		dialogID = "jump"
		dialog = i.jumpDialog.Render(i.cfg.Theme, dialogWidth)
	case i.extPanel.Active() && (!i.confirmDialog.Active() || !i.confirmDialog.Focused()):
		dialogID = "extension-panel"
		dialog = i.extPanel.Render(i.cfg.Theme, dialogWidth)
	case i.confirmDialog.Active():
		dialogID = "confirmation"
		if i.btwDialog.Active() {
			dialog = renderBtwConfirmation(i.cfg.Theme, dialogWidth, i.btwDialog, i.confirmDialog)
		} else {
			dialog = i.confirmDialog.Render(i.cfg.Theme, dialogWidth)
		}
	case i.btwDialog.Active():
		dialogID = "side-chat"
		dialog = i.btwDialog.Render(i.cfg.Theme, dialogWidth)
	case i.skillsDialog.Active():
		dialogID = "skills"
		if i.skillsDialog.Viewing() {
			dialogID = "skills-body"
		}
		dialog = i.skillsDialog.Render(i.cfg.Theme, dialogWidth)
	case i.changelogDialog.Active():
		dialogID = "changelog"
		dialog = i.changelogDialog.Render(i.cfg.Theme, dialogWidth)
	case i.logoutDialog.Active():
		dialogID = "logout"
		dialog = i.logoutDialog.Render(i.cfg.Theme, dialogWidth)
	case i.telegramDialog.Active():
		dialogID = "telegram"
		dialog = i.telegramDialog.Render(i.cfg.Theme, dialogWidth)
	case i.settingsDialog.Active():
		dialogID = "settings"
		dialog = i.settingsDialog.Render(i.cfg.Theme, dialogWidth)
	case i.sessionOpsDialog.Active():
		dialogID = "session-ops"
		dialog = i.sessionOpsDialog.Render(i.cfg.Theme, dialogWidth)
	case i.sessionTreeDialog.Active():
		dialogID = "session-tree"
		i.sessionTreeDialog.MaxRows = max(3, paneMax.ContentHeight()-5)
		dialog = i.sessionTreeDialog.Render(i.cfg.Theme, dialogWidth)
	}
	dialogTitle := floatingOverlayTitle(dialogID)
	dialogRemovedTopRows := 0
	if len(dialog) > 0 {
		dialog = padDialogFrame(dialog)
		frameTitle, body, removedTopRows := floatingDialogBody(dialog)
		if frameTitle != "" {
			dialogTitle = frameTitle
		}
		dialog, dialogRemovedTopRows = body, removedTopRows
	}
	dialogFocusRow := -1
	for row, line := range dialog {
		if strings.Contains(line, i.cfg.Theme.SelectionStyle()) {
			dialogFocusRow = row
			break
		}
	}
	modalBackdrop := len(dialog) > 0

	// Slash-command autocomplete: popup above the status line, only
	// when the editor starts with "/" and no dialog is already open.
	// Feed extension-registered commands into the suggester first so
	// they show up in tab-complete + the popup alongside the built-ins.
	i.suggest.SetJailed(i.cfg.Sandbox.Locked())
	i.suggest.SetLlamaConfigured(i.llamaConfigured)
	if i.cfg.Extensions != nil {
		catalog := i.cfg.Extensions.Commands()
		extra := make([]slashCommand, 0, len(catalog))
		for _, c := range catalog {
			// The popup renders extension commands under a dedicated
			// "── extensions ───" divider, so the description doesn't
			// need to repeat the source. If the description is empty,
			// fall back to the extension name so the row isn't blank.
			desc := c.Description
			if strings.TrimSpace(desc) == "" {
				desc = "(" + c.Extension + ")"
			}
			extra = append(extra, slashCommand{
				Name: "/" + c.Name,
				Desc: desc,
			})
		}
		i.suggest.SetExtra(extra)
	}
	var suggest []string
	currentInput := i.ed.Value()
	if i.suggest.SkillInputStarted(currentInput) {
		var list []*skills.Skill
		if i.cfg.SkillSnapshot != nil {
			list = i.cfg.SkillSnapshot()
		}
		i.suggest.SetSkills(list)
	}
	// Slash popup renders even while the agent is busy so the user
	// can queue a destructive command (/clear, /compact, /logout,
	// /model) or a read-only one (/help, /jump, /sessions, etc.)
	// without waiting for the current turn to finish. The dispatcher
	// in runSlash already handles the busy case per-command: safe
	// ones run immediately, destructive ones cancel the turn first.
	i.fileSuggest.SetCWD(i.cfg.CWD)
	mainInputFocused := len(dialog) == 0 || (i.confirmDialog.Active() && !i.confirmDialog.Focused() && !i.confirmChildActiveLocked())
	if mainInputFocused && i.suggest.Active(currentInput) {
		suggest = i.suggest.Render(currentInput, i.cfg.Theme, mainCols)
	} else if mainInputFocused && i.fileSuggest.Active(currentInput) {
		suggest = i.fileSuggest.Render(currentInput, i.cfg.Theme, mainCols)
	}

	// Detect overlay close (any dialog or slash/file suggestion popup
	// just transitioned from open to closed). Force a full redraw so
	// the rows the overlay occupied are guaranteed to be repainted
	// from the chat below, instead of the diff path leaving stale
	// dialog content behind. Equivalent to the user pressing ctrl+l.
	overlayOpen := len(dialog) > 0 || len(suggest) > 0
	if i.rend != nil && i.prevOverlayOpen && !overlayOpen {
		// An overlay (dialog or slash/file popup) just closed, so the
		// bottom band shrinks. On terminals where we can drop
		// scrollback, a full Clear is the simplest way to guarantee
		// the vacated rows are repainted from the chat below.
		//
		// On VS Code's terminal closing a dialog leaves the stale
		// overlay rows in the retained scrollback (we can't drop them
		// with the quiet in-place diff). Run the same full Clear() that
		// Ctrl+L uses so the scrollback is purged and the conversation
		// is repainted clean, matching what the user expects after
		// dismissing a picker. Clear() is keepScrollback-aware and
		// emits \x1b[3J there.
		i.rend.Clear()
	}
	i.prevOverlayOpen = overlayOpen
	if len(suggest) > 0 {
		// One blank row above the popup so it doesn't sit flush
		// against the chat / welcome content above.
		suggest = append([]string{""}, suggest...)
	}

	// Busy prefix shown at the far left of the status bar. The spinner owns
	// only its frame; the activity label reports the operation in progress.
	var busyPrefix string
	if i.busy {
		label := i.activity.label()
		if label == "" {
			label = "Preparing request"
		}
		busyPrefix = fmt.Sprintf("%s %s %s %s",
			i.cfg.Theme.FGColor(i.cfg.Theme.Assistant, i.spin.Frame()),
			i.cfg.Theme.FGColor(i.cfg.Theme.Assistant, label),
			i.cfg.Theme.FGColor(i.cfg.Theme.Muted, "-"),
			i.cfg.Theme.FGColor(i.cfg.Theme.Muted, i.spin.Elapsed().String()),
		)
	}

	ctxMax := 0
	if m, err := provider.FindModel(i.cfg.Provider, i.cfg.Model); err == nil {
		ctxMax = m.ContextWindow
	}
	statusPosition := tui.NormalizeStatusPosition(i.cfg.TUIStatusPosition)
	workingPosition := tui.NormalizeWorkingPosition(i.cfg.TUIWorkingPosition)
	workingWithStatus := statusPosition == workingPosition
	statusBusyPrefix := ""
	if workingWithStatus {
		statusBusyPrefix = busyPrefix
	}
	goalStatus := string(i.goalStatus)
	statusLines := tui.StatusBar(tui.StatusBarParams{
		Theme:          i.cfg.Theme,
		Provider:       i.cfg.Provider,
		Model:          i.cfg.Model,
		Reasoning:      i.cfg.Reasoning,
		FastMode:       i.cfg.FastMode != nil && *i.cfg.FastMode,
		Busy:           i.busy,
		BusyPrefix:     statusBusyPrefix,
		CWD:            i.cfg.CWD,
		Locked:         i.cfg.Sandbox.Locked(),
		NoYolo:         i.cfg.NoYolo,
		GoalStatus:     goalStatus,
		Usage:          i.cumUsage,
		Subscription:   i.cfg.AuthMethod == "oauth",
		ContextUsed:    i.lastCtxInput,
		ContextMax:     ctxMax,
		AutoCompacting: i.autoCompacting,
		Telegram:       i.telegramBridge != nil && i.telegramBridge.Active(),
		Cols:           mainCols,
	})
	inputStyle := tui.NormalizeInputStyle(i.cfg.TUIInputStyle)
	if inputStyle == tui.InputStyleLines || inputStyle == tui.InputStyleBlock {
		i.ed.Prompt = ""
	} else {
		i.ed.Prompt = i.cfg.Theme.AccentBar(i.cfg.Theme.Accent)
	}
	edLines, curR, curC := i.ed.Render(mainCols)
	// Floating independent views no longer replace the main bottom band: the
	// underlying chat, status, and editor keep updating behind the pane.
	residentViewActive := i.residentSubagentsDialog.Active() || i.residentChildSession != nil
	var allResidentSubagentLines []string
	residentSubagentHidden := 0
	if !residentViewActive && i.cfg.ResidentManager != nil {
		const residentIndicatorSnapshotLimit = 8
		snapshots, total := i.cfg.ResidentManager.ActiveSnapshotPage(residentIndicatorSnapshotLimit)
		allResidentSubagentLines = renderResidentSubagentActivityLines(i.cfg.Theme, i.spin.FrameAt(i.clock()), snapshots, mainCols, i.clock())
		residentSubagentHidden = total - len(snapshots)
	}
	var workingLines []string
	if busyPrefix != "" && !workingWithStatus {
		workingLines = []string{"  " + busyPrefix}
	}
	inputCursorOffset := 0
	inputCursorColOffset := 0
	switch inputStyle {
	case tui.InputStyleLines:
		edLines = tui.InputLines(i.cfg.Theme, edLines, mainCols)
		inputCursorOffset = 1
	case tui.InputStyleBlock:
		edLines = tui.InputBlock(i.cfg.Theme, edLines, mainCols)
		inputCursorOffset = 1
		inputCursorColOffset = 2
	}

	// "Sliding in" chips for messages the user typed while a turn is
	// in flight. Shown directly above the status bar so they're close
	// to the editor but don't push the chat around.
	var queue []string
	queued := append([]core.QueuedMessage(nil), i.queued...)
	if i.agent != nil {
		queued = append(queued, i.agent.PendingQueuedMessages()...)
	}
	if len(queued) > 0 {
		queue = append(queue, "")
		for _, q := range queued {
			label := i.cfg.Theme.FGColor(i.cfg.Theme.Accent, "  sliding in: ")
			text := queuedMessageSummary(q, mainCols-17)
			queue = append(queue, label+i.cfg.Theme.FGColor(i.cfg.Theme.Muted, text))
		}
		// Hint row, rendered in the same muted tone as the model
		// info on the status bar so it reads as ambient metadata
		// rather than a chip. Tells the user how to recover the
		// most recent queued message back into the editor.
		hint := "  Press " + slideBackChordHint() + " to slide back into input"
		queue = append(queue, i.cfg.Theme.FGColor(i.cfg.Theme.Muted, hint))
	}

	extensionLines := i.extensionChromeLinesForLayoutLocked(mainCols, rightBarActive, rightBarFallback)

	// Bottom-sticky sections (always visible, never scroll). Each
	// non-empty subsection (dialog, suggest popup, sliding-in queue)
	// is preceded by one blank row so it has air above the chat
	// content. The status block and editor get their own dedicated
	// blanks so spacing stays consistent whether or not a dialog or
	// popup is showing.
	composeBottom := func(residentSubagentLines []string) (bottom []string, inputStartRow int) {
		bottom = make([]string, 0, len(suggest)+len(queue)+len(extensionLines)+len(statusLines)+len(residentSubagentLines)+len(edLines)+9)
		bottom = append(bottom, suggest...)
		bottom = append(bottom, queue...)
		lineInput := inputStyle == tui.InputStyleLines
		statusBelow := statusPosition == tui.StatusPositionBelowInput
		workingBelow := workingPosition == tui.WorkingPositionBelowInput
		var aboveInput []string
		aboveInput = append(aboveInput, extensionLines...)
		if !statusBelow {
			aboveInput = append(aboveInput, statusLines...)
		}
		if !workingBelow {
			aboveInput = append(aboveInput, workingLines...)
		}
		var belowInput []string
		// Background resident work belongs directly below the editor. It is
		// deliberately independent of the main turn's busy/status placement.
		belowInput = append(belowInput, residentSubagentLines...)
		if workingBelow {
			belowInput = append(belowInput, workingLines...)
		}
		if statusBelow {
			belowInput = append(belowInput, statusLines...)
		}

		bottom = append(bottom, "")
		bottom = append(bottom, aboveInput...)
		needsGapAboveInput := len(aboveInput) > 0 && !lineInput
		if lineInput && statusBelow && !workingBelow && len(workingLines) > 0 {
			needsGapAboveInput = true
		}
		if needsGapAboveInput {
			bottom = append(bottom, "")
		}
		inputStartRow = len(bottom)
		bottom = append(bottom, edLines...)
		if len(belowInput) > 0 {
			// Active resident work sits immediately below the prompt. Other
			// below-input chrome keeps the usual breathing room.
			if !lineInput && len(residentSubagentLines) == 0 {
				bottom = append(bottom, "")
			}
			bottom = append(bottom, belowInput...)
		}
		return bottom, inputStartRow
	}

	const rendererBottomMarginRows = 1
	maxBottomRows := rows - rendererBottomMarginRows - 1
	var residentSubagentLines []string
	var bottom []string
	var inputStartRow int
	if len(allResidentSubagentLines) > 0 {
		for maxRows := len(allResidentSubagentLines) + 1; maxRows >= 0; maxRows-- {
			candidate := limitResidentSubagentActivityLines(i.cfg.Theme, allResidentSubagentLines, residentSubagentHidden, maxRows, mainCols)
			candidateBottom, _ := composeBottom(candidate)
			if len(candidateBottom) <= maxBottomRows || maxRows == 0 {
				residentSubagentLines = candidate
				break
			}
		}
	}
	bottom, inputStartRow = composeBottom(residentSubagentLines)

	chatRows := rows - len(bottom)
	if chatRows < 1 {
		chatRows = 1
	}

	// Auto-follow guard: when the user has scrolled up (scrollOffset
	// > 0) and the agent appends new content below the viewport while
	// they're reading, compensate so the visible content stays
	// anchored. scrollOffset is measured from the bottom of `chat`,
	// so without compensation a growing buffer pushes the window
	// downward through the content and the lines the user was
	// reading scroll off the top.
	//
	// Skip compensation when the terminal width changed (a resize
	// reflows the whole buffer and the line-count delta no longer
	// corresponds to appended content) and when scrollOffset is 0
	// (the user is following the tail and wants new content to push
	// the view down as usual).
	//
	// The window the user sees starts at row
	//   start = len(chat) - scrollOffset - chatRows
	// so to keep `start` fixed across a redraw we must offset by both
	// the buffer growth (len delta) AND the viewport-height change
	// (chatRows delta, e.g. the status band or sliding-in queue
	// appearing during a turn). Compensating only for the len delta
	// let a shrinking chatRows pull the window toward the tail, which
	// read as the viewport jumping to the bottom whenever the agent
	// streamed text or a tool call grew the bottom band.
	if i.scrollOffset > 0 && i.prevChatCols == mainCols && i.prevChatLen > 0 {
		i.scrollOffset = anchorScrollOffset(i.scrollOffset,
			i.prevChatLen, len(chat), i.prevChatRows, chatRows)
	}
	i.prevChatLen = len(chat)
	i.prevChatCols = mainCols
	i.prevChatRows = chatRows

	// Apply scroll offset to the chat slice.
	maxOffset := len(chat) - chatRows
	if maxOffset < 0 {
		maxOffset = 0
	}
	// Tail-render expansion: if the user has scrolled to (or above)
	// the top of the currently rendered tail and there are still
	// truncated messages above, widen view.TailLimit and rebuild.
	// The chat cache is keyed on tailLimit so the next cachedChatLocked
	// will re-issue Build instead of returning the stale slice. We
	// rebuild immediately so the same redraw shows the freshly-revealed
	// rows; otherwise the user would have to scroll again to see them.
	if i.scrollOffset >= maxOffset && i.view.TailLimit > 0 && i.view.TailLimit < len(i.view.Messages) {
		prevLen := len(chat)
		i.view.TailLimit += resumeTailExpandStep
		if i.view.TailLimit >= len(i.view.Messages) {
			i.view.TailLimit = 0 // unbounded
		}
		i.chatCacheValid = false
		chat = i.cachedChatLocked(mainCols)
		for len(chat) > 0 && strings.TrimSpace(chat[len(chat)-1]) == "" {
			chat = chat[:len(chat)-1]
		}
		// Newly-revealed rows are older messages prepended at the top.
		// scrollOffset counts rows from the bottom, so to keep the
		// viewport visually anchored on the same content the user was
		// looking at we shift it up by the number of rows added. Keep
		// the auto-follow baseline (prevChatLen) in sync with the
		// post-expansion length too, so the next render's follow guard
		// doesn't see this growth as a synthetic delta and yank the
		// viewport again.
		if grew := len(chat) - prevLen; grew > 0 {
			i.scrollOffset += grew
		}
		i.prevChatLen = len(chat)
		i.prevChatCols = mainCols
		maxOffset = len(chat) - chatRows
		if maxOffset < 0 {
			maxOffset = 0
		}
	}
	if i.scrollOffset > maxOffset {
		i.scrollOffset = maxOffset
	}
	if i.scrollOffset < 0 {
		i.scrollOffset = 0
	}

	var visibleChat []string
	if len(chat) <= chatRows {
		visibleChat = chat
	} else {
		end := len(chat) - i.scrollOffset
		rawStart := end - chatRows
		if rawStart < 0 {
			rawStart = 0
		}
		start := snapViewportStartToImageBlock(chat, rawStart)
		// If the snap pulled start upward (an image-block was atomic) while
		// the user is scrolling downward, the viewport would sit on the same
		// image until the user mashes down past every reserved row. Bump
		// scrollOffset past the image so one keypress always clears it.
		if start < rawStart && i.scrollOffset < i.prevScrollOffset {
			jump := rawStart - start
			i.scrollOffset -= jump
			if i.scrollOffset < 0 {
				i.scrollOffset = 0
			}
			end = len(chat) - i.scrollOffset
			rawStart = end - chatRows
			if rawStart < 0 {
				rawStart = 0
			}
			start = snapViewportStartToImageBlock(chat, rawStart)
		}
		end = start + chatRows
		if end > len(chat) {
			end = len(chat)
			start = end - chatRows
			if start < 0 {
				start = 0
			}
		}
		visibleChat = chat[start:end]
	}
	i.prevScrollOffset = i.scrollOffset
	visibleChat = clipBottomClippedImages(visibleChat)

	// A tiny "scrolled up" indicator in the top-right of the chat pane
	// so you know you're not at the bottom. When the viewport was
	// parked by /jump we include the turn number so the user remembers
	// they're reading history rather than the live conversation.
	if i.scrollOffset > 0 && len(visibleChat) > 0 {
		var text string
		if i.parkedTurn > 0 && i.parkedTotal > 0 {
			text = fmt.Sprintf("  ↑ viewing turn %d of %d - %d lines more below (pgdn / end)",
				i.parkedTurn, i.parkedTotal, i.scrollOffset)
		} else {
			text = fmt.Sprintf("  ↑ %d lines more below (end to jump)", i.scrollOffset)
		}
		note := i.cfg.Theme.FGColor(i.cfg.Theme.Muted, text)
		visibleChat = append([]string{note}, visibleChat...)
		if len(visibleChat) > chatRows {
			visibleChat = visibleChat[:chatRows]
		}
	}

	// Default: the real terminal cursor sits on the main editor's
	// input position. In main-screen log mode cursor rows are relative
	// to the fixed bottom band, not the chat transcript.
	// Floating content has its own coordinate space, so dialog cursor rows
	// remain relative to the content passed to FloatingPane.Compose.
	dialogLead := 0
	cursorRow := inputStartRow + inputCursorOffset + curR
	cursorCol := curC + inputCursorColOffset
	cursorInDialog := false
	if i.btwDialog.Active() {
		if r, c := i.btwDialog.CursorPos(dialogWidth); r >= 0 {
			cursorRow = dialogLead + r
			cursorCol = c
			cursorInDialog = true
		}
	}
	if i.dialog.Active() {
		if r, c := i.dialog.CursorPos(dialogWidth); r >= 0 {
			cursorRow = dialogLead + r
			cursorCol = c
			cursorInDialog = true
		}
	}
	if i.llamaDialog.Active() {
		if r, c := i.llamaDialog.CursorPos(); r >= 0 {
			cursorRow = dialogLead + r
			cursorCol = c
			cursorInDialog = true
		} else {
			cursorRow = -1
			cursorCol = 0
		}
	}
	if i.sessionDialog.Active() {
		if r, c := i.sessionDialog.CursorPos(); r >= 0 {
			cursorRow = dialogLead + r
			cursorCol = c
			cursorInDialog = true
		}
	}
	if i.sessionTreeDialog.Active() {
		if r, c := i.sessionTreeDialog.CursorPos(); r >= 0 {
			cursorRow = dialogLead + r
			cursorCol = c
			cursorInDialog = true
		}
	}
	if i.extPanel.Active() {
		cursorRow = -1
		cursorCol = 0
	}
	dialogCursorRow, dialogCursorCol := dialogFocusRow, 0
	if cursorInDialog {
		dialogCursorRow, dialogCursorCol = cursorRow-dialogRemovedTopRows, cursorCol
		if dialogCursorRow < 0 {
			dialogCursorRow = -1
		}
	}
	i.setInputCursorDimmed(modalBackdrop && !cursorInDialog)
	if i.renderRevision.Load() != renderRevision {
		i.mu.Unlock()
		return
	}
	theme := i.cfg.Theme
	// State assembly above is synchronized; terminal output is owned by
	// this renderer goroutine and must not hold the global state mutex.
	i.mu.Unlock()
	if modalBackdrop {
		background := floatingBackgroundFrame(visibleChat, bottom, rows)
		if rightBarActive {
			rightBar := tui.RenderRightBar(theme, rightBarWidgets, rightBarWidth, rows)
			background = floatingRightBarFrame(theme, background, rightBar, mainCols, rightBarWidth)
		}
		pane, paneCursorRow, paneCursorCol := i.floatingPane.Compose(theme, dialogID, dialogTitle, background, dialog, cols, rows, dialogCursorRow, dialogCursorCol)
		i.rend.DrawFloating(pane, paneCursorRow, paneCursorCol)
	} else if rightBarActive {
		rightBar := tui.RenderRightBar(theme, rightBarWidgets, rightBarWidth, rows)
		i.rend.DrawRightBar(visibleChat, bottom, rightBar, cursorRow, cursorCol)
	} else {
		_ = visibleChat // maintained for legacy scroll state/indicators; DrawLog owns chat viewport.
		i.rend.DrawLog(chat, bottom, cursorRow, cursorCol)
	}
	i.mu.Lock()
	if i.pendingAlert != nil && !i.busy && !i.streamOn && !i.streamFlushPending && len(i.streamPending) == 0 {
		alert := *i.pendingAlert
		i.pendingAlert = nil
		i.emitAlertLocked(alert)
	}
	i.mu.Unlock()
}

// floatingOverlayTitle supplies every overlay with a stable, human-readable
// title even when its body does not use the legacy frame-header convention.
func floatingOverlayTitle(id string) string {
	return strings.ReplaceAll(id, "-", " ")
}

// floatingDialogBody removes a legacy dialog's rule chrome and returns its
// title for the shared floating-pane border.
func floatingDialogBody(lines []string) (title string, body []string, removedTopRows int) {
	if len(lines) == 0 {
		return "", nil, 0
	}
	body = lines
	if isFrameHeaderLine(body[0]) {
		plain := strings.TrimPrefix(stripANSIBytes(body[0]), "── ")
		title = strings.TrimSpace(strings.TrimRight(plain, "─"))
		body = body[1:]
		removedTopRows = 1
	}
	if last := len(body) - 1; last >= 0 && isFrameRuleLine(body[last]) {
		body = body[:last]
	}
	return title, body, removedTopRows
}

// floatingBackgroundFrame preserves the regular viewport layout while an
// independent view is open. It is a fixed terminal frame only for the overlay
// lifetime; normal rendering immediately returns to scrollback-oriented flow
// when the pane closes.
func floatingBackgroundFrame(chat, bottom []string, rows int) []string {
	if rows < 1 {
		return nil
	}
	// Match DrawLog's logical order and renderer-owned bottom margin, then
	// retain exactly the terminal viewport. In particular, an editor/status
	// band taller than the terminal must show its newest rows, not its oldest.
	lines := make([]string, 0, len(chat)+len(bottom)+1)
	lines = append(lines, chat...)
	lines = append(lines, bottom...)
	lines = append(lines, "")
	if len(lines) > rows {
		lines = lines[len(lines)-rows:]
	}
	frame := make([]string, rows)
	// DrawLog's flow renderer leaves the logical session content at the
	// viewport's top. Keep that anchoring when switching to a fixed floating
	// frame so opening a pane does not visibly jump the whole session down.
	copy(frame, lines)
	return frame
}

func floatingRightBarFrame(theme tui.Theme, main, rightBar []string, mainWidth, rightBarWidth int) []string {
	for row := range main {
		barLine := ""
		if row < len(rightBar) {
			barLine = rightBar[row]
		}
		main[row] = tui.JoinRightBar(theme, main[row], barLine, mainWidth, rightBarWidth)
	}
	return main
}

func hasImageEscape(line string) bool {
	return strings.Contains(line, "\x1b]1337;File=") || strings.Contains(line, "\x1b_G")
}
func snapViewportStartToImageBlock(chat []string, start int) int {
	if start <= 0 || start >= len(chat) {
		return start
	}
	if hasImageEscape(chat[start]) || !isBoxBlankLine(chat[start]) {
		return start
	}
	for k := start - 1; k >= 0; k-- {
		line := chat[k]
		if hasImageEscape(line) {
			return k
		}
		if !isBoxBlankLine(line) {
			break
		}
	}
	return start
}
func filterHiddenTranscriptMessages(msgs []provider.Message) []provider.Message {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]provider.Message, 0, len(msgs))
	for _, m := range msgs {
		if isHiddenTranscriptMessage(m) {
			continue
		}
		out = append(out, m)
	}
	return out
}
func isHiddenTranscriptMessage(m provider.Message) bool {
	if m.Meta[autoCompactContinueMetaKey] == "true" || m.Meta[goalContinueMetaKey] == "true" {
		return true
	}
	if m.Role != provider.RoleUser || len(m.Content) == 0 {
		return false
	}
	tb, ok := m.Content[0].(provider.TextBlock)
	if !ok {
		return false
	}
	return strings.TrimSpace(tb.Text) == hiddenOpenAIImageMirrorPrefix
}
func clipBottomClippedImages(lines []string) []string {
	if len(lines) == 0 {
		return lines
	}
	out := append([]string(nil), lines...)
	for i, line := range out {
		if !hasImageEscape(line) {
			continue
		}
		// Image blocks render as: image escape, zero or more blank
		// reservation rows, then the muted "image - ..." info line,
		// then one trailing blank. If the info line isn't visible in
		// the current chat slice, the image would paint down into the
		// fixed status bar area. Suppress that image for this frame.
		//
		// When the image lives inside a tool box, the reservation rows
		// are wrapped in vertical box edges ("│  ...  │"); those rows
		// look non-blank under a naive whitespace check but are still
		// reservation rows for this scan, so treat them as blank.
		foundInfo := false
		for j := i + 1; j < len(out); j++ {
			if strings.Contains(out[j], "image - ") {
				foundInfo = true
				break
			}
			if !isBoxBlankLine(out[j]) {
				break
			}
		}
		if !foundInfo {
			out[i] = ""
		}
	}
	return out
}
func isBoxBlankLine(line string) bool {
	stripped := stripANSIBytes(line)
	stripped = strings.TrimSpace(stripped)
	stripped = strings.Trim(stripped, "│")
	stripped = strings.TrimSpace(stripped)
	return stripped == ""
}
func stripANSIBytes(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			end := i + 2
			for end < len(s) {
				c := s[end]
				end++
				if c >= 0x40 && c <= 0x7e {
					break
				}
			}
			i = end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
func panelKeyName(k tui.Key) string {
	switch k.Kind {
	case tui.KeyUp:
		return "up"
	case tui.KeyDown:
		return "down"
	case tui.KeyLeft:
		return "left"
	case tui.KeyRight:
		return "right"
	case tui.KeyEnter:
		return "enter"
	case tui.KeyEsc:
		return "esc"
	case tui.KeyTab:
		return "tab"
	case tui.KeyBackspace:
		return "backspace"
	case tui.KeyDelete:
		return "delete"
	case tui.KeyHome:
		return "home"
	case tui.KeyEnd:
		return "end"
	case tui.KeyPageUp:
		return "pageup"
	case tui.KeyPageDown:
		return "pagedown"
	case tui.KeyRune:
		return "rune"
	default:
		return "unknown"
	}
}
func panelKeyText(k tui.Key) string {
	if k.Kind == tui.KeyRune {
		return string(k.Rune)
	}
	return ""
}
func truncateLine(s string, n int) string {
	if n <= 0 {
		return ""
	}
	// Collapse newlines — chips are single line.
	s = strings.ReplaceAll(s, "\n", " ↩ ")
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 3 {
		return strings.Repeat(".", n)
	}
	return string(runes[:n-3]) + "..."
}
