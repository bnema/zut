package modes

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"

	"github.com/bnema/zut/packages/agent/internal/sessionindex"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

// searchReader is a modal, read-only view of a transcript snapshot opened
// on a search hit. zut's main view is terminal scrollback that cannot be
// scrolled or restyled after printing, so matches are shown here instead.
// Fields are guarded by Interactive.mu; view is only touched by the build
// goroutine (one at a time, see building).
type searchReader struct {
	terms   []string
	view    *tui.View // private snapshot; never the live view
	target  searchReaderTarget
	matches []int // display message indexes (see archive), ascending
	cur     int   // position in matches
	ready   bool  // first build published

	// archivePath, when set, is loaded by the first build: the full
	// archived (pre-compaction) history of the session, shown dimmed above
	// the live rows. Display indexes 0..len(archive.Rows)-1 are archived
	// rows; live message idx is len(archive.Rows)+idx. Written only by the
	// first build before it publishes; dropped with the reader.
	archivePath string
	archiveCtx  context.Context
	cancel      context.CancelFunc // cancels the archive load; called on close
	archive     core.ArchivedHistory
	sepRows     map[int]bool // rows of inner compaction separators

	// Built rows, cached per width; rebuilt off the lock on resize.
	width    int
	rows     []string
	anchors  []tui.MessageAnchor
	building bool
	top      int  // first visible row
	recenter bool // place the current match near the top on next render
	page     int  // body rows of the last render, for PgUp/PgDn
}

// searchReaderTarget says where to open the reader. hit, when set, is
// resolved against the snapshot; otherwise focus is a message index. note
// is shown in the status once the reader resolved (opened or not).
type searchReaderTarget struct {
	terms []string
	focus int
	hit   *sessionindex.HitRef
	note  string
}

func (r *searchReader) Active() bool { return r != nil }

// finderViewActive reports whether the finder, its resume prompt, or the
// search reader is open.
func (i *Interactive) finderViewActive() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.finder.Active() || i.finderResume.Active() || i.searchReader.Active()
}

const searchReaderFooter = "^n/^p match · ↑↓ PgUp/PgDn scroll · esc close"

