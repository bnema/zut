package tui

import (
	"fmt"
	"strings"
)

// liveCodeHighlightWindow caps how many trailing complete lines of a
// streaming code body are re-highlighted when a new line completes.
const liveCodeHighlightWindow = 400

// liveCodeState caches the highlighted complete lines of one streaming
// tool body. States are immutable once stored: updates install a new
// value, so render snapshots can share the map entries safely.
type liveCodeState struct {
	key        string // which field is streaming, e.g. "content" or "oldText#2"
	theme      uint64
	complete   int      // complete (newline-terminated) line count
	lines      []string // highlighted complete lines
	recomputes int      // number of re-highlights; observed by tests
}

// liveHighlighted returns the highlighted complete lines of a streaming
// code body and its sanitized, unhighlighted partial last line. It only
// re-highlights when the complete-line count changes, and highlights
// through highlightCodeUncached so growing prefixes never enter the shared
// highlight cache. A different key or theme starts a fresh state.
func (v *View) liveHighlighted(id, key, lang, text string) (done []string, partial string) {
	completeText, partial := "", text
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		completeText, partial = text[:i], text[i+1:]
	}
	partial = sanitizeLine(partial, codeTabStop)
	count := strings.Count(text, "\n")

	theme := toolThemeKey(v.Theme)
	prev := v.liveCode[id]
	if prev != nil && prev.key == key && prev.theme == theme && prev.complete == count {
		return prev.lines, partial
	}
	recomputes := 0
	if prev != nil && prev.key == key && prev.theme == theme {
		recomputes = prev.recomputes
	}
	next := &liveCodeState{key: key, theme: theme, complete: count, recomputes: recomputes}
	if count > 0 {
		next.lines = v.highlightLiveLines(strings.Split(completeText, "\n"), lang)
		next.recomputes++
	}
	if v.liveCode == nil {
		v.liveCode = make(map[string]*liveCodeState)
	}
	v.liveCode[id] = next
	return next.lines, partial
}

// highlightLiveLines highlights sanitized source lines. Past
// liveCodeHighlightWindow lines only the trailing window is highlighted;
// earlier rows get the plain tool color. Starting the lexer mid-file is an
// approximation of its state, but those earlier rows never show in the
// live tail window.
func (v *View) highlightLiveLines(lines []string, lang string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		lines[i] = sanitizeLine(l, codeTabStop)
	}
	start := max(0, len(lines)-liveCodeHighlightWindow)
	for i := 0; i < start; i++ {
		out[i] = v.Theme.FGColor(v.Theme.Tool, lines[i])
	}
	window := lines[start:]
	var hl []string
	if lang != "" {
		hl = v.Theme.highlightCodeUncached(strings.Join(window, "\n"), lang)
	}
	if len(hl) != len(window) {
		hl = make([]string, len(window))
		for i, l := range window {
			hl[i] = v.Theme.FGColor(v.Theme.Tool, l)
		}
	}
	copy(out[start:], hl)
	return out
}

// liveCodeRows is the number of code rows in the live write/edit tail
// window. One progress row sits above them.
const liveCodeRows = 12

// liveCodeLine is one source line of a live tail window.
type liveCodeLine struct {
	text   string // highlighted (complete) or sanitized plain (partial)
	sign   byte   // '+' or '-'
	number int    // 0 leaves the gutter number blank
}

// renderLiveWrite renders a streaming write as a progress row plus the
// last liveCodeRows visual rows of the content as numbered added rows.
func (v *View) renderLiveWrite(tc ToolCallView, content string, width int) []string {
	done, partial := v.liveHighlighted(tc.ID, "content", LanguageFromPath(tc.LivePath), content)
	lines := make([]liveCodeLine, 0, len(done)+1)
	for i, l := range done {
		lines = append(lines, liveCodeLine{text: l, sign: '+', number: i + 1})
	}
	if partial != "" {
		lines = append(lines, liveCodeLine{text: v.Theme.FGColor(v.Theme.Tool, partial), sign: '+', number: len(done) + 1})
	}
	progress := fmt.Sprintf("writing · %d lines", len(lines))
	return v.liveCodeWindow(progress, lines, width)
}

// renderLiveEdit renders the streaming edit as a progress row plus the tail
// of its oldText as removed rows followed by its newText as added rows.
func (v *View) renderLiveEdit(tc ToolCallView, edit LiveEdit, width int) []string {
	lang := LanguageFromPath(tc.LivePath)
	collect := func(suffix, text string, sign byte) []liveCodeLine {
		if text == "" {
			return nil
		}
		color := v.Theme.Tool
		if sign == '-' {
			color = v.Theme.Error
		}
		key := fmt.Sprintf("%s#%d", suffix, edit.Idx)
		done, partial := v.liveHighlighted(tc.ID+liveCodeIDSep+suffix, key, lang, text)
		out := make([]liveCodeLine, 0, len(done)+1)
		for _, l := range done {
			out = append(out, liveCodeLine{text: l, sign: sign})
		}
		if partial != "" {
			out = append(out, liveCodeLine{text: v.Theme.FGColor(color, partial), sign: sign})
		}
		return out
	}
	removed := collect("oldText", edit.OldText, '-')
	added := collect("newText", edit.NewText, '+')
	progress := fmt.Sprintf("edit %d · -%d +%d", edit.Idx, len(removed), len(added))
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
		rows := v.liveCodeRow(lines[i], rowCells)
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

// liveCodeRow renders one source line with the 7-cell diff gutter and the
// add/remove tint, wrapping long lines onto blank-gutter rows.
func (v *View) liveCodeRow(l liveCodeLine, rowCells int) []string {
	color, tint := v.Theme.Tool, v.Theme.DiffAddBG
	if l.sign == '-' {
		color, tint = v.Theme.Error, v.Theme.DiffDelBG
	}
	gutter := fmt.Sprintf("     %c ", l.sign)
	if l.number > 0 {
		gutter = fmt.Sprintf("%4d %c ", l.number, l.sign)
	}
	blank := strings.Repeat(" ", visibleWidth(gutter))
	chunks := wrapANSILine(l.text, max(1, rowCells-visibleWidth(gutter)))
	if len(chunks) == 0 {
		chunks = []string{""}
	}
	rows := make([]string, len(chunks))
	for i, chunk := range chunks {
		g := v.Theme.FGColor(color, gutter)
		if i > 0 {
			g = blank
		}
		rows[i] = v.Theme.tintRow(g+chunk, tint, rowCells)
	}
	return rows
}
