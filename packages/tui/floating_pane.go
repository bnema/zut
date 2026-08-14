package tui

import "strings"

// FloatingPane is a stateful viewport for an independent TUI view. It keeps
// its scroll position while the same view is active, follows a focused row
// into view, and composes that view over a live background frame.
//
// A FloatingPane is deliberately frontend-neutral: callers own their view's
// data and key handling, while the pane owns geometry, clipping, and visual
// composition.
type FloatingPane struct {
	id     string
	scroll int
}

// FloatingPaneRect describes a pane's outer border in zero-based terminal
// coordinates. Content occupies the rectangle inside that border.
type FloatingPaneRect struct {
	X, Y          int
	Width, Height int
	Drawer        bool
	Borderless    bool
}

func (r FloatingPaneRect) ContentWidth() int {
	if r.Drawer || r.Borderless {
		return max(1, r.Width)
	}
	return max(1, r.Width-2)
}

func (r FloatingPaneRect) ContentHeight() int {
	if r.Borderless {
		return max(1, r.Height)
	}
	if r.Drawer {
		return max(1, r.Height-1)
	}
	return max(1, r.Height-2)
}

func (r FloatingPaneRect) contentX() int {
	if r.Drawer || r.Borderless {
		return r.X
	}
	return r.X + 1
}

func (r FloatingPaneRect) contentY() int {
	if r.Borderless {
		return r.Y
	}
	return r.Y + 1
}

// FloatingPaneMaxRect returns the largest pane for the current terminal. The
// final pane is shortened to its content by Compose. Below 80 columns it
// becomes a full-width bottom drawer so dialogs remain readable and usable.
func FloatingPaneMaxRect(cols, rows int) FloatingPaneRect {
	if cols < 1 || rows < 1 {
		return FloatingPaneRect{}
	}
	height := max(1, rows*4/5)
	if rows >= 5 {
		height = max(5, height)
	}
	if height > rows {
		height = rows
	}
	if cols < 80 {
		return FloatingPaneRect{Y: rows - height, Width: cols, Height: height, Drawer: true}
	}
	width := max(6, cols*4/5)
	if width > cols {
		width = cols
	}
	return FloatingPaneRect{
		X:      (cols - width) / 2,
		Y:      (rows - height) / 2,
		Width:  width,
		Height: height,
	}
}

// Reset discards the viewport position. Callers normally do not need this:
// Compose resets it automatically when the view identity changes.
func (p *FloatingPane) Reset() {
	if p == nil {
		return
	}
	p.id = ""
	p.scroll = 0
}

// FloatingPaneFrame contains the dimmed background and foreground rows to
// paint at absolute terminal positions. Renderer.DrawFloating consumes it.
type FloatingPaneFrame struct {
	Background []string
	Lines      []string
	Rect       FloatingPaneRect
}

// Compose returns a complete floating-pane frame. title is centered in the
// pane border. contentCursorRow is relative to content and is used both to
// retain the pane's own viewport and to return an absolute terminal cursor
// row. Pass -1 when the foreground has no text cursor.
func (p *FloatingPane) Compose(theme Theme, id, title string, background, content []string, cols, rows, contentCursorRow, contentCursorCol int) (pane FloatingPaneFrame, cursorRow, cursorCol int) {
	pane.Background = DimLines(fitFloatingBackground(background, cols, rows))
	if cols < 1 || rows < 1 {
		return pane, -1, 0
	}
	if p == nil {
		p = &FloatingPane{}
	}
	if p.id != id {
		p.id = id
		p.scroll = 0
	}

	pane.Rect = FloatingPaneMaxRect(cols, rows)
	chromeRows := 2
	if pane.Rect.Drawer {
		chromeRows = 1
	}
	if len(content)+chromeRows < pane.Rect.Height {
		pane.Rect.Height = max(1, len(content)+chromeRows)
		if pane.Rect.Drawer {
			pane.Rect.Y = rows - pane.Rect.Height
		} else {
			pane.Rect.Y = (rows - pane.Rect.Height) / 2
		}
	}
	tooSmall := pane.Rect.Width < 3 || (!pane.Rect.Drawer && pane.Rect.Height < 3) || (pane.Rect.Drawer && pane.Rect.Height < 2)
	if tooSmall {
		// There is no room for a reliable border. Keep the foreground usable
		// on a tiny terminal by treating the entire screen as its viewport.
		pane.Rect = FloatingPaneRect{Width: cols, Height: rows, Borderless: true}
	}

	viewportRows := pane.Rect.ContentHeight()
	p.follow(contentCursorRow, len(content), viewportRows)
	content = p.viewport(content, viewportRows)
	pane.Lines = floatingPaneLines(theme, pane.Rect, title, content)

	if contentCursorRow >= p.scroll && contentCursorRow < p.scroll+viewportRows {
		cursorRow = pane.Rect.contentY() + contentCursorRow - p.scroll
		cursorCol = pane.Rect.contentX() + max(0, contentCursorCol)
		cursorCol = min(cursorCol, pane.Rect.contentX()+pane.Rect.ContentWidth()-1)
		cursorCol = min(cursorCol, cols-1)
		return pane, cursorRow, cursorCol
	}
	return pane, -1, 0
}

