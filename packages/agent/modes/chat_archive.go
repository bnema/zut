package modes

import (
	"context"
	"fmt"
	"slices"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

// chatArchiveState holds the archived (pre-compaction) history shown dimmed
// above the live transcript. It is display-only: archived rows never enter
// i.view.Messages or i.portMsgs, so message indices stay unchanged.
//
// The archive is loaded only when the session path changes. Rows added at
// the top of an already painted chat cannot be shown by the renderer, so a
// load result is applied with a full repaint, and only while idle.
type chatArchiveState struct {
	gen         uint64 // bumped per load request; stale results are dropped
	sessionPath string // path of the last hydrated session
	path        string // session path the current load or result belongs to
	cancel      context.CancelFunc
	hist        core.ArchivedHistory
	budget      int // archived rows shown; frozen when hist is applied
	rev         uint64
	themeRev    uint64
	// pending is a load result that arrived during a turn.
	pending *core.ArchivedHistory

	// rendered caches the dimmed rows for renderedKey.
	rendered    []string
	renderedKey chatArchiveRenderKey
	renderedOK  bool
}

type chatArchiveRenderKey struct {
	rev      uint64
	themeRev uint64
	cols     int
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

// skipArchivedMessage drops archived messages never shown: hidden rows and
// compaction summaries a fork copied in as normal message rows.
func skipArchivedMessage(m provider.Message) bool {
	return isHiddenTranscriptMessage(m) || m.Meta["compaction"] == "true"
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

// refreshChatArchiveLocked starts loading the archived history when the
// hydrated session path changed. A resnapshot of the same session (live
// compaction) keeps the archive as is: its rows are already in scrollback,
// and the next resume shows the new archive.
func (i *Interactive) refreshChatArchiveLocked() {
	a := &i.chatArchive
	path := i.sessionPathLocked()
	if path != "" && path == a.sessionPath {
		return
	}
	a.sessionPath = path
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	a.gen++
	a.pending = nil
	a.path = ""
	if len(a.hist.Rows) > 0 || len(a.hist.Marks) > 0 || a.hist.Older > 0 {
		// Another session: never show its predecessor's archive.
		a.hist, a.budget = core.ArchivedHistory{}, 0
		a.rev++
	}
	if path == "" || !hasCompactionSummary(i.portMsgs) {
		return
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
			Skip:  skipArchivedMessage,
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
// load or a session switch superseded it. During a turn the result waits
// for the idle transition (drainChatArchiveLocked).
func (i *Interactive) applyChatArchive(gen uint64, path string, hist core.ArchivedHistory) {
	i.mu.Lock()
	defer i.mu.Unlock()
	a := &i.chatArchive
	if a.gen != gen || a.path != path {
		return
	}
	a.cancel = nil
	if !i.chatArchiveIdleLocked() {
		a.pending = &hist
		return
	}
	a.pending = nil
	i.installChatArchiveLocked(hist)
}

// chatArchiveIdleLocked reports whether no turn is running or streaming.
func (i *Interactive) chatArchiveIdleLocked() bool {
	return !i.busy && !i.stream.Active()
}

// drainChatArchiveLocked applies a result held back during a turn. Called
// when the session goes idle and after each frame.
func (i *Interactive) drainChatArchiveLocked() {
	a := &i.chatArchive
	if a.pending == nil || !i.chatArchiveIdleLocked() {
		return
	}
	hist := *a.pending
	a.pending = nil
	if a.path == "" || a.path != i.sessionPathLocked() {
		return // belongs to a session no longer shown
	}
	i.installChatArchiveLocked(hist)
}

// installChatArchiveLocked shows hist with a full repaint, freezing the
// archive budget against the current live transcript. Nothing happens when
// the painted rows would not change.
func (i *Interactive) installChatArchiveLocked(hist core.ArchivedHistory) {
	a := &i.chatArchive
	budget := 0
	if len(hist.Rows) > 0 {
		budget = max(initialResumeTailLimit-len(i.view.Messages), 0)
	}
	if sameArchiveWindow(a.hist, a.budget, hist, budget) {
		return
	}
	a.hist, a.budget = hist, budget
	a.rev++
	a.renderedOK = false
	i.stableChatCacheValid = false
	// The rows land above an already painted chat: repaint it all. Request
	// it before releasing i.mu so no frame shows the new rows without it.
	i.requestRendererClear()
	i.invalidate()
}

// archiveWindow returns what renderChatArchive paints for hist and budget:
// the older count, the shown rows, and the inner marks.
func archiveWindow(hist core.ArchivedHistory, budget int) (older int, rows []core.ArchivedRow, marks []core.ArchivedMark) {
	if budget <= 0 || len(hist.Rows) == 0 {
		return 0, nil, nil
	}
	start := max(len(hist.Rows)-budget, 0)
	for _, m := range hist.Marks {
		if m.Index > start && m.Index < len(hist.Rows) {
			marks = append(marks, core.ArchivedMark{Index: m.Index - start, TokensBefore: m.TokensBefore})
		}
	}
	return hist.Older + start, hist.Rows[start:], marks
}

// sameArchiveWindow reports whether two archives paint the same rows.
func sameArchiveWindow(a core.ArchivedHistory, aBudget int, b core.ArchivedHistory, bBudget int) bool {
	ao, ar, am := archiveWindow(a, aBudget)
	bo, br, bm := archiveWindow(b, bBudget)
	return ao == bo && slices.Equal(am, bm) && slices.EqualFunc(ar, br, func(x, y core.ArchivedRow) bool {
		return x.MsgSeq == y.MsgSeq && x.Message.Role == y.Message.Role &&
			core.MessageTextFingerprint(x.Message) == core.MessageTextFingerprint(y.Message)
	})
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
	i.chatArchive.pending = nil
}

// chatArchiveRowsLocked returns the cached dimmed archive rows, or a
// render func to run (possibly without the lock) plus a store func to call
// under the lock afterwards.
func (i *Interactive) chatArchiveRowsLocked(cols int) (rows []string, render func() []string, store func([]string)) {
	a := &i.chatArchive
	if a.budget <= 0 || len(a.hist.Rows) == 0 {
		return nil, nil, nil
	}
	key := chatArchiveRenderKey{rev: a.rev, themeRev: a.themeRev, cols: cols}
	if a.renderedOK && a.renderedKey == key {
		return append([]string(nil), a.rendered...), nil, nil
	}
	hist, budget, th := a.hist, a.budget, i.cfg.Theme
	render = func() []string { return renderChatArchive(th, hist, budget, cols) }
	store = func(out []string) {
		if a.rev != key.rev || a.themeRev != key.themeRev {
			return
		}
		a.rendered = append(a.rendered[:0], out...)
		a.renderedKey = key
		a.renderedOK = true
	}
	return nil, render, store
}

// invalidateChatArchiveRenderLocked drops the cached archive rows (theme
// change).
func (i *Interactive) invalidateChatArchiveRenderLocked() {
	i.chatArchive.themeRev++
	i.chatArchive.renderedOK = false
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
	return append(out, renderArchivedRows(th, hist, start, cols).rows...)
}

// archivedRows is the dimmed rendering of archived rows.
type archivedRows struct {
	rows []string
	// anchors index hist.Rows (MessageIdx) and rows (Row).
	anchors []tui.MessageAnchor
	seps    map[int]bool // rows holding an inner compaction separator
	empty   map[int]bool // hist.Rows indexes that render no rows
}

// renderArchivedRows renders hist.Rows[start:] dimmed, split at the inner
// compaction marks with a dimmed separator at each. The latest mark (at the
// end) is left to the live compaction summary.
func renderArchivedRows(th tui.Theme, hist core.ArchivedHistory, start, width int) archivedRows {
	var out archivedRows
	segment := func(from, to int) {
		if from >= to {
			return
		}
		msgs := make([]provider.Message, 0, to-from)
		for _, r := range hist.Rows[from:to] {
			msgs = append(msgs, r.Message)
		}
		v := &tui.View{Theme: th, Messages: msgs, ImageProto: tui.ImageProtocolNone}
		segRows, segAnchors := v.BuildWithAnchors(width)
		for k := range to - from {
			if anchorRows(segAnchors, len(segRows), k) == 0 {
				if out.empty == nil {
					out.empty = map[int]bool{}
				}
				out.empty[from+k] = true
			}
		}
		for _, a := range segAnchors {
			out.anchors = append(out.anchors, tui.MessageAnchor{MessageIdx: from + a.MessageIdx, Row: len(out.rows) + a.Row})
		}
		out.rows = append(out.rows, tui.DimLines(segRows)...)
	}
	from := start
	for _, m := range hist.Marks {
		if m.Index <= start || m.Index >= len(hist.Rows) {
			continue
		}
		segment(from, m.Index)
		if out.seps == nil {
			out.seps = map[int]bool{}
		}
		out.seps[len(out.rows)] = true
		out.rows = append(out.rows, tui.Dim(tui.CompactionSeparator(th, width, tui.CompactionTokensLabel(m.TokensBefore))))
		from = m.Index
	}
	segment(from, len(hist.Rows))
	return out
}
