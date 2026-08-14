package modes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

const sessionTreeEscapeWindow = 500 * time.Millisecond

type doubleEscapeTracker struct {
	last  time.Time
	armed bool
}

func (d *doubleEscapeTracker) Arm(now time.Time) {
	d.last = now
	d.armed = true
}

// Consume reports whether now is the second tap of a valid gesture. A clock
// that moves backwards is treated as a reset rather than as a valid tap.
func (d *doubleEscapeTracker) Consume(now time.Time) bool {
	last := d.last
	armed := d.armed
	d.last = time.Time{}
	d.armed = false
	if !armed {
		return false
	}
	elapsed := now.Sub(last)
	return elapsed >= 0 && elapsed <= sessionTreeEscapeWindow
}

func (d *doubleEscapeTracker) Reset() {
	d.last = time.Time{}
	d.armed = false
}

func isUnmodifiedEscape(k tui.Key) bool {
	return k.Kind == tui.KeyEsc && !k.Ctrl && !k.Alt && !k.Shift && !k.Super
}

func (i *Interactive) sessionTreeEscapeNow() time.Time {
	if i.clock != nil {
		return i.clock()
	}
	return time.Now()
}

// currentSessionPath is the nil-safe session callback shared by slash
// commands and tree operations. Running without persistence (for example
// --no-session) is a supported mode, so callers must treat a missing
// callback exactly like an empty path rather than invoking it blindly.
func (i *Interactive) currentSessionPath() string {
	if i == nil || i.cfg.CurrentSessionPath == nil {
		return ""
	}
	return i.cfg.CurrentSessionPath()
}

// sessionTreeEscapeBlocked reports interactions that must retain precedence
// over the gesture. It is intentionally broader than the overlay switch in
// handleKey: notes and suggestions also own Escape even though they are not
// all rendered as dialogs.
func (i *Interactive) sessionTreeEscapeBlocked() bool {
	if i == nil {
		return true
	}
	if i.ed == nil {
		return true
	}
	if i.dialog != nil && i.dialog.Active() ||
		i.modelDialog != nil && i.modelDialog.Active() ||
		i.llamaDialog != nil && i.llamaDialog.Active() ||
		i.rescueDialog != nil && i.rescueDialog.Active() ||
		i.sessionDialog != nil && i.sessionDialog.Active() ||
		i.residentSubagentsDialog != nil && i.residentSubagentsDialog.Active() ||
		i.residentChildSession != nil ||
		i.jumpDialog != nil && i.jumpDialog.Active() ||
		i.btwDialog != nil && i.btwDialog.Active() ||
		i.skillsDialog != nil && i.skillsDialog.Active() ||
		i.changelogDialog != nil && i.changelogDialog.Active() ||
		i.logoutDialog != nil && i.logoutDialog.Active() ||
		i.telegramDialog != nil && i.telegramDialog.Active() ||
		i.settingsDialog != nil && i.settingsDialog.Active() ||
		i.sessionOpsDialog != nil && i.sessionOpsDialog.Active() ||
		i.sessionTreeDialog != nil && i.sessionTreeDialog.Active() ||
		i.extPanel != nil && i.extPanel.Active() ||
		i.confirmDialog != nil && i.confirmDialog.Active() {
		return true
	}
	if i.suggest != nil && i.suggest.Active(i.ed.Value()) ||
		i.fileSuggest != nil && i.fileSuggest.Active(i.ed.Value()) {
		return true
	}
	i.mu.Lock()
	blocked := len(i.helpBlock) > 0 || len(i.extNotes) > 0
	i.mu.Unlock()
	return blocked
}

// canArmSessionTreeEscape checks only the cheap, local eligibility rules for
// the first tap. The complete family read belongs to canOpenSessionTree on
// the second tap; otherwise a malformed descendant would prevent arming and
// the user would never receive the gate's status error.
func (i *Interactive) canArmSessionTreeEscape() bool {
	if i == nil || i.ed == nil || i.cfg.CWD == "" || i.sessionsRoot() == "" ||
		i.cfg.CurrentSessionPath == nil || i.cfg.LoadSession == nil {
		return false
	}
	if !i.ed.IsEmpty() || len(i.clipboardImages) != 0 || i.sessionTreeEscapeBlocked() {
		return false
	}
	i.mu.Lock()
	busy := i.busy || i.streamOn || i.streamFlushPending || len(i.streamPending) != 0 ||
		i.shellRunning || i.compacting || i.autoCompacting || i.awaitingStartupPre || i.sessionLoading || i.modelRefreshing
	queued := len(i.queued) != 0
	ag := i.agent
	pendingFork := i.pendingFork
	i.mu.Unlock()
	if busy || queued || pendingFork || ag == nil || ag.QueuedMessageCount() != 0 {
		return false
	}
	return i.currentSessionPath() != ""
}

