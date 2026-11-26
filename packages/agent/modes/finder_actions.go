package modes

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/mattn/go-runewidth"

	"github.com/bnema/zut/packages/agent/internal/sessionindex"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/tui"
)

// startRename opens the inline rename field for the selected item,
// prefilled with its title.
func (m *finderModel) startRename() {
	item, ok := m.selected()
	if !ok {
		return
	}
	m.mode = finderModeRename
	m.renameKey = item.Key
	m.rename = []rune(core.NormalizeSessionTitle(item.Title))
}

// handleRenameKey edits the rename field. Enter returns a Rename action for
// the item the rename started on; esc and ctrl+c cancel.
func (m *finderModel) handleRenameKey(k tui.Key) finderAction {
	switch k.Kind {
	case tui.KeyEsc, tui.KeyCtrlC:
		m.mode, m.rename, m.renameKey = finderModeList, nil, ""
	case tui.KeyEnter:
		title, key := string(m.rename), m.renameKey
		m.mode, m.rename, m.renameKey = finderModeList, nil, ""
		for _, item := range m.items {
			if item.Key == key {
				return finderAction{Kind: finderActionRename, Item: item, Hit: -1, Title: title}
			}
		}
	case tui.KeyBackspace:
		if n := len(m.rename); n > 0 {
			m.rename = m.rename[:n-1]
		}
	case tui.KeyCtrlU:
		m.rename = nil
	case tui.KeyCtrlW:
		end := len(m.rename)
		for end > 0 && unicode.IsSpace(m.rename[end-1]) {
			end--
		}
		for end > 0 && !unicode.IsSpace(m.rename[end-1]) {
			end--
		}
		m.rename = m.rename[:end]
	case tui.KeyPaste:
		m.rename = append(m.rename, pickerPasteRunes(k.Paste)...)
	case tui.KeyRune:
		if !k.Ctrl && unicode.IsPrint(k.Rune) {
			m.rename = append(m.rename, k.Rune)
		}
	}
	return finderAction{}
}

const finderRenameHelp = " · enter save · esc cancel"

// renderRename draws the rename field in w cells, keeping the end of the
// text (where the caret is) visible. It returns the caret column.
func (m *finderModel) renderRename(th tui.Theme, w int) (string, int) {
	prompt := " rename: "
	help := finderRenameHelp
	text := string(m.rename)
	room := w - runewidth.StringWidth(prompt)
	if runewidth.StringWidth(text)+1+runewidth.StringWidth(help) > room {
		help = ""
	}
	field := max(room-runewidth.StringWidth(help)-1, 0)
	runes := m.rename
	for len(runes) > 0 && runewidth.StringWidth(string(runes)) > field {
		runes = runes[1:]
	}
	text = string(runes)
	caret := runewidth.StringWidth(prompt) + runewidth.StringWidth(text)
	pad := max(room-runewidth.StringWidth(text)-runewidth.StringWidth(help), 0)
	line := th.FGColor(th.Accent, prompt) + text + th.FGColor(th.Muted, help) + strings.Repeat(" ", pad)
	return line, caret
}

// RemoveItem drops key from the results, keeps the cursor on the
// neighbouring row, and hides key from later results until ClearOverlay.
func (m *finderModel) RemoveItem(key string) {
	if m.deleted == nil {
		m.deleted = map[string]bool{}
	}
	m.deleted[key] = true
	m.overlaySeq++
	selected := ""
	if item, ok := m.selected(); ok {
		selected = item.Key
	}
	m.raw = finderWithout(m.raw, key)
	m.items = finderWithout(m.items, key)
	for idx, item := range m.items {
		if item.Key == selected {
			m.cursor = idx // another row went away: stay on the same item
			return
		}
	}
	m.hit = 0
	m.cursor = min(m.cursor, max(len(m.items)-1, 0))
}

func finderWithout(items []finderItem, key string) []finderItem {
	out := items[:0:0]
	for _, item := range items {
		if item.Key != key {
			out = append(out, item)
		}
	}
	return out
}

// RenameItem sets the title and label of key in place and keeps them on
// later results until ClearOverlay.
func (m *finderModel) RenameItem(key, title string) {
	if m.renamed == nil {
		m.renamed = map[string]string{}
	}
	m.renamed[key] = title
	m.overlaySeq++
	finderRetitle(m.raw, key, title)
	finderRetitle(m.items, key, title)
}

func finderRetitle(items []finderItem, key, title string) {
	for idx := range items {
		if items[idx].Key != key {
			continue
		}
		project, _, ok := strings.Cut(items[idx].Label, " · ")
		if !ok {
			project = "-"
		}
		items[idx].Title = title
		items[idx].Label = project + " · " + title
	}
}

// applyOverlay drops deleted keys and re-applies renames to fresh results,
// which may come from an index generation older than the action.
func (m *finderModel) applyOverlay(items []finderItem) []finderItem {
	if len(m.deleted) == 0 && len(m.renamed) == 0 {
		return items
	}
	out := make([]finderItem, 0, len(items))
	for _, item := range items {
		if m.deleted[item.Key] {
			continue
		}
		out = append(out, item)
	}
	for key, title := range m.renamed {
		finderRetitle(out, key, title)
	}
	return out
}

// ClearOverlay drops overlays when no action happened after seq, the
// overlay sequence observed when the finishing index update started. It
// reports whether they were cleared.
func (m *finderModel) ClearOverlay(seq uint64) bool {
	if m.overlaySeq != seq {
		return false
	}
	m.deleted, m.renamed = nil, nil
	return true
}

