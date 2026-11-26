package modes

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/bnema/zut/packages/agent/internal/sessionindex"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

const (
	finderSplitMinWidth   = 100 // side-by-side list and preview
	finderPreviewMinWidth = 50  // below this: list plus one preview line
	finderFooterHelp      = "tab source · ↑↓ pick · ^n/^p hit · enter open · ^r rename · ^d delete · @here this dir · esc close"
)

// finderFit truncates s (plain text) to w display cells and pads it to
// exactly w cells.
func finderFit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) > w {
		s = runewidth.Truncate(s, w, "…")
	}
	return s + strings.Repeat(" ", w-runewidth.StringWidth(s))
}

// finderLayout is the row budget of one Render call.
type finderLayout struct {
	width       int
	split       bool // list and preview side by side
	leftW       int
	rightW      int
	listRows    int
	previewRows int // stacked preview rows; 0 in split mode
	oneLine     bool
	footer      bool
}

// Render rows that precede the result list: the frame header (moved into
// the pane title), a spacer, the tabs/status line, and the query line.
const (
	finderHeaderRow = 0
	finderTabsRow   = 2
	finderQueryRow  = 3
)

// layout splits the body height. When rows are short it degrades in this
// order: stacked preview becomes one preview line, then the preview, then
// the footer is dropped.
func (m *finderModel) layout(width, height int) finderLayout {
	l := finderLayout{width: width, footer: true}
	fixed := 4 // spacer, tabs, query, rule under the query
	if m.err != "" {
		fixed++
	}
	body := height - fixed - 2 // rule + footer
	if body < 2 {
		l.footer = false
		body = height - fixed
	}
	body = max(body, 1)
	switch {
	case width >= finderSplitMinWidth:
		l.split = true
		l.leftW = (width - 1) * 45 / 100
		l.rightW = width - 1 - l.leftW
		l.listRows = body
	case width >= finderPreviewMinWidth && body >= 5:
		// One rule row between list and preview.
		l.listRows = max((body-1)*60/100, 1)
		l.previewRows = max(body-1-l.listRows, 1)
	case body >= 3:
		// List, rule, and one preview line of the current hit.
		l.listRows = body - 2
		l.oneLine = true
	default:
		l.listRows = body
	}
	return l
}

// Render draws the finder as a dialog: a "── Find" frame header that the
// floating pane turns into its title, then height body rows. Every line is
// exactly width display cells wide. Render updates the scroll offsets.
func (m *finderModel) Render(th tui.Theme, width, height int) []string {
	if width < 4 || height < 1 {
		return nil
	}
	l := m.layout(width, height)
	m.viewTop = clampViewTop(m.viewTop, m.cursor, l.listRows, len(m.items))
	muted := func(s string) string { return th.FGColor(th.Muted, s) }
	rule := muted(strings.Repeat("─", width))

	lines := []string{frameHeader(th, "Find", width), strings.Repeat(" ", width), m.renderTop(th, width), m.renderQuery(th, width)}
	if m.err != "" {
		lines = append(lines, th.FGColor(th.Warning, finderFit(" "+sanitizeSessionTreeText(m.err), width)))
	}
	terms, _ := sessionindex.ParseQuery(m.Query())
	if l.split {
		lines = append(lines, muted(strings.Repeat("─", l.leftW)+"┬"+strings.Repeat("─", l.rightW)))
		list := m.renderList(th, l.leftW, l.listRows)
		preview := m.renderPreview(th, terms, l.rightW, l.listRows)
		for i := range list {
			lines = append(lines, list[i]+muted("│")+preview[i])
		}
		if l.footer {
			lines = append(lines, muted(strings.Repeat("─", l.leftW)+"┴"+strings.Repeat("─", l.rightW)))
		}
	} else {
		lines = append(lines, rule)
		lines = append(lines, m.renderList(th, width, l.listRows)...)
		switch {
		case l.oneLine:
			lines = append(lines, rule, m.renderOneLinePreview(th, terms, width))
		case l.previewRows > 0:
			lines = append(lines, rule)
			lines = append(lines, m.renderPreview(th, terms, width, l.previewRows)...)
		}
		if l.footer {
			lines = append(lines, rule)
		}
	}
	if l.footer {
		lines = append(lines, m.renderFooter(th, width))
	}
	// Very short panes: never exceed the budget.
	if len(lines) > height+1 {
		lines = lines[:height+1]
	}
	return lines
}

