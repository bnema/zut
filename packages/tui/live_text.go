package tui

import "strings"

// liveTextCache keeps the rendered rows of the finished blocks of a
// streaming assistant reply. Streaming text only grows, so each frame
// renders just the unfinished tail instead of the whole reply.
type liveTextCache struct {
	width int
	theme uint64
	// src is the source prefix already rendered; it always ends right
	// after a blank line outside a code fence.
	src  string
	rows []string
}

// renderAssistantText renders assistant prose as a finished transcript
// message: markdown, wrapped and indented rows. Live text must render
// identically, otherwise rows already pushed into terminal scrollback would
// no longer match the final message.
func renderAssistantText(text string, th Theme, width int) []string {
	const indent = "  "
	inner := assistantBodyWidth(width - len(indent))
	var out []string
	for _, l := range strings.Split(RenderMarkdown(strings.TrimLeft(text, "\n"), th, inner), "\n") {
		for _, w := range wrapANSILine(l, inner) {
			out = append(out, indent+w)
		}
	}
	return out
}

// stableMarkdownPrefix returns the byte length of the longest prefix of src
// that ends after a blank line outside a code fence. Markdown state never
// crosses that boundary, so the prefix renders identically on its own.
func stableMarkdownPrefix(src string) int {
	cut := 0
	inFence := false
	prevBlank := false
	for pos := 0; pos < len(src); {
		end := strings.IndexByte(src[pos:], '\n')
		if end < 0 {
			// The last line is still being typed.
			break
		}
		line := src[pos : pos+end]
		pos += end + 1
		if isMarkdownFence(line) {
			inFence = !inFence
			prevBlank = false
			continue
		}
		blank := !inFence && strings.TrimSpace(line) == ""
		if blank && !prevBlank {
			cut = pos
		}
		prevBlank = blank
	}
	return cut
}

// liveTextRows renders the streaming reply, reusing cached rows for its
// finished blocks. The result is identical to renderAssistantText(text).
func (v *View) liveTextRows(text string, width int) []string {
	text = strings.TrimLeft(text, "\n")
	const indent = "  "
	inner := assistantBodyWidth(width - len(indent))
	c := &v.liveText
	themeKey := toolThemeKey(v.Theme)
	if c.width != width || c.theme != themeKey || !strings.HasPrefix(text, c.src) {
		*c = liveTextCache{width: width, theme: themeKey}
	}
	if cut := stableMarkdownPrefix(text); cut > len(c.src) {
		add := text[len(c.src):cut]
		raw := renderMarkdownRaw(strings.Split(strings.TrimSuffix(add, "\n"), "\n"), v.Theme, inner)
		for _, l := range strings.Split(strings.TrimSuffix(raw, "\n"), "\n") {
			for _, w := range wrapANSILine(l, inner) {
				c.rows = append(c.rows, indent+w)
			}
		}
		c.src = text[:cut]
	}
	// RenderMarkdown trims trailing newlines from the whole reply. When the
	// tail has content, that trim only affects the tail; otherwise it also
	// drops the cached block's trailing empty rows.
	tail := strings.TrimRight(renderMarkdownRaw(strings.Split(text[len(c.src):], "\n"), v.Theme, inner), "\n")
	if tail == "" {
		rows := c.rows
		for len(rows) > 1 && rows[len(rows)-1] == indent {
			rows = rows[:len(rows)-1]
		}
		if len(rows) == 0 {
			return []string{indent}
		}
		return append([]string(nil), rows...)
	}
	rows := append(make([]string, 0, len(c.rows)+8), c.rows...)
	for _, l := range strings.Split(tail, "\n") {
		for _, w := range wrapANSILine(l, inner) {
			rows = append(rows, indent+w)
		}
	}
	return rows
}
