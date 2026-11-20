package tui

import (
	"fmt"
	"strings"
)

// liveCodeHighlightWindow caps how many trailing complete lines of a
// streaming code body are highlighted and kept when a new line completes.
// It must be at least liveCodeRows.
const liveCodeHighlightWindow = 64

// liveCodeState caches the highlighted trailing complete lines of one
// streaming tool body. States are immutable once stored: updates install a
// new value, so render snapshots can share the map entries safely.
type liveCodeState struct {
	key         string // which field is streaming, e.g. "content" or "oldText#2"
	theme       uint64
	complete    int      // complete (newline-terminated) line count
	completeLen int      // byte length of the complete text; streams are append-only
	lastLine    string   // raw last complete line, catches same-length rewrites
	tail        []string // highlighted last min(complete, window) complete lines
	recomputes  int      // number of re-highlights; observed by tests
}

// liveHighlighted returns the highlighted trailing complete lines of a
// streaming code body (at most liveCodeHighlightWindow), the total complete
// line count, and the sanitized, unhighlighted partial last line. It only
// re-highlights when the complete text changes, and highlights through
// highlightCodeUncached so growing prefixes never enter the shared highlight
// cache. A different key or theme starts a fresh state.
func (v *View) liveHighlighted(id, key, lang, text string) (tail []string, complete int, partial string) {
	completeText, partial := "", text
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		completeText, partial = text[:i], text[i+1:]
	}
	partial = sanitizeLine(partial, codeTabStop)
	count := strings.Count(text, "\n")
	lastLine := completeText
	if i := strings.LastIndexByte(completeText, '\n'); i >= 0 {
		lastLine = completeText[i+1:]
	}

	theme := toolThemeKey(v.Theme)
	prev := v.liveCode[id]
	if prev != nil && prev.key == key && prev.theme == theme && prev.complete == count && prev.completeLen == len(completeText) && prev.lastLine == lastLine {
		return prev.tail, prev.complete, partial
	}
	recomputes := 0
	if prev != nil && prev.key == key && prev.theme == theme {
		recomputes = prev.recomputes
	}
	next := &liveCodeState{key: key, theme: theme, complete: count, completeLen: len(completeText), lastLine: lastLine, recomputes: recomputes}
	if count > 0 {
		next.tail = v.highlightLiveLines(liveTailLines(completeText, min(count, liveCodeHighlightWindow)), lang)
		next.recomputes++
	}
	if v.liveCode == nil {
		v.liveCode = make(map[string]*liveCodeState)
	}
	v.liveCode[id] = next
	return next.tail, next.complete, partial
}

// liveTailLines returns the last n lines of text without splitting the
// whole body.
func liveTailLines(text string, n int) []string {
	end := len(text)
	for ; n > 0; n-- {
		j := strings.LastIndexByte(text[:end], '\n')
		if j < 0 {
			return strings.Split(text, "\n")
		}
		if n == 1 {
			return strings.Split(text[j+1:], "\n")
		}
		end = j
	}
	return nil
}

// highlightLiveLines sanitizes and highlights source lines. Starting the
// lexer mid-file approximates its state; only the visible tail matters.
func (v *View) highlightLiveLines(lines []string, lang string) []string {
	for i, l := range lines {
		lines[i] = sanitizeLine(l, codeTabStop)
	}
	return v.highlightOrPlain(lines, lang, v.Theme.highlightCodeUncached)
}

// highlightOrPlain highlights lines as one source and aligns the result to
// len(lines): highlighters trim trailing blank lines, so missing rows are
// padded with "" and extra rows dropped. Without a language every line gets
// the plain tool color.
func (v *View) highlightOrPlain(lines []string, lang string, hl func(src, lang string) []string) []string {
	if lang == "" {
		out := make([]string, len(lines))
		for i, l := range lines {
			out[i] = v.Theme.FGColor(v.Theme.Tool, l)
		}
		return out
	}
	out := hl(strings.Join(lines, "\n"), lang)
	for len(out) < len(lines) {
		out = append(out, "")
	}
	return out[:len(lines)]
}

// liveCodeRows is the number of code rows in the live write/edit tail
// window. One progress row sits above them.
const liveCodeRows = 12

// liveCodeLine is one added or removed source line of a code-change body.
type liveCodeLine struct {
	text   string // highlighted (complete) or sanitized plain (partial)
	sign   byte   // '+' or '-'
	number int    // 0 leaves the gutter number blank
}

// renderLiveWrite renders a streaming write as a progress row plus the
// last liveCodeRows visual rows of the content as numbered added rows.
func (v *View) renderLiveWrite(tc ToolCallView, content string, width int) []string {
	tail, complete, partial := v.liveHighlighted(tc.ID, "content", LanguageFromPath(tc.LivePath), content)
	lines := v.liveTail(tail, complete, partial, '+', true)
	total := complete
	if partial != "" {
		total++
	}
	progress := fmt.Sprintf("writing · %d lines", total)
	return v.liveCodeWindow(progress, lines, width)
}