// CursorPos returns the 0-based row/col of the query caret in the lines
// returned by Render, or -1, -1 when the finder is closed. It uses the
// scroll offsets computed by the last Render.
func (m *finderModel) CursorPos() (row, col int) {
	if !m.Active() {
		return -1, -1
	}
	if m.mode == finderModeRename {
		return finderQueryRow, m.renameCaret
	}
	start := min(m.qscroll, m.qcursor)
	return finderQueryRow, 3 + runewidth.StringWidth(string(m.query[start:m.qcursor]))
}

// renderTop draws the tabs (active one bracketed in the accent color) and
// the right-aligned status in exactly w cells; the status is dropped first
// when space runs out.
func (m *finderModel) renderTop(th tui.Theme, w int) string {
	var names []string
	tabsW := 1 // leading space
	for i, src := range m.tabs {
		name := sanitizeSessionTreeText(src.Title())
		if i == m.tab {
			name = "[" + name + "]"
		}
		names = append(names, name)
		tabsW += runewidth.StringWidth(name)
	}
	if len(names) > 1 {
		tabsW += len(names) - 1
	}
	if tabsW > w {
		return th.FGColor(th.Muted, finderFit(" "+strings.Join(names, " "), w))
	}
	var b strings.Builder
	b.WriteString(" ")
	for i, name := range names {
		if i > 0 {
			b.WriteString(" ")
		}
		if i == m.tab {
			b.WriteString(th.FGColor(th.Accent, name))
		} else {
			b.WriteString(th.FGColor(th.Muted, name))
		}
	}
	status, color := m.status(th)
	status += " "
	sw := runewidth.StringWidth(status)
	if tabsW+1+sw > w {
		b.WriteString(strings.Repeat(" ", w-tabsW))
		return b.String()
	}
	b.WriteString(strings.Repeat(" ", w-tabsW-sw))
	b.WriteString(th.FGColor(color, status))
	return b.String()
}

// finderFitBorder renders left, a ─ fill, and the corner in exactly w cells.
func finderFitBorder(left, corner string, w int) string {
	if runewidth.StringWidth(left)+1 > w {
		return finderFit(strings.Repeat("─", w-1), w-1) + corner
	}
	return left + strings.Repeat("─", w-runewidth.StringWidth(left)-1) + corner
}

func (m *finderModel) status(th tui.Theme) (string, tui.TerminalColor) {
	if m.indexing {
		s := newSpinner(th)
		s.startedAt = m.loadingSince
		if m.indexTotal > 0 {
			return fmt.Sprintf("%s indexing %d/%d", s.Frame(), m.indexDone, m.indexTotal), th.Spinner
		}
		return s.Frame() + " indexing", th.Spinner
	}
	if m.loading {
		s := newSpinner(th)
		s.startedAt = m.loadingSince
		return s.Frame() + " searching", th.Spinner
	}
	hits := 0
	for _, item := range m.items {
		hits += item.Count
	}
	text := fmt.Sprintf("%d hits · %d sessions", hits, len(m.items))
	if m.partial {
		text += " · partial"
	}
	return text, th.Muted
}

