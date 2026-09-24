package tui

import "strings"

// assistantIndent is the left indent of assistant prose rows.
const assistantIndent = "  "

// liveTextCache keeps the rendered rows of the finished blocks of a
// streaming assistant reply. Streaming text only grows, so each frame
// renders just the unfinished tail instead of the whole reply.
type liveTextCache struct {
	width int
	theme uint64
	// src is the source prefix already rendered; it always ends at a
	// renderMarkdownRaw block boundary.
	src  string
	rows []string
}

// appendAssistantRows wraps rendered markdown rows to inner and indents them.
func appendAssistantRows(dst []string, rendered string, inner int) []string {
	for _, l := range strings.Split(rendered, "\n") {
		for _, w := range wrapANSILine(l, inner) {
			dst = append(dst, assistantIndent+w)
		}
	}
	return dst
}

// renderAssistantText renders assistant prose as a finished transcript
// message. Live text must render identically, otherwise rows already pushed
// into terminal scrollback would no longer match the final message.
func renderAssistantText(text string, th Theme, width int) []string {
	inner := assistantBodyWidth(width - len(assistantIndent))
	return appendAssistantRows(nil, RenderMarkdown(strings.TrimLeft(text, "\n"), th, inner), inner)
}

// liveTextRows renders the streaming reply, reusing cached rows for its
// finished blocks. The result is identical to renderAssistantText(text).
func (v *View) liveTextRows(text string, width int) []string {
	text = strings.TrimLeft(text, "\n")
	inner := assistantBodyWidth(width - len(assistantIndent))
	c := &v.liveText
	themeKey := toolThemeKey(v.Theme)
	if c.width != width || c.theme != themeKey || !strings.HasPrefix(text, c.src) {
		*c = liveTextCache{width: width, theme: themeKey}
	}

	// Render only the uncached tail. The renderer reports where finished
	// blocks end, so the cache never has to re-parse markdown on its own.
	lines := strings.Split(text[len(c.src):], "\n")
	cutLine, cutOut := 0, 0
	raw := renderMarkdownRaw(lines, v.Theme, inner, func(line, outLen int) {
		cutLine, cutOut = line, outLen
	})
	if cutLine > 0 {
		c.rows = appendAssistantRows(c.rows, strings.TrimSuffix(raw[:cutOut], "\n"), inner)
		n := 0
		for _, l := range lines[:cutLine] {
			n += len(l) + 1
		}
		c.src = text[:len(c.src)+n]
		raw = raw[cutOut:]
	}

	// RenderMarkdown trims trailing newlines from the whole reply. When the
	// tail has content, that trim only affects the tail; otherwise it also
	// drops the cached block's trailing empty rows.
	tail := strings.TrimRight(raw, "\n")
	if tail == "" {
		rows := c.rows
		for len(rows) > 1 && rows[len(rows)-1] == assistantIndent {
			rows = rows[:len(rows)-1]
		}
		if len(rows) == 0 {
			return []string{assistantIndent}
		}
		return append([]string(nil), rows...)
	}
	return appendAssistantRows(append(make([]string, 0, len(c.rows)+8), c.rows...), tail, inner)
}
