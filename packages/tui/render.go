package tui

import (
	"io"
	"os"
	"strings"

	"github.com/mattn/go-runewidth"
)

// runewidthRune reports the number of cells a rune occupies, pinned
// here so the renderer does not depend on the editor's helper.
func runewidthRune(r rune) int { return runewidth.RuneWidth(r) }

// Renderer maintains a previous frame and writes only the lines that
// changed on each Draw(). Callers pass a full target frame (slice of
// styled lines, already wrapped to width).
type Renderer struct {
	out  io.Writer
	prev []string
	rows int // terminal rows
	cols int // terminal cols

	// Cursor position after last draw (for placing input cursor).
	cursorRow int
	cursorCol int

	// prevHadImage tracks whether the previous frame contained an
	// inline-image escape so we can force a full clear+repaint whenever
	// the image set changes. prevHadKittyImage additionally ensures stale
	// Kitty image pixels are explicitly deleted when their layer disappears.
	prevHadImage      bool
	prevHadKittyImage bool

	// Main-screen flow renderer state. logLines is the full logical
	// buffer (chat + live bottom band) from the previous DrawLog call.
	// logViewportTop/logHardwareRow track where that logical buffer sits
	// in the terminal's visible viewport so we can diff safely, and bail
	// out to clear+replay when the diff would touch rows that are no
	// longer addressable.
	logChat   []string
	logBottom []string
	logLines  []string
	// Raw logical rows let DrawLog reject an unchanged frame before
	// truncating and repainting every transcript row.
	logRawChat        []string
	logRawBottom      []string
	logRawCols        int
	logRawRows        int
	logRawContentCols int
	logRawBackground  string
	logRawHasThemeBG  bool
	logViewportTop    int
	logHardwareRow    int
	logInit           bool

	// The right bar is a viewport overlay on top of the main-screen flow
	// renderer. Keeping its cache separate prevents persistent chrome from
	// replacing the transcript's logical buffer and native scrollback.
	rightBarActive   bool
	rightBarLines    []string
	rightBarMainCols int
	rightBarDimmed   bool

	// keepScrollback is true when we must NOT emit \x1b[3J
	// (erase-in-display 3, "clear scrollback").
	//
	// VS Code's integrated terminal (xterm.js) interprets \x1b[3J
	// as "also snap the viewport to the top of the remaining
	// buffer." Once the user has reopened a terminal with VS
	// Code's persistent-sessions feature on, there is real
	// replayed scrollback above the live cursor, and the snap is
	// visible: the host scrollbar jumps to the top every time we
	// do a full repaint (first paint, Ctrl+L via Renderer.Clear,
	// any writeFull(true) shrink). On every other terminal we
	// tested (iTerm, Ghostty, Kitty, Alacritty, Apple Terminal)
	// \x1b[3J just drops scrollback rows without moving the
	// viewport, which is what we actually want.
	//
	// The trade-off when keepScrollback is true: stale zut frames
	// remain in scrollback above the live view, so scrolling up
	// in VS Code's terminal shows old (already-superseded) zut
	// output. That is strictly less disruptive than the
	// scrollbar yanking to top on every Ctrl+L, and it is a
	// limitation specific to VS Code's terminal that we have no
	// way to work around without breaking other terminals.
	keepScrollback bool

	// theme is optional renderer-level styling applied at the final
	// row-writing boundary. In particular, Theme.Background is painted
	// as a full-width row background without making every View renderer
	// know about terminal padding and reset semantics.
	theme           Theme
	backgroundStyle string
}

// NewRenderer returns a renderer that writes to out.
//
// Detects VS Code's integrated terminal via $TERM_PROGRAM and, when
// detected, disables emission of \x1b[3J for the reasons documented
// on Renderer.keepScrollback. The env var is set by VS Code itself
// (and by Cursor, which forks VS Code's terminal — same xterm.js,
// same bug), so no user configuration is required.
func NewRenderer(out io.Writer) *Renderer {
	return &Renderer{
		out:            out,
		keepScrollback: os.Getenv("TERM_PROGRAM") == "vscode",
	}
}

// SetTheme updates renderer-level terminal styling. Changing the
// background affects every row, so cached frame state is invalidated.
func (r *Renderer) SetTheme(th Theme) {
	r.theme = th
	r.backgroundStyle = th.BackgroundStyle()
	r.Invalidate()
}

// Resize tells the renderer the current terminal size.
//
// On a real size change we also issue a clear-screen so the next Draw
// starts from a blank slate. Without the clear, characters from the
// old (wider) layout linger past the new right edge and rows from
// before the new bottom hang around as garbage.
func (r *Renderer) ResetScrollRegion() {
	if r.out != nil {
		_, _ = io.WriteString(r.out, SeqResetScrollRegion)
	}
}

