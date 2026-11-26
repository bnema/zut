package modes

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"

	"github.com/bnema/zut/packages/agent/internal/sessionindex"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

// searchReader is a modal, read-only view of a transcript snapshot opened
// on a search hit. zut's main view is terminal scrollback that cannot be
// scrolled or restyled after printing, so matches are shown here instead.
// Guarded by Interactive.mu.
type searchReader struct {
	terms   []string
	view    *tui.View // snapshot; never the live view
	matches []int     // message indexes in view.Messages, ascending
	cur     int       // position in matches

	// Built rows, cached per width; rebuilt only on resize.
	width    int
	rows     []string
	anchors  []tui.MessageAnchor
	top      int  // first visible row
	recenter bool // place the current match near the top on next render
	page     int  // body rows of the last render, for PgUp/PgDn
}

func (r *searchReader) Active() bool { return r != nil }

func (i *Interactive) searchReaderOpen() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.searchReader != nil
}

const searchReaderFooter = "^n/^p match · ↑↓ PgUp/PgDn scroll · esc close"

// chatSearchMessageText returns the searchable text of a user or assistant
// message, or "" for other roles.
func chatSearchMessageText(m provider.Message) string {
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

// chatSearchMatches reports whether every term is a prefix of some index
// token of text, mirroring the session index semantics.
func chatSearchMatches(text string, terms []string) bool {
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

// openSearchReader opens the reader on a snapshot of the current transcript
// at the match at (or nearest after) focusMsgIdx. With no match it reports
// a status error and stays closed.
func (i *Interactive) openSearchReader(terms []string, focusMsgIdx int) {
	terms, _ = sessionindex.ParseQuery(strings.Join(terms, " "))
	i.mu.Lock()
	defer i.invalidate()
	defer i.mu.Unlock()
	view := i.view.CloneForRender()
	view.TailLimit = 0
	view.Streaming, view.StreamingActive = "", false
	view.ToolCalls, view.Err = nil, ""
	var matches []int
	if len(terms) > 0 {
		for idx, m := range view.Messages {
			if chatSearchMatches(chatSearchMessageText(m), terms) {
				matches = append(matches, idx)
			}
		}
	}
	if len(matches) == 0 {
		i.searchReader = nil
		i.statusErr, i.statusOK = fmt.Sprintf("search %q: no match in this session", strings.Join(terms, " ")), ""
		return
	}
	cur := len(matches) - 1
	for k, idx := range matches {
		if idx >= focusMsgIdx {
			cur = k
			break
		}
	}
	i.searchReader = &searchReader{terms: terms, view: view, matches: matches, cur: cur, recenter: true}
}

// closeSearchReaderLocked closes the reader; its snapshot belongs to the
// transcript being replaced.
func (i *Interactive) closeSearchReaderLocked() { i.searchReader = nil }

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
		i.searchReader = nil
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
	i.mu.Unlock()
	i.invalidate()
}

func (r *searchReader) step(delta int) {
	n := len(r.matches)
	r.cur = ((r.cur+delta)%n + n) % n
	r.recenter = true
}

func (r *searchReader) title() string {
	return fmt.Sprintf("%s · %d/%d", sanitizeSessionTreeText(strings.Join(r.terms, " ")), r.cur+1, len(r.matches))
}

// messageAt returns the message index owning row, or -1.
func (r *searchReader) messageAt(row int) int {
	k := sort.Search(len(r.anchors), func(n int) bool { return r.anchors[n].Row > row }) - 1
	if k < 0 {
		return -1
	}
	return r.anchors[k].MessageIdx
}

func (r *searchReader) matchRow() int {
	want := r.matches[r.cur]
	for _, a := range r.anchors {
		if a.MessageIdx == want {
			return a.Row
		}
	}
	return 0
}

// Render draws exactly height lines: header, spacer, body, footer.
func (r *searchReader) Render(th tui.Theme, width, height int) []string {
	if width < 4 || height < 1 {
		return nil
	}
	if r.width != width {
		r.rows, r.anchors = r.view.BuildWithAnchors(width)
		r.width = width
		r.recenter = true
	}
	body := max(1, height-3)
	r.page = body
	if r.recenter {
		r.top = r.matchRow() - body/4
		r.recenter = false
	}
	r.top = min(r.top, len(r.rows)-body)
	r.top = max(r.top, 0)

	matched := make(map[int]bool, len(r.matches))
	for _, idx := range r.matches {
		matched[idx] = true
	}
	current := r.matches[r.cur]
	title := r.title()
	if runewidth.StringWidth(title) > width-4 {
		title = runewidth.Truncate(title, max(1, width-5), "…")
	}
	lines := []string{frameHeader(th, title, width), strings.Repeat(" ", width)}
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
	lines = append(lines, th.FGColor(th.Muted, finderFit(searchReaderFooter, width)))
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
