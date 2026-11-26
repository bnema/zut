package modes

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

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
	width, inner int
	split        bool // list and preview side by side
	leftW        int
	rightW       int
	listRows     int
	previewRows  int // stacked preview rows; 0 in split mode
	oneLine      bool
}

func (m *finderModel) layout(width, height int) finderLayout {
	l := finderLayout{width: width, inner: max(width-2, 0)}
	fixed := 6 // top, query, separator, separator, footer, bottom
	if m.err != "" {
		fixed++
	}
	body := max(height-fixed, 1)
	switch {
	case width >= finderSplitMinWidth:
		l.split = true
		l.leftW = (l.inner - 1) * 45 / 100
		l.rightW = l.inner - 1 - l.leftW
		l.listRows = body
	case width >= finderPreviewMinWidth && body >= 3:
		// One extra separator row between list and preview.
		l.listRows = max((body-1)*60/100, 1)
		l.previewRows = max(body-1-l.listRows, 1)
	default:
		// Narrow or short: list plus a one-line preview of the current hit.
		l.listRows = max(body-1, 1)
		l.oneLine = true
	}
	return l
}

// Render draws the finder box. Every returned line is exactly width
// display cells wide. Render updates the list scroll offset.
func (m *finderModel) Render(th tui.Theme, width, height int) []string {
	if width < 4 {
		return nil
	}
	l := m.layout(width, height)
	m.viewTop = clampViewTop(m.viewTop, m.cursor, l.listRows, len(m.items))
	border := func(s string) string { return th.FGColor(th.Muted, s) }
	row := func(content string) string { return border("│") + content + border("│") }

	lines := []string{m.renderTop(th, l), row(m.renderQuery(th, l.inner))}
	if m.err != "" {
		lines = append(lines, row(th.FGColor(th.Warning, finderFit(" "+sanitizeSessionTreeText(m.err), l.inner))))
	}
	terms, _ := sessionindex.ParseQuery(m.Query())

	if l.split {
		lines = append(lines, border("├"+strings.Repeat("─", l.leftW)+"┬"+strings.Repeat("─", l.rightW)+"┤"))
		list := m.renderList(th, l.leftW, l.listRows)
		preview := m.renderPreview(th, terms, l.rightW, l.listRows)
		for i := range list {
			lines = append(lines, border("│")+list[i]+border("│")+preview[i]+border("│"))
		}
		lines = append(lines, border("├"+strings.Repeat("─", l.leftW)+"┴"+strings.Repeat("─", l.rightW)+"┤"))
	} else {
		rule := border("├" + strings.Repeat("─", l.inner) + "┤")
		lines = append(lines, rule)
		for _, r := range m.renderList(th, l.inner, l.listRows) {
			lines = append(lines, row(r))
		}
		lines = append(lines, rule)
		if l.oneLine {
			lines = append(lines, row(m.renderOneLinePreview(th, terms, l.inner)))
		} else {
			for _, r := range m.renderPreview(th, terms, l.inner, l.previewRows) {
				lines = append(lines, row(r))
			}
		}
		if !l.oneLine {
			lines = append(lines, rule)
		}
	}
	lines = append(lines, row(m.renderFooter(th, l.inner)))
	lines = append(lines, border("╰"+strings.Repeat("─", l.inner)+"╯"))
	return lines
}

// CursorPos returns the 0-based row/col of the query caret within the
// rendered box, or -1, -1 when the finder is closed. It uses the query
// scroll offset computed by the last Render.
func (m *finderModel) CursorPos() (row, col int) {
	if !m.Active() {
		return -1, -1
	}
	start := min(m.qscroll, m.qcursor)
	return 1, 4 + runewidth.StringWidth(string(m.query[start:m.qcursor]))
}

func (m *finderModel) renderTop(th tui.Theme, l finderLayout) string {
	title := "╭─ Find "
	end := " ─╮"
	var tabsPlain []string
	tabsWidth := 0
	for i, src := range m.tabs {
		name := sanitizeSessionTreeText(src.Title())
		if i == m.tab {
			name = "[" + name + "]"
		}
		tabsPlain = append(tabsPlain, name)
		tabsWidth += runewidth.StringWidth(name)
	}
	if len(tabsPlain) > 1 {
		tabsWidth += len(tabsPlain) - 1
	}
	fill := l.width - runewidth.StringWidth(title) - runewidth.StringWidth(end) - tabsWidth - 1
	if len(tabsPlain) == 0 || fill < 1 {
		return th.FGColor(th.Muted, finderFitBorder(title, "╮", l.width))
	}
	var b strings.Builder
	b.WriteString(th.FGColor(th.Muted, title+strings.Repeat("─", fill)+" "))
	for i, name := range tabsPlain {
		if i > 0 {
			b.WriteString(" ")
		}
		if i == m.tab {
			b.WriteString(th.FGColor(th.Accent, name))
		} else {
			b.WriteString(th.FGColor(th.Muted, name))
		}
	}
	b.WriteString(th.FGColor(th.Muted, end))
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
	prompt := " > "
	status, color := m.status(th)
	status += " "
	room := w - runewidth.StringWidth(prompt)
	sw := runewidth.StringWidth(status)
	qw := runewidth.StringWidth(string(m.query))
	if qw+1+sw <= room {
		m.qscroll = 0
		return th.FGColor(th.Accent, prompt) + string(m.query) + strings.Repeat(" ", room-qw-sw) + th.FGColor(color, status)
	}
	// The query wins over the status; scroll it horizontally so the caret
	// (one cell after the text before it) stays inside the field.
	field := max(room-1, 0)
	m.qscroll = min(m.qscroll, m.qcursor)
	for m.qscroll < m.qcursor && runewidth.StringWidth(string(m.query[m.qscroll:m.qcursor])) > field {
		m.qscroll++
	}
	return th.FGColor(th.Accent, prompt) + finderFit(string(m.query[m.qscroll:]), room)
}

