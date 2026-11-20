package tui

import "strings"

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
