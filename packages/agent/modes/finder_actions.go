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
	m.confirmDelete = false
	m.renaming = true
	m.renameKey = item.Key
	m.rename = []rune(core.NormalizeSessionTitle(item.Title))
}

// handleRenameKey edits the rename field. Enter returns a Rename action for
// the item the rename started on; esc and ctrl+c cancel.
func (m *finderModel) handleRenameKey(k tui.Key) finderAction {
	switch k.Kind {
	case tui.KeyEsc, tui.KeyCtrlC:
		m.renaming, m.rename, m.renameKey = false, nil, ""
	case tui.KeyEnter:
		title, key := string(m.rename), m.renameKey
		m.renaming, m.rename, m.renameKey = false, nil, ""
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
		for _, r := range k.Paste {
			if unicode.IsPrint(r) {
				m.rename = append(m.rename, r)
			} else if unicode.IsSpace(r) {
				m.rename = append(m.rename, ' ')
			}
		}
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

// RemoveItem drops key from the results and keeps the cursor on the
// neighbouring row.
func (m *finderModel) RemoveItem(key string) {
	m.raw = finderWithout(m.raw, key)
	before := len(m.items)
	m.items = finderWithout(m.items, key)
	if len(m.items) != before {
		m.hit = 0
	}
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

// RenameItem sets the title and label of key in place.
func (m *finderModel) RenameItem(key, title string) {
	update := func(items []finderItem) {
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
	update(m.raw)
	update(m.items)
}

// finderSessionGuard protects sessions from finder actions:
// the active session cannot be deleted, and sessions running in the daemon
// (other than this one) are not modified from the finder.
func (i *Interactive) finderSessionGuard(item finderItem, deleting bool) (current bool, err string) {
	path := filepath.Clean(item.Key)
	if cur := i.currentSessionPath(); cur != "" && filepath.Clean(cur) == path {
		if deleting {
			return true, "cannot delete the active session; resume another session first"
		}
		return true, ""
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
	current, guard := i.finderSessionGuard(item, false)
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
// protections, removes its row, and drops it from the index.
func (i *Interactive) finderDelete(ctx context.Context, item finderItem) {
	// Without a way to know the active session, deleting is refused.
	if !i.portMode.Load() && i.cfg.CurrentSessionPath == nil {
		i.setFinderStatusErr("session deletion is not wired in this build")
		return
	}
	if _, guard := i.finderSessionGuard(item, true); guard != "" {
		i.setFinderStatusErr(guard)
		return
	}
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
		// ErrBusy: another writer holds the lock; retry once, and the next
		// Update drops files that no longer exist anyway.
		err := index.Forget(item.Key)
		if !errors.Is(err, sessionindex.ErrBusy) {
			return
		}
		timer := time.NewTimer(retry)
		defer timer.Stop()
		select {
		case <-timer.C:
			_ = index.Forget(item.Key)
		case <-ctx.Done():
		}
	}()
}