// liveTail builds liveCodeLines for at most the last liveCodeRows source
// lines of a body: the highlighted complete tail plus the partial line.
// numbered sets 1-based line numbers from the complete line count.
func (v *View) liveTail(tail []string, complete int, partial string, sign byte, numbered bool) []liveCodeLine {
	color := v.Theme.Tool
	if sign == '-' {
		color = v.Theme.Error
	}
	keep := liveCodeRows
	if partial != "" {
		keep--
	}
	if len(tail) > keep {
		tail = tail[len(tail)-keep:]
	}
	first := complete - len(tail) + 1
	out := make([]liveCodeLine, 0, len(tail)+1)
	for i, l := range tail {
		line := liveCodeLine{text: l, sign: sign}
		if numbered {
			line.number = first + i
		}
		out = append(out, line)
	}
	if partial != "" {
		line := liveCodeLine{text: v.Theme.FGColor(color, partial), sign: sign}
		if numbered {
			line.number = complete + 1
		}
		out = append(out, line)
	}
	return out
}

// renderLiveEdit renders the streaming edit as a progress row plus the tail
// of its oldText as removed rows followed by its newText as added rows.
func (v *View) renderLiveEdit(tc ToolCallView, edit LiveEdit, width int) []string {
	lang := LanguageFromPath(tc.LivePath)
	collect := func(suffix, text string, sign byte) ([]liveCodeLine, int) {
		if text == "" {
			return nil, 0
		}
		key := fmt.Sprintf("%s#%d", suffix, edit.Idx)
		tail, complete, partial := v.liveHighlighted(tc.ID+liveCodeIDSep+suffix, key, lang, text)
		total := complete
		if partial != "" {
			total++
		}
		return v.liveTail(tail, complete, partial, sign, false), total
	}
	removed, nRemoved := collect("oldText", edit.OldText, '-')
	added, nAdded := collect("newText", edit.NewText, '+')
	progress := fmt.Sprintf("edit %d · -%d +%d", edit.Idx, nRemoved, nAdded)
	return v.liveCodeWindow(progress, append(removed, added...), width)
}

// liveCodeIDSep separates a call id from a per-field suffix in liveCode
// keys; pruning compares only the call id part.
const liveCodeIDSep = "\x00"

// liveCodeCallID returns the tool-call id a liveCode key belongs to.
func liveCodeCallID(key string) string {
	if i := strings.Index(key, liveCodeIDSep); i >= 0 {
		return key[:i]
	}
	return key
}

// liveCodeWindow builds exactly liveCodeRows+1 rows: a muted progress row,
// then the last liveCodeRows visual rows of lines (after wrapping), padded
// with blank rows. Rows are built from the end so long bodies only wrap
// what is visible.
func (v *View) liveCodeWindow(progress string, lines []liveCodeLine, width int) []string {
	const indent = "    "
	rowCells := width - len(indent)
	var tail []string
	for i := len(lines) - 1; i >= 0 && len(tail) < liveCodeRows; i-- {
		rows := v.codeChangeRow(lines[i], rowCells)
		for j := len(rows) - 1; j >= 0 && len(tail) < liveCodeRows; j-- {
			tail = append(tail, indent+rows[j])
		}
	}
	out := make([]string, 0, liveCodeRows+1)
	out = append(out, indent+v.Theme.FGColor(v.Theme.Muted, progress))
	for i := len(tail) - 1; i >= 0; i-- {
		out = append(out, tail[i])
	}
	for len(out) < liveCodeRows+1 {
		out = append(out, "")
	}
	return out
}

// codeChangeRow renders one added or removed source line with the 7-cell
// diff gutter and the add/remove tint, wrapping long lines onto
// blank-gutter rows. Live write/edit windows and written-file results
// share it.
func (v *View) codeChangeRow(l liveCodeLine, rowCells int) []string {
	color, tint := v.Theme.Tool, v.Theme.DiffAddBG
	if l.sign == '-' {
		color, tint = v.Theme.Error, v.Theme.DiffDelBG
	}
	gutter := fmt.Sprintf("     %c ", l.sign)
	if l.number > 0 {
		gutter = fmt.Sprintf("%4d %c ", l.number, l.sign)
	}
	gutterWidth := visibleWidth(gutter)
	first := v.Theme.FGColor(color, gutter)
	chunks := wrapANSILine(l.text, max(1, rowCells-gutterWidth))
	if len(chunks) == 0 {
		chunks = []string{""}
	}
	rows := make([]string, len(chunks))
	blank := ""
	for i, chunk := range chunks {
		g := first
		if i > 0 {
			if blank == "" {
				blank = strings.Repeat(" ", gutterWidth)
			}
			g = blank
		}
		rows[i] = v.Theme.tintRow(g+chunk, tint, rowCells)
	}
	return rows
}
