package modes

import (
	"fmt"
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
		lines = append(lines, th.FGColor(th.Warning, pickerFit(" "+sanitizeSessionTreeText(m.err), width)))
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
	return finderQueryRow, m.query.Caret()
}

// renderTop draws the tabs (active one bracketed in the accent color) and
// the right-aligned status in exactly w cells; the status is dropped first
// when space runs out.
func (m *finderModel) renderTop(th tui.Theme, w int) string {
	names := make([]string, len(m.tabs))
	for i, src := range m.tabs {
		names[i] = sanitizeSessionTreeText(src.Title())
	}
	status, color := m.status(th)
	return renderPickerTabs(th, names, m.tab, status, color, w)
}

// finderFitBorder renders left, a ─ fill, and the corner in exactly w cells.
func finderFitBorder(left, corner string, w int) string {
	if runewidth.StringWidth(left)+1 > w {
		return pickerFit(strings.Repeat("─", w-1), w-1) + corner
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
	return m.query.Render(th, w)
}

func (m *finderModel) renderList(th tui.Theme, w, rows int) []string {
	out := make([]string, 0, rows)
	end := min(len(m.items), m.viewTop+rows)
	visible := make([]pickerRow, 0, end-min(m.viewTop, end))
	for i := m.viewTop; i < end; i++ {
		visible = append(visible, m.items[i].row())
	}
	cols := pickerListColumns(visible)
	for i, row := range visible {
		out = append(out, renderPickerRow(th, row, m.viewTop+i == m.cursor, cols, w))
	}
	if len(out) == 0 && rows > 0 {
		msg := " no matches"
		if m.hint != "" {
			msg = " " + m.hint
		}
		if m.loading {
			msg = " …"
		}
		out = append(out, th.FGColor(th.Muted, pickerFit(msg, w)))
	}
	for len(out) < rows {
		out = append(out, strings.Repeat(" ", max(w, 0)))
	}
	return out
}

// row is the list row of item: a live session gets the ● mark.
func (item finderItem) row() pickerRow {
	row := pickerRow{Label: item.Label, Detail: item.Detail, Count: item.Count}
	if item.Live {
		row.Mark = "●"
	}
	return row
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

// renderHitHeader renders "you · 2026-10-29 14:32:05" in w cells: the
// role in its chat color, the time dimmed, so the header reads apart from
// the snippet text below it.
func renderHitHeader(th tui.Theme, hit finderHit, w int) string {
	meta := " · " + formatHitTime(hit.Time)
	return renderRoleHeader(th, hit.Role, finderRoleLabel(hit.Role), meta, w)
}

// renderRoleHeader renders " label" in the chat color of role followed by
// the dimmed meta, in exactly w cells.
func renderRoleHeader(th tui.Theme, role provider.Role, label, meta string, w int) string {
	color := th.Assistant
	if role == provider.RoleUser {
		color = th.User
	}
	label = " " + label
	lw := runewidth.StringWidth(label)
	if lw >= w {
		return th.FGColor(color, pickerFit(label, w))
	}
	return th.FGColor(color, label) + th.FGColor(th.DimColor(th.Muted, 40), pickerFit(meta, w-lw))
}

// renderHitSnippet renders the snippet windowed around the first match
// with terms highlighted, in exactly w cells.
func renderHitSnippet(th tui.Theme, text string, terms []string, w int) string {
	text = strings.Join(strings.Fields(sanitizeSessionTreeText(text)), " ")
	if text == "" {
		return th.FGColor(th.DimColor(th.Muted, 40), pickerFit(" …", w))
	}
	text = windowSessionSearchExcerpt(text, tui.FirstTermMatch(text, terms), max(w-1, 1))
	line := th.FGColor(th.FG, pickerFit(" "+text, w))
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
	} else if e, has := m.cards[item.Key]; ok && rows > 0 && has && !e.card.empty() {
		out = renderFinderCard(th, item, e.card, w, rows)
	} else if ok && rows > 0 {
		out = append(out, th.FGColor(th.Muted, pickerFit(" "+sanitizeSessionTreeText(item.Detail), w)))
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
		return th.FGColor(th.Warning, pickerFit(" delete "+label+"? y confirm · any key cancel", w))
	}
	return th.FGColor(th.Muted, pickerFit(" "+finderFooterHelp, w))
}