func (m *finderModel) renderQuery(th tui.Theme, w int) string {
	if m.mode == finderModeRename {
		line, caret := m.renderRename(th, w)
		m.renameCaret = caret
		return line
	}
	prompt := " > "
	room := max(w-runewidth.StringWidth(prompt), 0)
	// Scroll horizontally so the caret (one cell after the text before it)
	// stays inside the field.
	field := max(room-1, 0)
	m.qscroll = min(m.qscroll, m.qcursor)
	for m.qscroll < m.qcursor && runewidth.StringWidth(string(m.query[m.qscroll:m.qcursor])) > field {
		m.qscroll++
	}
	if runewidth.StringWidth(string(m.query)) <= field {
		m.qscroll = 0
	}
	return th.FGColor(th.Accent, prompt) + finderFit(string(m.query[m.qscroll:]), room)
}

func (m *finderModel) renderList(th tui.Theme, w, rows int) []string {
	out := make([]string, 0, rows)
	end := min(len(m.items), m.viewTop+rows)
	cols := finderListColumns(m.items[min(m.viewTop, end):end])
	for i := m.viewTop; i < end; i++ {
		out = append(out, m.renderItem(th, m.items[i], i == m.cursor, cols, w))
	}
	if len(out) == 0 && rows > 0 {
		msg := " no matches"
		if m.hint != "" {
			msg = " " + m.hint
		}
		if m.loading {
			msg = " …"
		}
		out = append(out, th.FGColor(th.Muted, finderFit(msg, w)))
	}
	for len(out) < rows {
		out = append(out, strings.Repeat(" ", max(w, 0)))
	}
	return out
}

// finderColumns are the widths of the right-aligned detail and count
// columns over the visible rows, so they line up and never truncate.
type finderColumns struct{ detail, count int }

func finderListColumns(items []finderItem) finderColumns {
	var c finderColumns
	for _, item := range items {
		c.detail = max(c.detail, runewidth.StringWidth(sanitizeSessionTreeText(item.Detail)))
		if item.Count > 0 {
			c.count = max(c.count, len(strconv.Itoa(item.Count)))
		}
	}
	return c
}

// renderItem renders "▸ group · label   detail  count ●" in w cells. A
// label "group · rest" shows its group in the accent color. The detail and
// count columns keep their width; only the label is truncated, and the
// columns are dropped when the label would get fewer than 8 cells.
func (m *finderModel) renderItem(th tui.Theme, item finderItem, selected bool, cols finderColumns, w int) string {
	marker := "  "
	if selected {
		marker = "▸ "
	}
	count := ""
	if item.Count > 0 {
		count = strconv.Itoa(item.Count)
	}
	badge := " "
	if item.Live {
		badge = "●"
	}
	detail := sanitizeSessionTreeText(item.Detail)
	detailCol := ""
	if cols.detail > 0 {
		detailCol = "  " + strings.Repeat(" ", cols.detail-runewidth.StringWidth(detail)) + detail
	}
	countCol := ""
	if cols.count > 0 {
		countCol = "  " + strings.Repeat(" ", cols.count-len(count)) + count
	}
	tail := " " + badge + " "
	avail := w - runewidth.StringWidth(marker+detailCol+countCol+tail)
	if avail < 8 {
		detailCol = ""
		avail = w - runewidth.StringWidth(marker+countCol+tail)
	}
	if avail < 1 {
		return finderFit(marker+sanitizeSessionTreeText(item.Label), w)
	}
	label := finderFit(sanitizeSessionTreeText(item.Label), avail)
	if selected {
		return th.Highlight(marker + label + detailCol + countCol + tail)
	}
	var b strings.Builder
	b.WriteString(marker)
	b.WriteString(renderFinderLabel(th, label, avail))
	b.WriteString(th.FGColor(th.Muted, detailCol))
	b.WriteString(countCol)
	if item.Live {
		b.WriteString(" " + th.FGColor(th.Accent, badge) + " ")
	} else {
		b.WriteString(tail)
	}
	return b.String()
}

// renderFinderLabel fits label to w cells; a "group · rest" label shows
// its group in the accent color.
func renderFinderLabel(th tui.Theme, label string, w int) string {
	label = finderFit(label, w)
	group, rest, ok := strings.Cut(label, " · ")
	if !ok {
		return label
	}
	return th.FGColor(th.Accent, group) + th.FGColor(th.Muted, " · ") + rest
}