func (r *Renderer) Resize(cols, rows int) {
	if cols != r.cols || rows != r.rows {
		r.cols = cols
		r.rows = rows
		r.prev = nil
		r.logChat = nil
		r.logBottom = nil
		r.logLines = nil
		r.logViewportTop = 0
		r.logHardwareRow = 0
		r.logInit = false
		r.rightBarActive = false
		r.rightBarLines = nil
		r.rightBarMainCols = 0
		if r.out != nil {
			if r.keepScrollback {
				// A resize is a discrete user action, like Ctrl+L: the
				// old-width frame must be fully purged or the in-place
				// viewport clear leaves the scrolled-away rows behind
				// (the user otherwise has to press Ctrl+L to fix it).
				// Emit \x1b[3J to drop the retained scrollback and
				// repaint clean, accepting VS Code's viewport-snap the
				// same way Clear does.
				_, _ = io.WriteString(r.out, SeqDeleteKittyImages+SeqClearScreen+SeqClearScrollback+MoveTo(1, 1))
			} else {
				// Clear both screen and (where safe) scrollback so stale
				// content from the old width doesn't bleed through. Move
				// to (1,1) so the next DrawLog/writeFull starts from a
				// clean slate. Use the no-home variant: the explicit
				// MoveTo below sets the cursor without triggering VS
				// Code's viewport-snap.
				_, _ = io.WriteString(r.out, SeqDeleteKittyImages+SeqClearScreenNoHome+r.clearScrollbackSeq()+MoveTo(1, 1))
			}
		}
	}
}

// Clear forces a full repaint on the next Draw and clears the screen
// plus scrollback. In main-screen flow mode this is required whenever
// already-emitted transcript layout changes (for example ctrl+o
// expand/collapse), because terminal scrollback cannot be edited
// reliably once printed.
func (r *Renderer) Clear() {
	r.prev = nil
	r.logChat = nil
	r.logBottom = nil
	r.logLines = nil
	r.logViewportTop = 0
	r.logHardwareRow = 0
	r.logInit = false
	r.rightBarActive = false
	r.rightBarLines = nil
	r.rightBarMainCols = 0
	if r.keepScrollback {
		// On VS Code's xterm.js the transcript is taller than the
		// viewport, so an in-place clear (home + erase-to-end) only
		// wipes the visible rows: the part that scrolled above the
		// viewport stays in the retained buffer and the next full
		// repaint stacks a duplicate above the live frame.
		//
		// Clear() is an explicit user refresh (Ctrl+L), so here we do
		// emit \x1b[3J to actually drop that scrollback, then home and
		// repaint. This is the one place we accept VS Code's
		// viewport-snap, because the user asked for a clean screen.
		// (Implicit repaints, e.g. closing a dialog, deliberately
		// avoid this path so they never snap the viewport.)
		if r.out != nil {
			_, _ = io.WriteString(r.out, SeqDeleteKittyImages+SeqClearScreen+SeqClearScrollback+MoveTo(1, 1))
		}
		return
	}
	_, _ = io.WriteString(r.out, SeqDeleteKittyImages+SeqClearScreenNoHome+r.clearScrollbackSeq()+MoveTo(1, 1))
}

// clearScrollbackSeq returns the scrollback-clear escape, or the
// empty string when we are running under a terminal where emitting
// it has user-visible side effects (see Renderer.keepScrollback).
// Callers concatenate this into a larger control sequence; an empty
// return value is a no-op there.
func (r *Renderer) clearScrollbackSeq() string {
	if r.keepScrollback {
		return ""
	}
	return SeqClearScrollback
}

// KeepsScrollback reports whether this renderer suppresses the
// scrollback-clear escape (true under VS Code's terminal). Callers
// use it to pick a viewport-safe full repaint (Invalidate) over a
// scrollback-clearing one (Clear) when redrawing overlays.
func (r *Renderer) KeepsScrollback() bool { return r.keepScrollback }

// Invalidate forces a full repaint on the next Draw without clearing the
// whole terminal first. Useful when the cached diff is unreliable but a
// visible full-screen flash would be too distracting.
func (r *Renderer) Invalidate() {
	r.prev = nil
	r.logLines = nil
}

// Draw updates the terminal so that the visible frame ends with the
// given lines (bottom-aligned). cursorRow/cursorCol are offsets within
// the lines slice indicating where to place the terminal cursor; use
// -1 to hide it.
// containsImageEscape reports whether the line carries an inline-image
// escape we must repaint rather than diff against the previous frame.
func containsImageEscape(s string) bool {
	return strings.Contains(s, "\x1b]1337;File=") || strings.Contains(s, "\x1b_G")
}

func imageFootprintBlank(s string) bool {
	s = strings.ReplaceAll(stripANSI(s), imageFootprintSentinel, "")
	s = strings.Trim(strings.TrimSpace(s), "│")
	return strings.TrimSpace(s) == ""
}