func (m *finderModel) renderList(th tui.Theme, w, rows int) []string {
	out := make([]string, 0, rows)
	for i := m.viewTop; i < len(m.items) && len(out) < rows; i++ {
		out = append(out, m.renderItem(th, m.items[i], i == m.cursor, w))
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

func (m *finderModel) renderItem(th tui.Theme, item finderItem, selected bool, w int) string {
	marker := "  "
	if selected {
		marker = "▸ "
	}
	right := ""
	if item.Count > 0 {
		right = strconv.Itoa(item.Count)
	}
	badge := " "
	if item.Live {
		badge = "●"
	}
	right += " " + badge + " "
	label := sanitizeSessionTreeText(item.Label)
	detail := sanitizeSessionTreeText(item.Detail)
	avail := w - runewidth.StringWidth(marker) - runewidth.StringWidth(right)
	if avail < 1 {
		return finderFit(marker+label, w)
	}
	label = finderFit(label, avail)
	label = strings.TrimRight(label, " ")
	lw := runewidth.StringWidth(label)
	detailRoom := avail - lw - 2
	if detail == "" || detailRoom < 4 {
		detail = ""
	} else {
		detail = strings.TrimRight(finderFit(detail, detailRoom), " ")
	}
	sep := ""
	if detail != "" {
		sep = "  "
	}
	gap := strings.Repeat(" ", avail-lw-runewidth.StringWidth(sep+detail))
	if selected {
		return th.Highlight(marker + label + sep + detail + gap + right)
	}
	var b strings.Builder
	b.WriteString(marker + label + sep)
	b.WriteString(th.FGColor(th.Muted, detail))
	b.WriteString(gap)
	if item.Live {
		b.WriteString(right[:len(right)-len(badge)-1])
		b.WriteString(th.FGColor(th.Accent, badge))
		b.WriteString(" ")
	} else {
		b.WriteString(right)
	}
	return b.String()
}

func finderRoleLabel(role provider.Role) string {
	if role == provider.RoleUser {
		return "you"
	}
	return "agent"
}

// renderHitHeader renders "you · 3 days ago [compacted]" in w cells.
func renderHitHeader(th tui.Theme, hit finderHit, w int) string {
	head := " " + finderRoleLabel(hit.Role) + " · " + formatRelative(hit.Time)
	if !hit.Compacted {
		return th.FGColor(th.Muted, finderFit(head, w))
	}
	badge := " compacted"
	if runewidth.StringWidth(head+badge) > w {
		return th.FGColor(th.Muted, finderFit(head, w))
	}
	return th.FGColor(th.Muted, head) + th.FGColor(th.DimColor(th.Muted, 40), finderFit(badge, w-runewidth.StringWidth(head)))
}

// renderHitSnippet renders the snippet windowed around the first match
// with terms highlighted, in exactly w cells.
func renderHitSnippet(th tui.Theme, text string, terms []string, w int) string {
	text = strings.Join(strings.Fields(sanitizeSessionTreeText(text)), " ")
	if text == "" {
		return th.FGColor(th.DimColor(th.Muted, 40), finderFit(" …", w))
	}
	var indexes []int
	if at := finderFirstMatch(text, terms); at >= 0 {
		indexes = []int{at}
	}
	text, _ = windowSessionSearchExcerpt(text, indexes, max(w-1, 1))
	line := th.FGColor(th.Muted, finderFit(" "+text, w))
	line, _ = tui.HighlightTerms(line, terms, th)
	return line
}

// finderFirstMatch returns the byte offset of the first word-prefix match
// of any term in text, or -1.
func finderFirstMatch(text string, terms []string) int {
	prev := rune(-1)
	for i, r := range text {
		if prev < 0 || !(unicode.IsLetter(prev) || unicode.IsDigit(prev) || prev == '_') {
			for _, t := range terms {
				if t != "" && finderHasPrefixFold(text[i:], t) {
					return i
				}
			}
		}
		prev = r
	}
	return -1
}

func finderHasPrefixFold(s, prefix string) bool {
	for _, pr := range prefix {
		r, size := utf8.DecodeRuneInString(s)
		if size == 0 || unicode.ToLower(r) != unicode.ToLower(pr) {
			return false
		}
		s = s[size:]
	}
	return true
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
	if m.confirmDelete {
		label := ""
		if item, ok := m.selected(); ok {
			label = sanitizeSessionTreeText(item.Label)
		}
		return th.FGColor(th.Warning, finderFit(" delete "+label+"? y confirm · any key cancel", w))
	}
	return th.FGColor(th.Muted, finderFit(" "+finderFooterHelp, w))
}