// searchReaderMessageText returns the searchable text of a user or
// assistant message, or "" for other roles.
func searchReaderMessageText(m provider.Message) string {
	if m.Role != provider.RoleUser && m.Role != provider.RoleAssistant {
		return ""
	}
	var b strings.Builder
	for _, c := range m.Content {
		if tb, ok := c.(provider.TextBlock); ok {
			b.WriteString(tb.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// searchReaderMatches reports whether every term is a prefix of some index
// token of text, mirroring the session index semantics.
func searchReaderMatches(text string, terms []string) bool {
	if text == "" || len(terms) == 0 {
		return false
	}
	found := make([]bool, len(terms))
	left := len(terms)
	sessionindex.Tokenize(text, func(tok string) {
		for k, t := range terms {
			if !found[k] && strings.HasPrefix(tok, t) {
				found[k] = true
				left--
			}
		}
	})
	return left == 0
}

// openSearchReader opens the reader on a snapshot of the current
// transcript. Matching and row building run off the input loop; the reader
// shows a loading row until they finish. With no match it closes and
// reports a status error.
func (i *Interactive) openSearchReader(target searchReaderTarget) {
	terms, _ := sessionindex.ParseQuery(strings.Join(target.terms, " "))
	target.terms = terms
	cols, rows := i.cfg.Terminal.Size()
	width := tui.FloatingPaneMaxRect(cols, rows).ContentWidth()
	i.mu.Lock()
	view := i.view.CloneForRender()
	view.TailLimit = 0
	view.Streaming, view.StreamingActive = "", false
	view.ToolCalls, view.Err = nil, ""
	// Image escapes are not text: measuring or clipping them corrupts the
	// terminal. The reader shows the text fallback.
	view.ImageProto = tui.ImageProtocolNone
	r := &searchReader{terms: terms, view: view, target: target, width: width, building: true}
	if hasCompactionSummary(view.Messages) {
		r.archivePath = i.sessionPathLocked()
		parent := i.runCtx
		if parent == nil {
			parent = context.Background()
		}
		r.archiveCtx, r.cancel = context.WithCancel(parent)
	}
	i.closeSearchReaderLocked()
	i.searchReader = r
	i.mu.Unlock()
	i.invalidate()
	go i.buildSearchReader(r, width, true)
}

// loadSearchReaderArchive reads the full archived history of r's session
// (first build only). A failure leaves the reader with live rows only.
func (i *Interactive) loadSearchReaderArchive(r *searchReader) {
	if r.archivePath == "" {
		return
	}
	if flush := i.cfg.FlushSession; flush != nil {
		flush()
	}
	hist, err := core.ReadArchivedSessionText(r.archiveCtx, r.archivePath, core.ArchivedOptions{Skip: skipArchivedMessage})
	if err != nil {
		return
	}
	r.archive = hist
}

// buildSearchReaderRows renders the archived rows (dimmed, with a
// separator at each inner compaction mark) above the live snapshot. It
// returns anchors indexed by display message index, the separator rows,
// and the display indexes that render no rows.
func (r *searchReader) buildSearchReaderRows(width int) (rows []string, anchors []tui.MessageAnchor, seps map[int]bool, empty map[int]bool) {
	arch := renderArchivedRows(r.view.Theme, r.archive, 0, width)
	rows, anchors, seps, empty = arch.rows, arch.anchors, arch.seps, arch.empty
	if empty == nil {
		empty = map[int]bool{}
	}
	base := len(r.archive.Rows)
	liveRows, liveAnchors := r.view.BuildWithAnchors(width)
	for k := range r.view.Messages {
		if anchorRows(liveAnchors, len(liveRows), k) == 0 {
			empty[base+k] = true
		}
	}
	for _, a := range liveAnchors {
		anchors = append(anchors, tui.MessageAnchor{MessageIdx: base + a.MessageIdx, Row: len(rows) + a.Row})
	}
	return append(rows, liveRows...), anchors, seps, empty
}

// resolveHitInArchive returns the display index of the archived row a
// search hit points to: same MsgSeq and, when the hit carries one, same
// text fingerprint.
func resolveHitInArchive(rows []core.ArchivedRow, fp uint64, msgSeq int) (int, bool) {
	for k, row := range rows {
		if row.MsgSeq == msgSeq && (fp == 0 || core.MessageTextFingerprint(row.Message) == fp) {
			return k, true
		}
	}
	return 0, false
}

// buildSearchReader computes matches (first build) and rows for width on
// the private snapshot (plus the archived history, loaded by the first
// build), then publishes them on the Run loop if r is still the open
// reader.
func (i *Interactive) buildSearchReader(r *searchReader, width int, first bool) {
	if first {
		i.loadSearchReaderArchive(r)
	}
	rows, anchors, seps, empty := r.buildSearchReaderRows(width)
	var matches []int
	cur := 0
	// r.target is written only before the first build starts.
	note := r.target.note
	if first {
		arch := r.archive.Rows
		for k, row := range arch {
			if !empty[k] && searchReaderMatches(searchReaderMessageText(row.Message), r.terms) {
				matches = append(matches, k)
			}
		}
		base := len(arch)
		msgs := r.view.Messages
		for idx, m := range msgs {
			// Messages that render no rows cannot be shown or tinted, and a
			// compaction summary renders as a separator without its text.
			if m.Meta["compaction"] == "true" || !searchReaderMatches(searchReaderMessageText(m), r.terms) || empty[base+idx] {
				continue
			}
			matches = append(matches, base+idx)
		}
		focus := base + r.target.focus
		if h := r.target.hit; h != nil {
			// The archive first: its match is exact (MsgSeq and text), so
			// repeated text from an archived era never lands on a live row.
			if k, ok := resolveHitInArchive(arch, h.Fingerprint, h.MsgSeq); ok {
				focus = k
			} else if idx, ok := resolveHitInView(msgs, h.Fingerprint, h.MsgSeq, h.TotalMsgs); ok {
				focus = base + idx
			} else {
				// The hit is gone: open on the next other match at or after
				// where it was, else the last one.
				focus = base + max(0, len(msgs)-(h.TotalMsgs-h.MsgSeq))
				note = joinStatus(note, "match is no longer in the session history")
			}
		}
		cur = len(matches) - 1
		for k, idx := range matches {
			if idx >= focus {
				cur = k
				break
			}
		}
	}
	apply := func() {
		i.mu.Lock()
		defer i.invalidate()
		defer i.mu.Unlock()
		if i.searchReader != r {
			return
		}
		r.rows, r.anchors, r.sepRows, r.width, r.building, r.recenter = rows, anchors, seps, width, false, true
		if !first {
			return
		}
		r.ready = true
		if len(matches) == 0 {
			i.closeSearchReaderLocked()
			msg := fmt.Sprintf("search %q: no match in this session", strings.Join(r.terms, " "))
			if note != "" {
				i.statusErr, i.statusOK = "", joinStatus(note, msg)
			} else {
				i.statusErr, i.statusOK = msg, ""
			}
			return
		}
		r.matches, r.cur = matches, cur
		if note != "" {
			i.statusErr, i.statusOK = "", note
		}
	}
	ctx := i.runCtx
	if ctx == nil {
		select {
		case i.loopCalls <- apply:
		default:
		}
		return
	}
	select {
	case i.loopCalls <- apply:
	case <-ctx.Done():
	}
}

// anchorRows returns how many rows message idx renders. Anchors hold one
// entry per message in order when the view has no tail limit.
func anchorRows(anchors []tui.MessageAnchor, total, idx int) int {
	if idx < 0 || idx >= len(anchors) || anchors[idx].MessageIdx != idx {
		return 0
	}
	end := total
	if idx+1 < len(anchors) {
		end = anchors[idx+1].Row
	}
	return end - anchors[idx].Row
}

// closeSearchReaderLocked closes the reader and cancels its archive load.
func (i *Interactive) closeSearchReaderLocked() {
	if r := i.searchReader; r != nil && r.cancel != nil {
		r.cancel()
	}
	i.searchReader = nil
}

// handleSearchReaderKey handles a key while the reader is open. The reader
// is modal: keys it does not use are ignored.
func (i *Interactive) handleSearchReaderKey(k tui.Key) {
	i.mu.Lock()
	r := i.searchReader
	if r == nil {
		i.mu.Unlock()
		return
	}
	page := max(1, r.page)
	switch k.Kind {
	case tui.KeyEsc, tui.KeyCtrlC:
		i.closeSearchReaderLocked()
	case tui.KeyCtrlN:
		r.step(1)
	case tui.KeyCtrlP:
		r.step(-1)
	case tui.KeyUp:
		r.top--
	case tui.KeyDown:
		r.top++
	case tui.KeyPageUp:
		r.top -= page
	case tui.KeyPageDown:
		r.top += page
	case tui.KeyHome:
		r.top = 0
	case tui.KeyEnd:
		r.top = len(r.rows)
	}
	r.clampTop(page)
	i.mu.Unlock()
	i.invalidate()
}

func (r *searchReader) clampTop(page int) {
	r.top = max(0, min(r.top, len(r.rows)-page))
}

func (r *searchReader) step(delta int) {
	n := len(r.matches)
	if n == 0 {
		return
	}
	r.cur = ((r.cur+delta)%n + n) % n
	r.recenter = true
}

func (r *searchReader) title() string {
	terms := sanitizeSessionTreeText(strings.Join(r.terms, " "))
	if len(r.matches) == 0 {
		return terms
	}
	return fmt.Sprintf("%s · %d/%d", terms, r.cur+1, len(r.matches))
}

// messageAt returns the message index owning row, or -1.
func (r *searchReader) messageAt(row int) int {
	if r.sepRows[row] {
		return -1
	}
	k := sort.Search(len(r.anchors), func(n int) bool { return r.anchors[n].Row > row }) - 1
	if k < 0 {
		return -1
	}
	return r.anchors[k].MessageIdx
}

func (r *searchReader) matchRow() int {
	if want := r.matches[r.cur]; want < len(r.anchors) && r.anchors[want].MessageIdx == want {
		return r.anchors[want].Row
	}
	return 0
}

// Render draws exactly height lines: header, spacer, body, footer. A width
// change starts a rebuild off the lock; the old rows show meanwhile, or a
// loading row before the first build.
func (i *Interactive) renderSearchReaderLocked(th tui.Theme, width, height int) []string {
	r := i.searchReader
	if r.ready && r.width != width && !r.building {
		r.building = true
		go i.buildSearchReader(r, width, false)
	}
	return r.Render(th, width, height)
}

// Render draws the reader from its current rows; see renderSearchReaderLocked.
func (r *searchReader) Render(th tui.Theme, width, height int) []string {
	if width < 4 || height < 1 {
		return nil
	}
	body := max(1, height-3)
	r.page = body
	title := r.title()
	if runewidth.StringWidth(title) > width-4 {
		title = runewidth.Truncate(title, max(1, width-5), "…")
	}
	lines := []string{frameHeader(th, title, width), strings.Repeat(" ", width)}
	if !r.ready || len(r.matches) == 0 {
		lines = append(lines, th.FGColor(th.Muted, pickerFit(" searching…", width)))
		for len(lines) < height-1 {
			lines = append(lines, strings.Repeat(" ", width))
		}
	} else {
		if r.recenter {
			r.top = r.matchRow() - body/4
			r.recenter = false
		}
		r.clampTop(body)
		matched := make(map[int]bool, len(r.matches))
		for _, idx := range r.matches {
			matched[idx] = true
		}
		current := r.matches[r.cur]
		for row := r.top; row < r.top+body; row++ {
			if row >= len(r.rows) {
				lines = append(lines, strings.Repeat(" ", width))
				continue
			}
			line := r.rows[row]
			if w := runewidth.StringWidth(stripANSIBytes(line)); w > width {
				line = clipStyledLine(line, width)
			} else if w < width {
				line += strings.Repeat(" ", width-w)
			}
			if msg := r.messageAt(row); matched[msg] {
				line, _ = tui.HighlightTerms(line, r.terms, th)
				if msg == current {
					line = tui.HighlightLine(line, th)
				}
			}
			lines = append(lines, line)
		}
	}
	lines = append(lines, th.FGColor(th.Muted, pickerFit(searchReaderFooter, width)))
	if len(lines) > height {
		lines = lines[len(lines)-height:]
	}
	return lines
}

// clipStyledLine cuts an ANSI-styled line to width display cells, keeping
// escape sequences and ending with a reset.
func clipStyledLine(line string, width int) string {
	var b strings.Builder
	w := 0
	for k := 0; k < len(line); {
		if line[k] == 0x1b && k+1 < len(line) && line[k+1] == '[' {
			j := k + 2
			for j < len(line) && (line[j] < 0x40 || line[j] > 0x7e) {
				j++
			}
			j = min(j+1, len(line))
			b.WriteString(line[k:j])
			k = j
			continue
		}
		r, size := utf8.DecodeRuneInString(line[k:])
		rw := runewidth.RuneWidth(r)
		if w+rw > width {
			break
		}
		b.WriteRune(r)
		w += rw
		k += size
	}
	b.WriteString("\x1b[0m")
	return b.String()
}