// visibleTailHasImage reports whether an inline image or its blank terminal
// footprint intersects the visible tail. Images earlier in retained scrollback
// do not prevent ordinary text reflows from using the stable in-place path.
func visibleTailHasImage(lines []string, viewportTop int) bool {
	if viewportTop < 0 {
		viewportTop = 0
	}
	if viewportTop >= len(lines) {
		return false
	}
	for _, line := range lines[viewportTop:] {
		if containsImageEscape(line) {
			return true
		}
	}
	if viewportTop == 0 || !imageFootprintBlank(lines[viewportTop]) {
		return false
	}
	for idx := viewportTop - 1; idx >= 0; idx-- {
		if containsImageEscape(lines[idx]) {
			return true
		}
		if !imageFootprintBlank(lines[idx]) {
			return false
		}
	}
	return false
}

// paintBackgroundRow applies the optional theme background to a single
// already-truncated terminal row. It pads with spaces to cols so the
// background reaches the right edge, and re-applies the background
// after full SGR resets inside the row so local styling does not punch
// transparent holes through the global tint.
func paintBackgroundRow(line string, cols int, background string) string {
	if background == "" || cols <= 0 || containsImageEscape(line) {
		return line
	}
	line = strings.ReplaceAll(line, reset, reset+background)
	if w := visibleWidth(line); w < cols {
		line += strings.Repeat(" ", cols-w)
	}
	return background + line + reset
}

// truncateToWidth clips s so its on-screen width doesn't exceed cols
// cells, preserving ANSI CSI escape sequences (which don't consume
// cells). Lines carrying an inline-image escape are returned as-is
// since we can't measure their painted size.
//
// Fast path: a byte-length <= cols is a conservative upper bound
// guaranteeing the cell width is also <= cols, so we skip all the
// rune-width math. That covers the vast majority of lines in a
// transcript (narrow terminals wrap early; wide ones leave headroom).
func truncateToWidth(s string, cols int) string {
	if cols <= 0 || containsImageEscape(s) {
		return s
	}
	if len(s) <= cols {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))
	seen := 0
	osc8Active := false
	runes := []rune(s)
	for i := 0; i < len(runes); {
		r := runes[i]
		// CSI escape sequence (ESC [ ... final): zero-width.
		if r == 0x1b && i+1 < len(runes) && runes[i+1] == '[' {
			out.WriteRune(r)
			out.WriteRune(runes[i+1])
			i += 2
			for i < len(runes) {
				c := runes[i]
				out.WriteRune(c)
				i++
				if c >= 0x40 && c <= 0x7e {
					break
				}
			}
			continue
		}
		// OSC string escape (BEL or ST terminated): zero-width and atomic.
		if r == 0x1b && i+1 < len(runes) && runes[i+1] == ']' {
			start := i
			i += 2
			for i < len(runes) {
				c := runes[i]
				i++
				if c == 0x07 {
					break
				}
				if c == 0x1b && i < len(runes) && runes[i] == '\\' {
					i++
					break
				}
			}
			osc8Closer := start+5 < i && runes[start+2] == '8' && runes[start+3] == ';' && runes[start+4] == ';' &&
				((runes[start+5] == 0x07 && i == start+6) ||
					(start+6 < i && runes[start+5] == 0x1b && runes[start+6] == '\\' && i == start+7))
			if seen < cols || osc8Closer {
				for _, c := range runes[start:i] {
					out.WriteRune(c)
				}
				if runes[start+2] == '8' {
					osc8Active = !osc8Closer
				}
			}
			continue
		}
		rw := runewidthRune(r)
		if seen+rw > cols {
			if osc8Active {
				// The remaining text is omitted, so close a hyperlink
				// whose opener was already emitted before the boundary.
				out.WriteString("\x1b]8;;\x07")
			}
			// Flush any trailing ANSI escapes (resets, erase-to-EOL)
			// so background colors and cleanup sequences survive.
			for i < len(runes) {
				if runes[i] == 0x1b && i+1 < len(runes) && runes[i+1] == '[' {
					out.WriteRune(runes[i])
					out.WriteRune(runes[i+1])
					i += 2
					for i < len(runes) {
						c := runes[i]
						out.WriteRune(c)
						i++
						if c >= 0x40 && c <= 0x7e {
							break
						}
					}
				} else if runes[i] == 0x1b && i+1 < len(runes) && runes[i+1] == ']' {
					// Do not append a new OSC sequence after the visible
					// boundary: an OSC 8 opener would leak hyperlink state
					// into the next terminal row. A matching OSC 8 closer is
					// retained so a link opened before the boundary is closed.
					keep := i+5 < len(runes) && runes[i+2] == '8' && runes[i+3] == ';' && runes[i+4] == ';' &&
						(runes[i+5] == 0x07 || (i+6 < len(runes) && runes[i+5] == 0x1b && runes[i+6] == '\\'))
					start := i
					i += 2
					for i < len(runes) {
						c := runes[i]
						i++
						if c == 0x07 {
							break
						}
						if c == 0x1b && i < len(runes) && runes[i] == '\\' {
							i++
							break
						}
					}
					if keep {
						for _, c := range runes[start:i] {
							out.WriteRune(c)
						}
					}
				} else {
					break
				}
			}
			break
		}
		out.WriteRune(r)
		seen += rw
		i++
	}
	return out.String()
}