// canOpenSessionTree is the shared, fail-closed precondition for both the
// /session tree command and the bare-Escape gesture. It intentionally avoids
// filesystem work so a slow or large session family cannot stall input; the
// asynchronous dialog loader validates the family after the overlay opens.
func (i *Interactive) canOpenSessionTree() bool {
	if i == nil || i.ed == nil || i.cfg.CWD == "" || i.sessionsRoot() == "" ||
		i.cfg.CurrentSessionPath == nil || i.cfg.LoadSession == nil {
		return false
	}
	if !i.ed.IsEmpty() || len(i.clipboardImages) != 0 || i.sessionTreeEscapeBlocked() {
		return false
	}

	i.mu.Lock()
	busy := i.busy || i.streamOn || i.streamFlushPending || len(i.streamPending) != 0 ||
		i.shellRunning || i.compacting || i.autoCompacting || i.awaitingStartupPre || i.sessionLoading || i.modelRefreshing
	queued := len(i.queued) != 0
	ag := i.agent
	pendingFork := i.pendingFork
	i.mu.Unlock()
	if busy || queued || pendingFork || ag == nil {
		return false
	}
	if ag.QueuedMessageCount() != 0 {
		return false
	}

	return i.currentSessionPath() != ""
}

// canCommitSessionTreeSelection is the open-gate subset used after the tree
// already owns the keyboard. It deliberately permits the active tree dialog;
// reusing canOpenSessionTree here would reject every Enter selection because
// the dialog is itself an overlay.
func (i *Interactive) canCommitSessionTreeSelection() bool {
	if i == nil || i.cfg.CurrentSessionPath == nil || i.cfg.LoadSession == nil || i.sessionsRoot() == "" || i.cfg.CWD == "" {
		return false
	}
	i.mu.Lock()
	busy := i.busy || i.streamOn || i.streamFlushPending || len(i.streamPending) != 0 ||
		i.shellRunning || i.compacting || i.autoCompacting || i.awaitingStartupPre || i.sessionLoading || i.modelRefreshing
	queued := len(i.queued) != 0
	ag := i.agent
	i.mu.Unlock()
	if busy || queued || ag == nil || ag.QueuedMessageCount() != 0 {
		return false
	}
	return i.currentSessionPath() != ""
}

func (i *Interactive) setSessionTreeError(message string) {
	if i.sessionTreeDialog != nil {
		i.sessionTreeDialog.Close()
	}
	i.mu.Lock()
	i.statusErr = message
	i.statusOK = ""
	i.mu.Unlock()
	i.invalidate()
}

// openSessionTree is the only path that activates the session-tree overlay.
// It queues the family read rather than performing it on the input goroutine;
// a failed asynchronous read closes the overlay and reports the usual error.
func (i *Interactive) openSessionTree() bool {
	if !i.canOpenSessionTree() {
		i.setSessionTreeError("tree: session tree is unavailable")
		return false
	}
	current := i.currentSessionPath()
	if current == "" {
		i.setSessionTreeError("tree: no session is active")
		return false
	}
	if i.sessionTreeDialog == nil {
		i.setSessionTreeError("tree: session tree is unavailable")
		return false
	}
	i.mu.Lock()
	loadCtx := i.runCtx
	i.mu.Unlock()
	if loadCtx == nil {
		loadCtx = context.Background()
	}
	i.sessionTreeLoads = i.sessionTreeDialog.OpenSessionFamilyAsync(loadCtx, i.sessionsRoot(), i.cfg.CWD, current, i.cfg.FlushSession)
	i.mu.Lock()
	i.statusErr = ""
	i.statusOK = ""
	i.mu.Unlock()
	i.invalidate()
	return true
}

type sessionTreeSelectionResult struct {
	upTo         int
	restoreDraft bool
	draftText    string
	images       []clipboardImageAttachment
}

