package modes

import (
	"context"
	"fmt"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

// chatArchiveState holds the archived (pre-compaction) history shown dimmed
// above the live transcript. It is display-only: archived rows never enter
// i.view.Messages or i.portMsgs, so message indices stay unchanged.
type chatArchiveState struct {
	gen    uint64 // bumped per load request; stale results are dropped
	path   string // session path the current load or result belongs to
	cancel context.CancelFunc
	hist   core.ArchivedHistory
	rev    uint64 // bumped whenever hist changes; part of the chat cache key

	// rendered caches the dimmed rows for (rev, budget, cols).
	rendered    []string
	renderedKey chatArchiveRenderKey
	renderedOK  bool
}

type chatArchiveRenderKey struct {
	rev    uint64
	budget int
	cols   int
}

// hasCompactionSummary reports whether msgs carry a compaction summary.
func hasCompactionSummary(msgs []provider.Message) bool {
	for _, m := range msgs {
		if m.Meta["compaction"] == "true" {
			return true
		}
	}
	return false
}

// sessionPathLocked is currentSessionPath for callers holding i.mu.
func (i *Interactive) sessionPathLocked() string {
	if i.portMode.Load() {
		return i.portInfo.SessionPath
	}
	if i.cfg.CurrentSessionPath == nil {
		return ""
	}
	return i.cfg.CurrentSessionPath()
}

// refreshChatArchiveLocked loads the archived history after the live
// messages were replaced, or clears it when they hold no compaction.
func (i *Interactive) refreshChatArchiveLocked() {
	a := &i.chatArchive
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	a.gen++
	path := ""
	if hasCompactionSummary(i.portMsgs) {
		path = i.sessionPathLocked()
	}
	if path == "" {
		a.path = ""
		if len(a.hist.Rows) > 0 || len(a.hist.Marks) > 0 || a.hist.Older > 0 {
			a.hist = core.ArchivedHistory{}
			a.rev++
		}
		return
	}
	if path != a.path && (len(a.hist.Rows) > 0 || a.hist.Older > 0) {
		// Another session: never show its predecessor's archive meanwhile.
		a.hist = core.ArchivedHistory{}
		a.rev++
	}
	a.path = path
	parent := i.runCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	a.cancel = cancel
	gen, flush := a.gen, i.cfg.FlushSession
	go func() {
		defer cancel()
		if flush != nil {
			flush()
		}
		hist, err := core.ReadArchivedSessionText(ctx, path, core.ArchivedOptions{
			Skip:  isHiddenTranscriptMessage,
			Limit: initialResumeTailLimit,
		})
		if ctx.Err() != nil || err != nil {
			return // superseded, closed, or unreadable: keep the live view as is
		}
		apply := func() { i.applyChatArchive(gen, path, hist) }
		select {
		case i.loopCalls <- apply:
		case <-ctx.Done():
		}
	}()
}

// applyChatArchive installs a load result on the Run loop unless a newer
// load or a session switch superseded it.
func (i *Interactive) applyChatArchive(gen uint64, path string, hist core.ArchivedHistory) {
	i.mu.Lock()
	a := &i.chatArchive
	if a.gen != gen || a.path != path {
		i.mu.Unlock()
		return
	}
	a.cancel = nil
	a.hist = hist
	a.rev++
	i.stableChatCacheValid = false
	i.mu.Unlock()
	i.invalidate()
}

// cancelChatArchive stops an in-flight load (Run shutdown).
func (i *Interactive) cancelChatArchive() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.chatArchive.cancel != nil {
		i.chatArchive.cancel()
		i.chatArchive.cancel = nil
	}
	i.chatArchive.gen++
}

// chatArchiveBudgetLocked is how many archived rows fit before the live
// transcript reaches the resume tail limit.
func (i *Interactive) chatArchiveBudgetLocked() int {
	if len(i.chatArchive.hist.Rows) == 0 {
		return 0
	}
	return max(initialResumeTailLimit-len(i.view.Messages), 0)
}

// chatArchiveRowsLocked returns the cached dimmed archive rows, or a
// render func to run (possibly without the lock) plus a store func to call
// under the lock afterwards.
func (i *Interactive) chatArchiveRowsLocked(cols int) (rows []string, render func() []string, store func([]string)) {
	a := &i.chatArchive
	key := chatArchiveRenderKey{rev: a.rev, budget: i.chatArchiveBudgetLocked(), cols: cols}
	if key.budget == 0 {
		return nil, nil, nil
	}
	if a.renderedOK && a.renderedKey == key {
		return append([]string(nil), a.rendered...), nil, nil
	}
	hist, th := a.hist, i.cfg.Theme
	render = func() []string { return renderChatArchive(th, hist, key.budget, cols) }
	store = func(out []string) {
		if a.rev != key.rev {
			return
		}
		a.rendered = append(a.rendered[:0], out...)
		a.renderedKey = key
		a.renderedOK = true
	}
	return nil, render, store
}

// renderChatArchive renders the last budget archived rows dimmed, with a
// separator at each inner compaction boundary and an "older messages" line
// when earlier rows exist. The latest boundary (at the end) is left to the
// live compaction summary.
func renderChatArchive(th tui.Theme, hist core.ArchivedHistory, budget, cols int) []string {
	if budget <= 0 || len(hist.Rows) == 0 || cols <= 0 {
		return nil
	}
	start := max(len(hist.Rows)-budget, 0)
	var out []string
	if older := hist.Older + start; older > 0 {
		noun := "messages"
		if older == 1 {
			noun = "message"
		}
		out = append(out, tui.Dim(truncateLine(fmt.Sprintf("  %d older %s · ctrl+f to search", older, noun), cols)))
	}
	segment := func(from, to int) {
		if from >= to {
			return
		}
		msgs := make([]provider.Message, 0, to-from)
		for _, r := range hist.Rows[from:to] {
			msgs = append(msgs, r.Message)
		}
		v := &tui.View{Theme: th, Messages: msgs}
		out = append(out, tui.DimLines(v.Build(cols))...)
	}
	from := start
	for _, m := range hist.Marks {
		if m.Index <= start || m.Index >= len(hist.Rows) {
			continue
		}
		segment(from, m.Index)
		out = append(out, tui.Dim(tui.CompactionSeparator(th, cols, tui.CompactionTokensLabel(m.TokensBefore))))
		from = m.Index
	}
	segment(from, len(hist.Rows))
	return out
}