func (r *Renderer) Draw(lines []string, cursorRow, cursorCol int) {
	if r.cols == 0 || r.rows == 0 {
		return
	}
	// Bottom-align: only the last r.rows lines are visible.
	visible := lines
	if len(visible) > r.rows {
		visible = visible[len(visible)-r.rows:]
		cursorRow -= len(lines) - len(visible)
	}
	// Pad to r.rows with empty lines at the top. Every line is also
	// hard-truncated to cols so the terminal never soft-wraps our output
	// (which would push the status bar out of its row).
	frame := make([]string, r.rows)
	top := r.rows - len(visible)
	for i := 0; i < top; i++ {
		frame[i] = ""
	}
	for i, line := range visible {
		frame[top+i] = paintBackgroundRow(truncateToWidth(line, r.cols), r.cols, r.backgroundStyle)
	}
	if r.theme.Background != nil {
		for i := 0; i < top; i++ {
			frame[i] = paintBackgroundRow("", r.cols, r.backgroundStyle)
		}
	}

	var w strings.Builder
	w.WriteString(SeqSynchronizedOn)
	w.WriteString(SeqHideCursor)

	// When inline images are in play we always full-repaint (clear
	// screen first, then rewrite every row). Terminals manage image
	// pixels in a layer we cannot diff against, so the per-line cache
	// is unreliable. Inline images are opt-in via ZUT_INLINE_IMAGES;
	// the common code path below is the fast cached diff.
	curHasImage := false
	curHasKittyImage := false
	for _, l := range frame {
		if containsImageEscape(l) {
			curHasImage = true
			if strings.Contains(l, "\x1b_G") {
				curHasKittyImage = true
			}
		}
	}
	forceAll := curHasImage || r.prevHadImage
	if forceAll {
		// No-home variant: the per-row MoveTo(i+1, 1) writes in the
		// loop below position the cursor for every painted row, so
		// the embedded \x1b[H would only serve to make VS Code snap
		// its scrollbar to the top of the viewport on every image or
		// selection-highlight frame.
		w.WriteString(SeqClearScreenNoHome)
		if curHasKittyImage || r.prevHadKittyImage {
			// Delete previously placed kitty images once per frame, before
			// rewriting all rows. This also removes a former foreground image
			// when the replacement frame contains no Kitty image escape.
			w.WriteString(SeqDeleteKittyImages)
		}
	}

	// Detect selection highlights: if the current OR previous frame
	// has selection-background rows, force full repaint. VS Code's
	// terminal doesn't reliably clear background colors on row
	// overwrites, leaving ghost highlights behind.
	hasSelection := false
	if r.theme.Background == nil {
		selectionBG := r.theme.bgPrefix(r.theme.SelectionBG)
		for _, l := range frame {
			if selectionBG != "" && strings.Contains(l, selectionBG) {
				hasSelection = true
				break
			}
		}
		if !hasSelection && r.prev != nil {
			for _, l := range r.prev {
				if selectionBG != "" && strings.Contains(l, selectionBG) {
					hasSelection = true
					break
				}
			}
		}
	}

	full := r.prev == nil || len(r.prev) != r.rows
	for i := 0; i < r.rows; i++ {
		if full || forceAll || hasSelection || r.prev[i] != frame[i] {
			w.WriteString(MoveTo(i+1, 1))
			w.WriteString("\x1b[0m") // reset all attributes first
			w.WriteString(SeqClearLine)
			w.WriteString(frame[i])
		}
	}

	if cursorRow >= 0 {
		absRow := top + cursorRow + 1
		absCol := cursorCol + 1
		if absRow >= 1 && absRow <= r.rows {
			w.WriteString(MoveTo(absRow, absCol))
			w.WriteString(SeqShowCursor)
		}
	}
	w.WriteString(SeqSynchronizedOff)

	_, _ = io.WriteString(r.out, w.String())

	r.prev = frame
	r.prevHadImage = curHasImage
	r.prevHadKittyImage = curHasKittyImage
	r.cursorRow = cursorRow
	r.cursorCol = cursorCol
}