// sessionTreeSelection turns a structured dialog target into a branch
// boundary. User rows are boundaries before that message and restore the
// full editable draft; assistant/tool rows include the selected completed
// exchange. Empty and detached rows are already safe message-count
// boundaries. A tool call and its result rows are never split.
func sessionTreeSelection(msgs []provider.Message, target sessionTreeTarget) (sessionTreeSelectionResult, error) {
	if target.IsBoundary() {
		upTo := target.SelectionBoundary
		if upTo < 0 || upTo > len(msgs) || !safeSessionToolBoundary(msgs[:upTo]) {
			return sessionTreeSelectionResult{}, fmt.Errorf("selection splits a tool exchange")
		}
		return sessionTreeSelectionResult{upTo: upTo}, nil
	}
	msgIdx := target.EffectiveIndex
	if msgIdx < 0 || msgIdx >= len(msgs) {
		return sessionTreeSelectionResult{}, fmt.Errorf("selection is outside the session")
	}
	selected := msgs[msgIdx]
	if selected.Role == provider.RoleUser {
		upTo := target.SelectionBoundary
		if upTo < 0 || upTo > len(msgs) || !safeSessionToolBoundary(msgs[:upTo]) {
			return sessionTreeSelectionResult{}, fmt.Errorf("selection splits a tool exchange")
		}
		text, images := sessionTreeDraft(selected, target.UserDraft)
		return sessionTreeSelectionResult{
			upTo:         upTo,
			restoreDraft: true,
			draftText:    text,
			images:       images,
		}, nil
	}

	upTo := target.SelectionBoundary
	if upTo < msgIdx+1 || upTo > len(msgs) {
		return sessionTreeSelectionResult{}, fmt.Errorf("selection is outside the session")
	}
	if selected.Role == provider.RoleAssistant && messageHasToolCall(selected) || selected.Role == provider.RoleTool {
		// A provider tool exchange may occupy one assistant row followed by
		// one or more tool rows. Extend a selected row to the end of that
		// contiguous exchange before checking the boundary.
		for upTo < len(msgs) && msgs[upTo].Role == provider.RoleTool {
			upTo++
		}
	}
	if !safeSessionToolBoundary(msgs[:upTo]) {
		return sessionTreeSelectionResult{}, fmt.Errorf("selection splits a tool exchange")
	}
	return sessionTreeSelectionResult{upTo: upTo}, nil
}

func messageHasToolCall(msg provider.Message) bool {
	for _, content := range msg.Content {
		if _, ok := content.(provider.ToolCallBlock); ok {
			return true
		}
	}
	return false
}

// safeSessionToolBoundary checks the provider's structural invariant at a
// message cut: every tool call in the prefix has its matching result in the
// immediately following tool row, and no result is orphaned.
func safeSessionToolBoundary(msgs []provider.Message) bool {
	pending := map[string]bool{}
	for _, msg := range msgs {
		if len(pending) > 0 && msg.Role != provider.RoleTool {
			return false
		}
		switch msg.Role {
		case provider.RoleAssistant:
			if len(pending) != 0 {
				return false
			}
			for _, content := range msg.Content {
				if call, ok := content.(provider.ToolCallBlock); ok {
					if call.ID == "" || pending[call.ID] {
						return false
					}
					pending[call.ID] = true
				}
			}
		case provider.RoleTool:
			if len(pending) == 0 {
				if len(msg.AddedToolNames) > 0 {
					// Deferred-tool activation rows intentionally carry
					// provider-local state without a preceding tool call.
					continue
				}
				return false
			}
			for _, content := range msg.Content {
				result, ok := content.(provider.ToolResultBlock)
				if !ok || !pending[result.CallID] {
					return false
				}
				delete(pending, result.CallID)
			}
		}
	}
	return len(pending) == 0
}

func sessionTreeDraft(msg provider.Message, fallback string) (string, []clipboardImageAttachment) {
	// Agent.Prompt stores the text first and appends image blocks after it,
	// even when an older transcript contains several text blocks. Rebuild
	// that same editor representation: join every text block, then place
	// image markers after the complete text so submitting the restored draft
	// recreates the original text/images pair without losing any text.
	var textBlocks []string
	var imageBlocks []provider.ImageBlock
	for _, content := range msg.Content {
		switch block := content.(type) {
		case provider.TextBlock:
			textBlocks = append(textBlocks, block.Text)
		case provider.ImageBlock:
			imageBlocks = append(imageBlocks, block)
		}
	}
	var text strings.Builder
	if len(textBlocks) > 0 {
		text.WriteString(strings.Join(textBlocks, "\n"))
	} else {
		text.WriteString(fallback)
	}
	images := make([]clipboardImageAttachment, 0, len(imageBlocks))
	for _, block := range imageBlocks {
		if text.Len() > 0 {
			last := text.String()
			if len(last) > 0 && last[len(last)-1] != ' ' && last[len(last)-1] != '\n' && last[len(last)-1] != '\t' {
				text.WriteByte(' ')
			}
		}
		marker := fmt.Sprintf("[clipboard image #%d]", len(images)+1)
		text.WriteString(marker)
		text.WriteByte(' ')
		images = append(images, clipboardImageAttachment{
			Marker: marker,
			Image: provider.ImageBlock{
				MimeType:         block.MimeType,
				Data:             append([]byte(nil), block.Data...),
				ThoughtSignature: block.ThoughtSignature,
			},
		})
	}
	return text.String(), images
}