func (p *FloatingPane) follow(focusRow, contentRows, viewportRows int) {
	if focusRow < 0 || viewportRows < 1 {
		p.scroll = clampFloatingScroll(p.scroll, contentRows, viewportRows)
		return
	}
	if focusRow < p.scroll {
		p.scroll = focusRow
	} else if focusRow >= p.scroll+viewportRows {
		p.scroll = focusRow - viewportRows + 1
	}
	p.scroll = clampFloatingScroll(p.scroll, contentRows, viewportRows)
}

func (p *FloatingPane) viewport(content []string, rows int) []string {
	if rows < 1 {
		return nil
	}
	p.scroll = clampFloatingScroll(p.scroll, len(content), rows)
	end := min(len(content), p.scroll+rows)
	return content[p.scroll:end]
}

func clampFloatingScroll(scroll, contentRows, viewportRows int) int {
	if scroll < 0 {
		return 0
	}
	if maxScroll := max(0, contentRows-viewportRows); scroll > maxScroll {
		return maxScroll
	}
	return scroll
}

func fitFloatingBackground(background []string, cols, rows int) []string {
	frame := make([]string, rows)
	if cols < 1 || rows < 1 {
		return frame
	}
	if len(background) > rows {
		background = background[len(background)-rows:]
	}
	start := rows - len(background)
	for row, line := range background {
		frame[start+row] = truncateToWidth(line, cols)
	}
	return frame
}

func floatingPaneLines(theme Theme, rect FloatingPaneRect, title string, content []string) []string {
	if rect.Width < 1 || rect.Height < 1 {
		return nil
	}
	tooSmall := rect.Width < 3 || (!rect.Drawer && rect.Height < 3) || (rect.Drawer && rect.Height < 2)
	if tooSmall {
		lines := make([]string, rect.Height)
		copy(lines, content)
		return lines
	}
	lines := make([]string, rect.Height)
	if rect.Drawer {
		lines[0] = floatingPaneTitle(theme, title, rect.Width, false)
		for row := 0; row < rect.ContentHeight(); row++ {
			line := ""
			if row < len(content) {
				line = truncateToWidth(content[row], rect.ContentWidth())
			}
			if width := visibleWidth(line); width < rect.ContentWidth() {
				line += strings.Repeat(" ", rect.ContentWidth()-width)
			}
			lines[row+1] = line
		}
		return lines
	}
	border := theme.FGColor(theme.Muted, "│")
	lines[0] = floatingPaneTitle(theme, title, rect.Width, true)
	lines[len(lines)-1] = theme.FGColor(theme.Muted, "╰"+strings.Repeat("─", rect.Width-2)+"╯")
	for row := 0; row < rect.ContentHeight(); row++ {
		line := ""
		if row < len(content) {
			line = truncateToWidth(content[row], rect.ContentWidth())
		}
		if width := visibleWidth(line); width < rect.ContentWidth() {
			line += strings.Repeat(" ", rect.ContentWidth()-width)
		}
		lines[row+1] = border + line + border
	}
	return lines
}

func floatingPaneTitle(theme Theme, title string, width int, boxed bool) string {
	if width < 1 {
		return ""
	}
	interior := width
	left, right := "", ""
	if boxed {
		if width < 3 {
			return theme.FGColor(theme.Muted, strings.Repeat("─", width))
		}
		interior -= 2
		left, right = "╭", "╮"
	}
	label := ""
	if title != "" {
		label = " " + truncateToWidth(title, max(1, interior-2)) + " "
	}
	labelWidth := visibleWidth(label)
	if labelWidth > interior {
		label = truncateToWidth(label, interior)
		labelWidth = visibleWidth(label)
	}
	before := max(0, (interior-labelWidth)/2)
	after := max(0, interior-labelWidth-before)
	return theme.FGColor(theme.Muted, left+strings.Repeat("─", before)+label+strings.Repeat("─", after)+right)
}