func finderRoleLabel(role provider.Role) string {
	if role == provider.RoleUser {
		return "you"
	}
	return "agent"
}

// formatHitTime renders a hit's local time to the second: hits of one
// session usually share a day, so the date alone does not tell them apart.
func formatHitTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// renderHitHeader renders "you · 2026-10-29 14:32:05 compacted" in w
// cells: the role in its chat color, the time and badge dimmed, so the
// header reads apart from the snippet text below it.
func renderHitHeader(th tui.Theme, hit finderHit, w int) string {
	role, roleColor := " "+finderRoleLabel(hit.Role), th.Assistant
	if hit.Role == provider.RoleUser {
		roleColor = th.User
	}
	meta := " · " + formatHitTime(hit.Time)
	if hit.Compacted {
		meta += " compacted"
	}
	rw := runewidth.StringWidth(role)
	if rw >= w {
		return th.FGColor(roleColor, finderFit(role, w))
	}
	return th.FGColor(roleColor, role) + th.FGColor(th.DimColor(th.Muted, 40), finderFit(meta, w-rw))
}

// renderHitSnippet renders the snippet windowed around the first match
// with terms highlighted, in exactly w cells.
func renderHitSnippet(th tui.Theme, text string, terms []string, w int) string {
	text = strings.Join(strings.Fields(sanitizeSessionTreeText(text)), " ")
	if text == "" {
		return th.FGColor(th.DimColor(th.Muted, 40), finderFit(" …", w))
	}
	text = windowSessionSearchExcerpt(text, tui.FirstTermMatch(text, terms), max(w-1, 1))
	line := th.FGColor(th.FG, finderFit(" "+text, w))
	line, _ = tui.HighlightTerms(line, terms, th)
	return line
}

func (m *finderModel) renderPreview(th tui.Theme, terms []string, w, rows int) []string {
	out := make([]string, 0, rows)
	item, ok := m.selected()
	if ok && len(item.Hits) > 0 {
		window := max(rows/2, 1)
		top := clampViewTop(0, m.hit, window, len(item.Hits))
		for i := top; i < len(item.Hits) && len(out)+2 <= max(rows, 2); i++ {
			header := renderHitHeader(th, item.Hits[i], w)
			snippet := renderHitSnippet(th, item.Hits[i].Text, terms, w)
			if i == m.hit {
				header, snippet = tui.HighlightLine(header, th), tui.HighlightLine(snippet, th)
			}
			out = append(out, header)
			if len(out) < rows {
				out = append(out, snippet)
			}
		}
	} else if card, has := m.cards[item.Key]; ok && rows > 0 && has && !card.empty() {
		out = renderFinderCard(th, item, card, w, rows)
	} else if ok && rows > 0 {
		out = append(out, th.FGColor(th.Muted, finderFit(" "+sanitizeSessionTreeText(item.Detail), w)))
	}
	for len(out) > rows {
		out = out[:rows]
	}
	for len(out) < rows {
		out = append(out, strings.Repeat(" ", max(w, 0)))
	}
	return out
}

func (m *finderModel) renderOneLinePreview(th tui.Theme, terms []string, w int) string {
	item, ok := m.selected()
	if !ok || len(item.Hits) == 0 {
		return strings.Repeat(" ", w)
	}
	line := renderHitSnippet(th, item.Hits[min(m.hit, len(item.Hits)-1)].Text, terms, w)
	return tui.HighlightLine(line, th)
}

func (m *finderModel) renderFooter(th tui.Theme, w int) string {
	if m.mode == finderModeConfirmDelete {
		label := ""
		if item, ok := m.selected(); ok {
			label = sanitizeSessionTreeText(item.Label)
		}
		return th.FGColor(th.Warning, finderFit(" delete "+label+"? y confirm · any key cancel", w))
	}
	return th.FGColor(th.Muted, finderFit(" "+finderFooterHelp, w))
}