// DrawFloating paints a complete, dimmed live background and then an opaque
// floating pane at absolute coordinates. The background is deliberately
// invalidated on every call: the pane is not represented in Draw's frame cache,
// so repainting it first prevents stale foreground cells when either layer
// changes or the terminal is resized.
func (r *Renderer) DrawFloating(pane FloatingPaneFrame, cursorRow, cursorCol int) {
	if r.cols == 0 || r.rows == 0 {
		return
	}
	r.Invalidate()
	r.Draw(pane.Background, -1, 0)

	paneHasImage := false
	paneHasKittyImage := false
	for _, line := range pane.Lines {
		if containsImageEscape(line) {
			paneHasImage = true
			if strings.Contains(line, "\x1b_G") {
				paneHasKittyImage = true
			}
		}
	}

	var w strings.Builder
	w.WriteString(SeqSynchronizedOn)
	w.WriteString(SeqHideCursor)
	for row, line := range pane.Lines {
		y := pane.Rect.Y + row
		if y < 0 || y >= r.rows {
			continue
		}
		w.WriteString(MoveTo(y+1, pane.Rect.X+1))
		w.WriteString("\x1b[0m")
		w.WriteString(paintBackgroundRow(truncateToWidth(line, pane.Rect.Width), pane.Rect.Width, r.backgroundStyle))
	}
	if cursorRow >= 0 && cursorRow < r.rows && cursorCol >= 0 && cursorCol < r.cols {
		w.WriteString(MoveTo(cursorRow+1, cursorCol+1))
		w.WriteString(SeqShowCursor)
	}
	w.WriteString(SeqSynchronizedOff)
	_, _ = io.WriteString(r.out, w.String())
	r.prevHadImage = r.prevHadImage || paneHasImage
	r.prevHadKittyImage = r.prevHadKittyImage || paneHasKittyImage
	// pane.Lines are painted outside Draw's frame cache, so the next normal
	// frame must be fully repainted to remove the foreground cells.
	r.Invalidate()
}

// DrawLog renders zut in the terminal's main screen as normal terminal
// flow rather than a fixed full-screen frame. Chat lines are emitted once
// into the host terminal scrollback; the current bottom block (dialogs,
// slash popup, status, editor) is erased and redrawn in place at the end.
//
// cursorBottomRow/cursorCol are offsets into bottom, not the full frame.
func (r *Renderer) DrawLog(chat, bottom []string, cursorBottomRow, cursorCol int) {
	if r.rightBarActive {
		r.clearRightBarOverlay()
	}
	r.drawLog(chat, bottom, cursorBottomRow, cursorCol, r.cols)
}

