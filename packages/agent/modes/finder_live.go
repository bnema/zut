package modes

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/bnema/zut/packages/agent/session"
)

// finderLiveLister marks sources whose empty-query list should start with
// live sessions that have no index entry yet.
type finderLiveLister interface {
	ListsLive() bool
}

func (*finderSessionSource) ListsLive() bool { return true }

// finderLiveKey normalizes a session path for live matching.
func finderLiveKey(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	return filepath.Clean(path)
}

// finderLiveItems converts daemon session infos to finder rows keyed by
// session path.
func finderLiveItems(infos []session.Info) []finderItem {
	items := make([]finderItem, 0, len(infos))
	for _, info := range infos {
		key := finderLiveKey(info.SessionPath)
		if key == "" {
			continue // not persisted: nothing to match or resume
		}
		raw := sanitizeSessionTreeText(strings.TrimSpace(info.Title))
		title := raw
		if title == "" {
			title = "(new session)"
		}
		when := info.LastUserInputAt
		if when.IsZero() {
			when = info.UpdatedAt
		}
		items = append(items, finderItem{
			Key:     key,
			Label:   sessionProject(info.RepoRoot, info.CWD) + " · " + title,
			Detail:  formatRelative(when),
			Live:    true,
			LiveID:  info.ID,
			Title:   raw,
			LiveCWD: info.CWD,
		})
	}
	return items
}

// SetLive replaces the live session list and re-decorates the shown items,
// keeping the cursor on the same key and any snippets already loaded.
func (m *finderModel) SetLive(items []finderItem) {
	m.live = items
	m.liveLoaded = true
	if !m.Active() {
		return
	}
	prevKey := ""
	if item, ok := m.selected(); ok {
		prevKey = item.Key
	}
	hits := make(map[string][]finderHit, len(m.items))
	for _, item := range m.items {
		hits[item.Key] = item.Hits
	}
	m.items = m.decorateLive(m.raw)
	m.cursor = 0
	found := false
	for idx := range m.items {
		if h, ok := hits[m.items[idx].Key]; ok {
			m.items[idx].Hits = h
		}
		if !found && m.items[idx].Key == prevKey {
			m.cursor, found = idx, true
		}
	}
	if !found {
		m.hit = 0
	}
}

// decorateLive marks raw items that are live and, for an empty query on a
// live-listing source, prepends live sessions missing from raw.
func (m *finderModel) decorateLive(raw []finderItem) []finderItem {
	if len(m.live) == 0 {
		return raw
	}
	byKey := make(map[string]finderItem, len(m.live))
	for _, item := range m.live {
		byKey[item.Key] = item
	}
	out := make([]finderItem, 0, len(raw)+len(m.live))
	seen := make(map[string]bool, len(raw))
	for _, item := range raw {
		key := finderLiveKey(item.Key)
		seen[key] = true
		if live, ok := byKey[key]; ok {
			item.Live, item.LiveID, item.LiveCWD = true, live.LiveID, live.LiveCWD
		}
		out = append(out, item)
	}
	lister, ok := m.source().(finderLiveLister)
	if !ok || !lister.ListsLive() || finderFuzzyText(string(m.query)) != "" {
		return out
	}
	var top []finderItem
	for _, item := range m.live {
		if !seen[item.Key] {
			top = append(top, item)
		}
	}
	return append(top, out...)
}

// loadFinderLive fetches daemon sessions once for the open finder (port
// mode only) and applies them on the Run loop. An error is reported in the
// status line and leaves the finder without live marks.
func (i *Interactive) loadFinderLive(ctx context.Context) {
	if !i.portMode.Load() {
		return
	}
	client := i.SessionClient()
	if client == nil {
		return
	}
	i.mu.Lock()
	m := i.finder
	i.mu.Unlock()
	timeout := i.sessionCallTimeout()
	go func() {
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		infos, err := client.List(callCtx)
		cancel()
		if err != nil {
			// Without the list, rename and delete stay refused in port mode.
			msg := "finder: list running sessions: " + err.Error()
			select {
			case i.loopCalls <- func() { i.setFinderStatusErr(msg) }:
			case <-ctx.Done():
			}
			return
		}
		items := finderLiveItems(infos)
		apply := func() {
			i.mu.Lock()
			defer i.mu.Unlock()
			if i.finder != m || !m.Active() {
				return
			}
			m.SetLive(items)
			i.invalidate()
		}
		select {
		case i.loopCalls <- apply:
		case <-ctx.Done():
		}
	}()
}