// finderSessionGuard protects sessions from finder actions. It checks,
// in order:
//   - the key is a session file managed under the sessions root (the index
//     is not trusted as a path source);
//   - the active session cannot be deleted (renaming it is allowed);
//   - in port mode, the daemon live list must have loaded, and sessions
//     running there (other than this one) are not modified.
func (i *Interactive) finderSessionGuard(ctx context.Context, item finderItem, deleting bool) (current bool, err string) {
	if _, merr := core.ManagedSessionMeta(ctx, i.sessionsRoot(), item.Key); merr != nil {
		return false, "finder: refusing to modify " + friendlyPath(item.Key) + ": " + merr.Error()
	}
	path := filepath.Clean(item.Key)
	if cur := i.currentSessionPath(); cur != "" && filepath.Clean(cur) == path {
		if deleting {
			return true, "cannot delete the active session; resume another session first"
		}
		return true, ""
	}
	if i.portMode.Load() {
		i.mu.Lock()
		loaded := i.finder != nil && i.finder.liveLoaded
		i.mu.Unlock()
		if !loaded {
			return false, "finder: running sessions are not known yet; try again"
		}
	}
	if item.Live {
		if deleting {
			return false, "cannot delete a running session; close it first"
		}
		return false, "cannot rename a running session from here; attach to it first"
	}
	return false, ""
}

func (i *Interactive) setFinderStatusOK(message string) {
	i.mu.Lock()
	i.statusOK, i.statusErr = message, ""
	i.mu.Unlock()
	i.invalidate()
}

// finderRename renames item's session file with core.RenameSession,
// updates its row, and refreshes the index in the background.
func (i *Interactive) finderRename(ctx context.Context, item finderItem, title string) {
	title = core.NormalizeSessionTitle(title)
	if title == "" {
		i.setFinderStatusErr("rename: session title is empty")
		return
	}
	current, guard := i.finderSessionGuard(ctx, item, false)
	if guard != "" {
		i.setFinderStatusErr(guard)
		return
	}
	if current {
		i.markSessionTitleSwitching()
	}
	if err := core.RenameSession(item.Key, title); err != nil {
		if current {
			i.restoreFailedSessionTitle()
		}
		i.setFinderStatusErr("rename: " + err.Error())
		return
	}
	if current {
		i.setManualSessionTitle(title)
	}
	i.mu.Lock()
	if i.finder != nil {
		i.finder.RenameItem(item.Key, title)
	}
	i.mu.Unlock()
	i.setFinderStatusOK("renamed session: " + title)
	i.startFinderIndexUpdate(ctx)
}

// finderDelete deletes item's session file with finderSessionGuard's
// protections. In port mode the daemon list is fetched again first so a
// session that started running since the finder opened is not deleted.
func (i *Interactive) finderDelete(ctx context.Context, item finderItem) {
	// Without a way to know the active session, deleting is refused.
	if !i.portMode.Load() && i.cfg.CurrentSessionPath == nil {
		i.setFinderStatusErr("session deletion is not wired in this build")
		return
	}
	if _, guard := i.finderSessionGuard(ctx, item, true); guard != "" {
		i.setFinderStatusErr(guard)
		return
	}
	if !i.portMode.Load() {
		i.finderDeleteNow(ctx, item)
		return
	}
	client := i.SessionClient()
	if client == nil {
		i.setFinderStatusErr("finder: running sessions are not known; delete refused")
		return
	}
	i.mu.Lock()
	model := i.finder
	i.mu.Unlock()
	timeout := i.sessionCallTimeout()
	go func() {
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		infos, err := client.List(callCtx)
		cancel()
		apply := func() {
			if err != nil {
				i.setFinderStatusErr("finder: list running sessions: " + err.Error() + "; delete refused")
				return
			}
			live := finderLiveItems(infos)
			i.mu.Lock()
			if i.finder == model && model.Active() {
				model.SetLive(live)
			}
			i.mu.Unlock()
			key := finderLiveKey(item.Key)
			for _, l := range live {
				if l.Key == key {
					item.Live, item.LiveID, item.LiveCWD = true, l.LiveID, l.LiveCWD
				}
			}
			if _, guard := i.finderSessionGuard(ctx, item, true); guard != "" {
				i.setFinderStatusErr(guard)
				return
			}
			i.finderDeleteNow(ctx, item)
		}
		select {
		case i.loopCalls <- apply:
		case <-ctx.Done():
		}
	}()
}

// finderDeleteNow deletes a guarded session, removes its row, and drops it
// from the index, then refreshes the index.
func (i *Interactive) finderDeleteNow(ctx context.Context, item finderItem) {
	if err := core.DeleteSession(item.Key); err != nil {
		i.setFinderStatusErr("delete session: " + err.Error())
		return
	}
	i.mu.Lock()
	if i.finder != nil {
		i.finder.RemoveItem(item.Key)
	}
	index, retry := i.finderIndex, i.finderRetryDelay
	i.mu.Unlock()
	i.setFinderStatusOK("deleted session: " + friendlyPath(item.Key))
	if index == nil {
		return
	}
	go func() {
		// ErrBusy: another writer holds the lock; retry once. The update
		// queued afterwards also drops files that no longer exist.
		if err := index.Forget(item.Key); errors.Is(err, sessionindex.ErrBusy) {
			timer := time.NewTimer(retry)
			select {
			case <-timer.C:
				_ = index.Forget(item.Key)
			case <-ctx.Done():
				timer.Stop()
				return
			}
		}
		select {
		case i.loopCalls <- func() { i.startFinderIndexUpdate(ctx) }:
		case <-ctx.Done():
		}
	}()
}
