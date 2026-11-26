package modes

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/mattn/go-runewidth"
	"github.com/sahilm/fuzzy"

	"github.com/bnema/zut/packages/tui"
)

// Building blocks shared by the list pickers (/find, the model picker and
// the rescue picker): a query field, tabs, list rows with right-aligned
// columns, and fuzzy scoring. Each picker keeps its own state machine.

// pickerFit truncates s (plain text) to w display cells and pads it to
// exactly w cells.
func pickerFit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) > w {
		s = runewidth.Truncate(s, w, "…")
	}
	return s + strings.Repeat(" ", w-runewidth.StringWidth(s))
}

// pickerQuery is a single-line query field rendered as " > text".
type pickerQuery struct {
	text   []rune
	cursor int
	scroll int // first rune shown when the text overflows
}

const pickerQueryPrompt = " > "

func (q *pickerQuery) String() string { return string(q.text) }

func (q *pickerQuery) Reset() { q.text, q.cursor, q.scroll = nil, 0, 0 }

// Edit applies single-line editing keys and reports whether the text
// changed. Left/Right/Home/End move the caret.
func (q *pickerQuery) Edit(k tui.Key) bool {
	switch k.Kind {
	case tui.KeyRune:
		if k.Ctrl || !unicode.IsPrint(k.Rune) {
			return false
		}
		return q.insert([]rune{k.Rune})
	case tui.KeyPaste:
		return q.insert(pickerPasteRunes(k.Paste))
	case tui.KeyBackspace:
		if q.cursor == 0 {
			return false
		}
		q.text = append(q.text[:q.cursor-1], q.text[q.cursor:]...)
		q.cursor--
		return true
	case tui.KeyCtrlW:
		end := q.cursor
		start := end
		for start > 0 && unicode.IsSpace(q.text[start-1]) {
			start--
		}
		for start > 0 && !unicode.IsSpace(q.text[start-1]) {
			start--
		}
		if start == end {
			return false
		}
		q.text = append(q.text[:start], q.text[end:]...)
		q.cursor = start
		return true
	case tui.KeyCtrlU:
		if q.cursor == 0 {
			return false
		}
		q.text = append([]rune(nil), q.text[q.cursor:]...)
		q.cursor = 0
		return true
	case tui.KeyLeft:
		q.cursor = max(q.cursor-1, 0)
	case tui.KeyRight:
		q.cursor = min(q.cursor+1, len(q.text))
	case tui.KeyHome, tui.KeyCtrlA:
		q.cursor = 0
	case tui.KeyEnd, tui.KeyCtrlE:
		q.cursor = len(q.text)
	}
	return false
}

func (q *pickerQuery) insert(rs []rune) bool {
	if len(rs) == 0 {
		return false
	}
	next := make([]rune, 0, len(q.text)+len(rs))
	next = append(next, q.text[:q.cursor]...)
	next = append(next, rs...)
	next = append(next, q.text[q.cursor:]...)
	q.text = next
	q.cursor += len(rs)
	return true
}

// Render draws the field in exactly w cells, scrolling horizontally so the
// caret stays visible.
func (q *pickerQuery) Render(th tui.Theme, w int) string {
	room := max(w-runewidth.StringWidth(pickerQueryPrompt), 0)
	// The caret takes one cell after the text before it.
	field := max(room-1, 0)
	q.scroll = min(q.scroll, q.cursor)
	for q.scroll < q.cursor && runewidth.StringWidth(string(q.text[q.scroll:q.cursor])) > field {
		q.scroll++
	}
	if runewidth.StringWidth(string(q.text)) <= field {
		q.scroll = 0
	}
	return th.FGColor(th.Accent, pickerQueryPrompt) + pickerFit(string(q.text[q.scroll:]), room)
}

// Caret is the caret column in the line Render last returned.
func (q *pickerQuery) Caret() int {
	start := min(q.scroll, q.cursor)
	return runewidth.StringWidth(pickerQueryPrompt) + runewidth.StringWidth(string(q.text[start:q.cursor]))
}

// pickerPasteRunes keeps the printable runes of a paste for a single-line
// field; other whitespace (newlines, tabs) becomes a space.
func pickerPasteRunes(paste string) []rune {
	var rs []rune
	for _, r := range paste {
		if unicode.IsPrint(r) {
			rs = append(rs, r)
		} else if unicode.IsSpace(r) {
			rs = append(rs, ' ')
		}
	}
	return rs
}

// renderPickerTabs draws the tabs (the active one bracketed in the accent
// color) and the right-aligned status in exactly w cells; the status is
// dropped first when space runs out. names must be sanitized plain text.
func renderPickerTabs(th tui.Theme, names []string, active int, status string, statusColor tui.TerminalColor, w int) string {
	labels := make([]string, len(names))
	tabsW := 1 // leading space
	for i, name := range names {
		if i == active {
			name = "[" + name + "]"
		}
		labels[i] = name
		tabsW += runewidth.StringWidth(name)
	}
	if len(labels) > 1 {
		tabsW += len(labels) - 1
	}
	if tabsW > w {
		return renderPickerTabsOverflow(th, labels, active, w)
	}
	var b strings.Builder
	b.WriteString(" ")
	for i, label := range labels {
		if i > 0 {
			b.WriteString(" ")
		}
		if i == active {
			b.WriteString(th.FGColor(th.Accent, label))
		} else {
			b.WriteString(th.FGColor(th.Muted, label))
		}
	}
	status += " "
	sw := runewidth.StringWidth(status)
	if tabsW+1+sw > w {
		b.WriteString(strings.Repeat(" ", w-tabsW))
		return b.String()
	}
	b.WriteString(strings.Repeat(" ", w-tabsW-sw))
	b.WriteString(th.FGColor(statusColor, status))
	return b.String()
}