// drawLog is DrawLog's owning flow implementation. contentCols may reserve
// terminal columns for a separately composited viewport overlay.
func (r *Renderer) drawLog(chat, bottom []string, cursorBottomRow, cursorCol, contentCols int) bool {
	if r.cols == 0 || r.rows == 0 {
		return false
	}
	if contentCols <= 0 || contentCols > r.cols {
		contentCols = r.cols
	}
	if len(bottom) == 0 {
		bottom = []string{""}
	}
	if r.logRawFrameUnchanged(chat, bottom, cursorBottomRow, cursorCol, contentCols) {
		return false
	}
	chatFrame := make([]string, len(chat))
	for i, line := range chat {
		chatFrame[i] = paintBackgroundRow(truncateToWidth(line, contentCols), contentCols, r.backgroundStyle)
	}
	bottomFrame := make([]string, len(bottom))
	for i, line := range bottom {
		bottomFrame[i] = paintBackgroundRow(truncateToWidth(line, contentCols), contentCols, r.backgroundStyle)
	}

	// Always reserve one real row below the editor/status band. This is
	// renderer-owned (not a best-effort trailing blank in the caller's
	// bottom block), so the logical-buffer diff keeps it visible and cursor
	// placement remains relative to the editor itself.
	const bottomMarginRows = 1
	lines := make([]string, 0, len(chatFrame)+len(bottomFrame)+bottomMarginRows)
	lines = append(lines, chatFrame...)
	lines = append(lines, bottomFrame...)
	for range bottomMarginRows {
		lines = append(lines, paintBackgroundRow("", contentCols, r.backgroundStyle))
	}
	// In main-screen flow mode zut normally emits only its logical
	// content rows and leaves the rest of the terminal viewport alone.
	// When a theme background is configured, fill that otherwise-idle
	// space with painted blank rows so the full window is tinted while
	// keeping the scrollback-oriented renderer model unchanged for the
	// default transparent case.
	if r.theme.Background != nil {
		for len(lines) < r.rows {
			lines = append(lines, paintBackgroundRow("", contentCols, r.backgroundStyle))
		}
	}
	if len(lines) == 0 {
		lines = []string{""}
	}

	cursorTargetRow := -1
	if cursorBottomRow >= 0 && cursorBottomRow < len(bottomFrame) {
		cursorTargetRow = len(chatFrame) + cursorBottomRow
	}

	// Idle no-op fast path. When the buffer AND the cursor position
	// haven't changed since the last DrawLog, emit nothing. The
	// alternative — always writing SeqHideCursor + cursor-position +
	// SeqShowCursor — resets the terminal's cursor blink timer on
	// every tick. At our 120ms animation cadence that means the
	// caret in an idle dialog editor (e.g. an open subagent transcript
	// for an agent that's currently idle) appears as a solid block
	// that never blinks, because we keep "showing" it before the
	// terminal can blink it off. Bailing out here lets the OS run
	// its blink cycle.
	if r.logInit && r.logRawContentCols == contentCols &&
		cursorBottomRow == r.cursorRow && cursorCol == r.cursorCol && sameLines(lines, r.logLines) {
		return false
	}

	var w strings.Builder
	w.WriteString(SeqSynchronizedOn)
	w.WriteString(SeqHideCursor)

	writeFull := func(clear, purgeScrollback bool) {
		if clear {
			w.WriteString(SeqDeleteKittyImages)
			if r.keepScrollback {
				// VS Code's xterm.js scrolls the visible content up into
				// scrollback on \x1b[2J, which duplicates the frame (the
				// old paint stays above the new one). Home to the
				// viewport top and erase-to-end (\x1b[0J) instead: that
				// clears the visible screen in place without pushing the
				// previous frame into scrollback. We still cannot drop
				// existing scrollback (\x1b[3J snaps the viewport there),
				// but a full repaint no longer stacks a fresh copy below
				// the old one.
				w.WriteString(SeqCursorHome)
				w.WriteString(SeqClearToEnd)
			} else {
				w.WriteString(SeqClearScreenNoHome)
				// Purging scrollback invalidates native terminal selections.
				// It is safe on the first paint because no zut output can be
				// selected yet, but implicit recovery repaints happen while an
				// agent is streaming and must preserve the user's selection.
				if purgeScrollback {
					w.WriteString(r.clearScrollbackSeq())
				}
				w.WriteString(MoveTo(1, 1))
			}
		}
		for idx, line := range lines {
			if idx > 0 {
				w.WriteString("\r\n")
			}
			w.WriteString("\x1b[0m")
			w.WriteString(SeqClearLine)
			w.WriteString(line)
		}
		r.logHardwareRow = len(lines) - 1
		r.logViewportTop = len(lines) - r.rows
		if r.logViewportTop < 0 {
			r.logViewportTop = 0
		}
	}

	moveToLogicalRow := func(targetRow int) {
		if targetRow < 0 {
			targetRow = 0
		}
		if targetRow >= len(lines) {
			targetRow = len(lines) - 1
		}
		viewportBottom := r.logViewportTop + r.rows - 1
		if targetRow > viewportBottom {
			currentScreenRow := r.logHardwareRow - r.logViewportTop
			if currentScreenRow < 0 {
				currentScreenRow = 0
			}
			if currentScreenRow >= r.rows {
				currentScreenRow = r.rows - 1
			}
			moveToBottom := r.rows - 1 - currentScreenRow
			if moveToBottom > 0 {
				w.WriteString("\x1b[" + itoa(moveToBottom) + "B")
			}
			scroll := targetRow - viewportBottom
			for s := 0; s < scroll; s++ {
				w.WriteString("\r\n")
			}
			r.logViewportTop += scroll
			r.logHardwareRow = targetRow
			return
		}
		currentScreenRow := r.logHardwareRow - r.logViewportTop
		targetScreenRow := targetRow - r.logViewportTop
		lineDiff := targetScreenRow - currentScreenRow
		if lineDiff > 0 {
			w.WriteString("\x1b[" + itoa(lineDiff) + "B")
		} else if lineDiff < 0 {
			w.WriteString("\x1b[" + itoa(-lineDiff) + "A")
		}
		r.logHardwareRow = targetRow
	}

	positionCursor := func() {
		if cursorTargetRow < 0 || cursorTargetRow >= len(lines) {
			return
		}
		moveToLogicalRow(cursorTargetRow)
		w.WriteString("\r")
		physicalCursorCol := cursorCol
		if physicalCursorCol < 0 {
			physicalCursorCol = 0
		}
		if physicalCursorCol >= contentCols {
			physicalCursorCol = contentCols - 1
		}
		if physicalCursorCol > 0 {
			w.WriteString("\x1b[" + itoa(physicalCursorCol) + "C")
		}
		w.WriteString(SeqShowCursor)
	}

	// Selection-highlight workaround removed: it could mis-invalidate
	// user-bubble padding rows whose colored bg made botHasHL trip,
	// causing the next diff pass to leave those rows visually thinned
	// because the cached entry was the \x00 sentinel rather than the
	// real previous bg-colored row.

	wasInitialized := r.logInit
	full := !wasInitialized || len(r.logLines) == 0
	if full {
		// A cache invalidation may require a full viewport repaint after flow
		// has started, but only the initial paint may discard old scrollback.
		writeFull(true, !wasInitialized)
		r.logInit = true
	} else {
		firstChanged := -1
		lastChanged := -1
		maxLines := len(lines)
		if len(r.logLines) > maxLines {
			maxLines = len(r.logLines)
		}
		for idx := 0; idx < maxLines; idx++ {
			oldLine := ""
			if idx < len(r.logLines) {
				oldLine = r.logLines[idx]
			}
			newLine := ""
			if idx < len(lines) {
				newLine = lines[idx]
			}
			if oldLine != newLine {
				if firstChanged == -1 {
					firstChanged = idx
				}
				lastChanged = idx
			}
		}
		// Buffer grew but the appended rows were empty (or otherwise
		// equal to the implicit "" past the old end). The diff above
		// won't flag those rows, yet the renderer still needs to
		// advance its hardware cursor / viewport tracking past them so
		// the next render starts from the correct position. Treat the
		// extension as changed.
		if len(lines) > len(r.logLines) {
			if firstChanged == -1 {
				firstChanged = len(r.logLines)
			}
			if lastChanged < len(lines)-1 {
				lastChanged = len(lines) - 1
			}
		}

		// Rows that have scrolled above the viewport are immutable terminal
		// history. Replaying the full logical buffer to update one of them
		// duplicates that history, and erasing it first destroys native text
		// selections. Ignore inaccessible changes and patch only the first
		// changed row that is still addressable.
		if firstChanged >= 0 && firstChanged < r.logViewportTop {
			delta := len(chatFrame) - len(r.logChat)
			reflowAbove := false
			if delta != 0 {
				overlapEnd := min(len(chatFrame), len(r.logChat))
				// When the viewport starts in the bottom band there are no chat
				// rows to compare at the old logical coordinates, but changing
				// chat height still shifts the entire visible band.
				reflowAbove = r.logViewportTop >= overlapEnd
				for idx := r.logViewportTop; !reflowAbove && idx < overlapEnd; idx++ {
					if r.logChat[idx] != chatFrame[idx] {
						reflowAbove = true
					}
				}
			}
			if reflowAbove {
				// A text-only logical buffer can be rebased without resetting the
				// terminal viewport. Advance physical rows when it grows, then
				// repaint the visible tail in place for both growth and shrinkage.
				// Cursor-home plus erase-to-end makes several terminals visibly
				// jump on streaming-markdown reflows.
				growth := len(lines) - len(r.logLines)
				newViewportTop := max(0, len(lines)-r.rows)
				hasVisibleImages := visibleTailHasImage(r.logLines, r.logViewportTop) ||
					visibleTailHasImage(lines, newViewportTop)
				if !hasVisibleImages && len(lines) >= r.rows {
					currentScreenRow := r.logHardwareRow - r.logViewportTop
					if currentScreenRow < 0 {
						currentScreenRow = 0
					}
					if currentScreenRow >= r.rows {
						currentScreenRow = r.rows - 1
					}
					if growth > 0 {
						if down := r.rows - 1 - currentScreenRow; down > 0 {
							w.WriteString("\x1b[" + itoa(down) + "B")
						}
						for range growth {
							w.WriteString("\r\n")
						}
						currentScreenRow = r.rows - 1
					}
					if currentScreenRow > 0 {
						w.WriteString("\x1b[" + itoa(currentScreenRow) + "A")
					}
					w.WriteString("\r")
					for idx := newViewportTop; idx < len(lines); idx++ {
						if idx > newViewportTop {
							w.WriteString("\r\n")
						}
						w.WriteString("\x1b[0m")
						w.WriteString(SeqClearLine)
						w.WriteString(lines[idx])
					}
					r.logViewportTop = newViewportTop
					r.logHardwareRow = len(lines) - 1
				} else {
					// Inline-image reflows and buffers shorter than the viewport need
					// the conservative repaint path. Replaying the full transcript
					// would duplicate retained scrollback.
					w.WriteString(SeqDeleteKittyImages + SeqCursorHome + SeqClearToEnd)
					viewportTop := max(0, len(lines)-r.rows)
					start := viewportTop
					if start > 0 && imageFootprintBlank(lines[start]) {
						for idx := start - 1; idx >= 0; idx-- {
							if containsImageEscape(lines[idx]) {
								start = idx
								break
							}
							if !imageFootprintBlank(lines[idx]) {
								break
							}
						}
					}
					for idx := start; idx < len(lines); idx++ {
						if idx > start {
							w.WriteString("\r\n")
						}
						w.WriteString("\x1b[0m")
						w.WriteString(SeqClearLine)
						w.WriteString(lines[idx])
					}
					r.logViewportTop = viewportTop
					r.logHardwareRow = len(lines) - 1
				}
				firstChanged = -1
				lastChanged = -1
			} else {
				firstChanged = -1
				lastChanged = -1
				for idx := r.logViewportTop; idx < maxLines; idx++ {
					oldLine := ""
					if idx < len(r.logLines) {
						oldLine = r.logLines[idx]
					}
					newLine := ""
					if idx < len(lines) {
						newLine = lines[idx]
					}
					if oldLine != newLine {
						if firstChanged == -1 {
							firstChanged = idx
						}
						lastChanged = idx
					}
				}
				// A newly appended blank row compares equal to the implicit empty
				// row past the old slice, but it still has to advance the terminal.
				if firstChanged == -1 && len(lines) > len(r.logLines) && len(r.logLines) >= r.logViewportTop {
					firstChanged = len(r.logLines)
					lastChanged = len(lines) - 1
				}
			}
		}

		if firstChanged == -1 {
			// No addressable content changes; the final cursor positioning below
			// may still move the hardware cursor if the editor cursor changed.
		} else {
			prevViewportTop := r.logViewportTop
			viewportTop := prevViewportTop
			hardwareRow := r.logHardwareRow
			prevViewportBottom := prevViewportTop + r.rows - 1
			appendStart := len(lines) > len(r.logLines) && firstChanged == len(r.logLines) && firstChanged > 0
			moveTarget := firstChanged
			if appendStart {
				moveTarget = firstChanged - 1
			}

			if moveTarget > prevViewportBottom {
				currentScreenRow := hardwareRow - prevViewportTop
				if currentScreenRow < 0 {
					currentScreenRow = 0
				}
				if currentScreenRow >= r.rows {
					currentScreenRow = r.rows - 1
				}
				moveToBottom := r.rows - 1 - currentScreenRow
				if moveToBottom > 0 {
					w.WriteString("\x1b[" + itoa(moveToBottom) + "B")
				}
				scroll := moveTarget - prevViewportBottom
				for s := 0; s < scroll; s++ {
					w.WriteString("\r\n")
				}
				prevViewportTop += scroll
				viewportTop += scroll
				hardwareRow = moveTarget
			}

			currentScreenRow := hardwareRow - prevViewportTop
			targetScreenRow := moveTarget - viewportTop
			lineDiff := targetScreenRow - currentScreenRow
			if lineDiff > 0 {
				w.WriteString("\x1b[" + itoa(lineDiff) + "B")
			} else if lineDiff < 0 {
				w.WriteString("\x1b[" + itoa(-lineDiff) + "A")
			}
			if appendStart {
				w.WriteString("\r\n")
			} else {
				w.WriteString("\r")
			}

			renderEnd := lastChanged
			if renderEnd >= len(lines) {
				renderEnd = len(lines) - 1
			}
			for idx := firstChanged; idx <= renderEnd; idx++ {
				if idx > firstChanged {
					w.WriteString("\r\n")
				}
				w.WriteString("\x1b[0m")
				w.WriteString(SeqClearLine)
				w.WriteString(lines[idx])
			}
			finalRow := renderEnd
			if len(r.logLines) > len(lines) {
				extra := len(r.logLines) - len(lines)
				if extra > r.rows {
					writeFull(true, false)
				} else {
					for e := 0; e < extra; e++ {
						w.WriteString("\x1b[1B")
						w.WriteString("\r")
						w.WriteString("\x1b[0m")
						w.WriteString(SeqClearLine)
						finalRow++
					}
					if extra > 0 {
						w.WriteString("\x1b[" + itoa(extra) + "A")
						finalRow -= extra
					}
				}
			}
			r.logHardwareRow = finalRow
			r.logViewportTop = viewportTop
			if minTop := r.logHardwareRow - r.rows + 1; minTop > r.logViewportTop {
				r.logViewportTop = minTop
			}
			if r.logViewportTop < 0 {
				r.logViewportTop = 0
			}
		}
	}

	positionCursor()
	w.WriteString(SeqSynchronizedOff)
	_, _ = io.WriteString(r.out, w.String())

	r.logChat = append(r.logChat[:0], chatFrame...)
	r.logBottom = append(r.logBottom[:0], bottomFrame...)
	r.logLines = append(r.logLines[:0], lines...)
	r.logRawChat = append(r.logRawChat[:0], chat...)
	r.logRawBottom = append(r.logRawBottom[:0], bottom...)
	r.logRawCols = r.cols
	r.logRawRows = r.rows
	r.logRawContentCols = contentCols
	r.logRawBackground = r.backgroundStyle
	r.logRawHasThemeBG = r.theme.Background != nil
	r.cursorRow = cursorBottomRow
	r.cursorCol = cursorCol
	return true
}

func (r *Renderer) logRawFrameUnchanged(chat, bottom []string, cursorBottomRow, cursorCol, contentCols int) bool {
	return cursorBottomRow == r.cursorRow && cursorCol == r.cursorCol &&
		r.logRawContentUnchanged(chat, bottom, contentCols)
}

func (r *Renderer) logRawContentUnchanged(chat, bottom []string, contentCols int) bool {
	return r.logInit && len(r.logLines) > 0 &&
		r.logRawCols == r.cols && r.logRawRows == r.rows && r.logRawContentCols == contentCols &&
		r.logRawBackground == r.backgroundStyle && r.logRawHasThemeBG == (r.theme.Background != nil) &&
		sameLines(chat, r.logRawChat) && sameLines(bottom, r.logRawBottom)
}

// sameLines reports whether two []string have the exact same
// length and per-row contents. Used by DrawLog's idle no-op fast
// path; cheap enough at our frame rates and far simpler than
// hashing every byte.
func sameLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