// renderPickerTabsOverflow draws tabs that do not fit in w cells: leading
// tabs give way ("… " prefix) until the active one is visible, trailing
// tabs that do not fit are dropped, and the status is omitted.
func renderPickerTabsOverflow(th tui.Theme, labels []string, active, w int) string {
	width := func(start int) int { // cells up to the end of the active tab
		lead := " "
		if start > 0 {
			lead = " … "
		}
		return runewidth.StringWidth(lead + strings.Join(labels[start:active+1], " "))
	}
	start := 0
	for start < active && width(start) > w {
		start++
	}
	var b strings.Builder
	used := 1
	b.WriteString(" ")
	if start > 0 {
		b.WriteString(th.FGColor(th.Muted, "… "))
		used += 2
	}
	for i := start; i < len(labels); i++ {
		lw := runewidth.StringWidth(labels[i])
		sep := 0
		if i > start {
			sep = 1
		}
		if used+sep+lw > w {
			if i == active {
				// Even alone the active tab is too wide: truncate it.
				b.WriteString(strings.Repeat(" ", sep))
				b.WriteString(th.FGColor(th.Accent, pickerFit(labels[i], w-used-sep)))
				used = w
			}
			break
		}
		b.WriteString(strings.Repeat(" ", sep))
		color := th.Muted
		if i == active {
			color = th.Accent
		}
		b.WriteString(th.FGColor(color, labels[i]))
		used += sep + lw
	}
	if used < w {
		b.WriteString(strings.Repeat(" ", w-used))
	}
	return b.String()
}

// pickerRow is one list row: "▸ group · label note   detail  count mark".
type pickerRow struct {
	Label  string // a "group · rest" label shows its group in the accent color
	Note   string // dimmed text right after the label, dropped first
	Detail string // right-aligned dimmed column
	Count  int    // right-aligned count column, hidden when 0
	Mark   string // one-cell badge in the accent color, blank when empty
}

// pickerColumns are the widths of the right-aligned detail and count
// columns over the visible rows, so they line up and never truncate.
type pickerColumns struct{ detail, count int }

func pickerListColumns(rows []pickerRow) pickerColumns {
	var c pickerColumns
	for _, row := range rows {
		c.detail = max(c.detail, runewidth.StringWidth(sanitizeSessionTreeText(row.Detail)))
		if row.Count > 0 {
			c.count = max(c.count, len(strconv.Itoa(row.Count)))
		}
	}
	return c
}

// renderPickerRow renders row in exactly w cells. The detail and count
// columns keep their width; only the label is truncated, and the detail
// column is dropped when the label would get fewer than 8 cells.
func renderPickerRow(th tui.Theme, row pickerRow, selected bool, cols pickerColumns, w int) string {
	marker := "  "
	if selected {
		marker = "▸ "
	}
	count := ""
	if row.Count > 0 {
		count = strconv.Itoa(row.Count)
	}
	mark := row.Mark
	if mark == "" {
		mark = " "
	}
	detail := sanitizeSessionTreeText(row.Detail)
	detailCol := ""
	if cols.detail > 0 {
		detailCol = "  " + strings.Repeat(" ", max(cols.detail-runewidth.StringWidth(detail), 0)) + detail
	}
	countCol := ""
	if cols.count > 0 {
		countCol = "  " + strings.Repeat(" ", max(cols.count-len(count), 0)) + count
	}
	tail := " " + mark + " "
	avail := w - runewidth.StringWidth(marker+detailCol+countCol+tail)
	if avail < 8 {
		detailCol = ""
		avail = w - runewidth.StringWidth(marker+countCol+tail)
	}
	label := sanitizeSessionTreeText(row.Label)
	if avail < 1 {
		return pickerFit(marker+label, w)
	}
	note := sanitizeSessionTreeText(row.Note)
	labelW := runewidth.StringWidth(label)
	noteW := 0
	if note != "" && labelW+2 < avail {
		noteW = avail - labelW
	} else {
		labelW = avail
	}
	label = pickerFit(label, labelW)
	if noteW > 0 {
		note = pickerFit(" "+note, noteW)
	} else {
		note = ""
	}
	if selected {
		return th.Highlight(marker + label + note + detailCol + countCol + tail)
	}
	var b strings.Builder
	b.WriteString(marker)
	b.WriteString(renderPickerLabel(th, label, labelW))
	if note != "" {
		b.WriteString(th.FGColor(th.DimColor(th.Muted, 40), note))
	}
	b.WriteString(th.FGColor(th.Muted, detailCol))
	b.WriteString(countCol)
	if row.Mark != "" {
		b.WriteString(" " + th.FGColor(th.Accent, mark) + " ")
	} else {
		b.WriteString(tail)
	}
	return b.String()
}

// renderPickerLabel fits label to w cells; a "group · rest" label shows
// its group in the accent color.
func renderPickerLabel(th tui.Theme, label string, w int) string {
	label = pickerFit(label, w)
	group, rest, ok := strings.Cut(label, " · ")
	if !ok {
		return label
	}
	return th.FGColor(th.Accent, group) + th.FGColor(th.Muted, " · ") + rest
}

// pickerFuzzyScores scores each label against query with the finder's
// fuzzy matcher (case-insensitive, characters in order). matched[i] is
// false when label i does not contain the query characters in order.
func pickerFuzzyScores(query string, labels []string) (scores []int, matched []bool) {
	scores = make([]int, len(labels))
	matched = make([]bool, len(labels))
	if query == "" {
		return scores, matched
	}
	for _, m := range fuzzy.Find(query, labels) {
		scores[m.Index] = m.Score
		matched[m.Index] = true
	}
	return scores, matched
}
