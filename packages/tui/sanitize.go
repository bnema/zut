package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

// Tab stops used when expanding tabs. Code uses 4 columns so Go and
// Makefiles stay compact; shell output keeps the terminal's 8 so tables
// printed by commands such as ls or column stay aligned.
const (
	codeTabStop     = 4
	terminalTabStop = 8
)

// Text from models, tools, and paste is untrusted terminal input. Layout
// code measures rows with visibleWidth and pads box edges from that
// measurement, so every such string must pass through sanitizeText or
// sanitizeLine before layout. Tabs, carriage returns, escape sequences, and
// other controls move the real cursor differently from what visibleWidth
// counts, which misplaces box edges and leaves stale cells on screen.

// splitLines splits s into rows, treating CRLF and lone CR as line breaks
// so carriage-return progress output becomes separate rows.
func splitLines(s string) []string {
	if strings.IndexByte(s, '\r') >= 0 {
		s = strings.ReplaceAll(s, "\r\n", "\n")
		s = strings.ReplaceAll(s, "\r", "\n")
	}
	return strings.Split(s, "\n")
}

// sanitizeText returns s with every row passed through sanitizeLine.
// Line breaks (LF, CRLF, CR) are kept as "\n".
func sanitizeText(s string, tabStop int) string {
	if isPlainText(s, true) {
		return s
	}
	lines := splitLines(s)
	for i, l := range lines {
		lines[i] = sanitizeLine(l, tabStop)
	}
	return strings.Join(lines, "\n")
}

// sanitizeLine makes one row safe to measure and draw: tabs expand to
// spaces at tabStop columns, line breaks become spaces, and escape
// sequences, other control characters, and invalid UTF-8 are dropped.
// Styling is applied by the renderer afterwards, so no incoming escape
// sequence is kept.
func sanitizeLine(s string, tabStop int) string {
	if isPlainText(s, false) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	col := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i = skipEscapeSequence(s, i)
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == '\t':
			n := tabStop - col%tabStop
			b.WriteString(strings.Repeat(" ", n))
			col += n
		case r == '\n' || r == '\r':
			b.WriteByte(' ')
			col++
		case r == utf8.RuneError && size == 1, r < 0x20, r >= 0x7f && r < 0xa0:
			// Invalid byte, C0/C1 control, or DEL: not printable.
		case unicode.Is(unicode.Cf, r):
			// Invisible format characters (bidi marks, joiners, prepended
			// marks). Terminals disagree on how they join neighbouring
			// cells, so their drawn width cannot be predicted.
		default:
			b.WriteRune(r)
			col += runewidth.RuneWidth(r)
		}
	}
	return b.String()
}

// isPlainText reports whether s is printable ASCII (plus LF when
// allowNewline), the common case that needs no rewriting.
func isPlainText(s string, allowNewline bool) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' && allowNewline {
			continue
		}
		if c < 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}

// skipEscapeSequence returns the index just past the escape sequence
// starting at s[i].
func skipEscapeSequence(s string, i int) int {
	if i >= len(s) || s[i] != 0x1b {
		return i + 1
	}
	if i+1 >= len(s) {
		return len(s)
	}
	switch s[i+1] {
	case '[': // CSI: ESC [ ... final byte 0x40-0x7e
		j := i + 2
		for j < len(s) {
			c := s[j]
			j++
			if c >= 0x40 && c <= 0x7e {
				break
			}
		}
		return j
	case ']': // OSC: ESC ] ... BEL or ST
		return skipStringEscape(s, i+2)
	case 'P', '_', '^', 'X': // DCS/APC/PM/SOS: ESC P ... ST, etc.
		return skipStringEscape(s, i+2)
	default:
		// Two-byte escape (cursor save/restore, charset select, etc.).
		return i + 2
	}
}

func skipStringEscape(s string, i int) int {
	for i < len(s) {
		if s[i] == 0x07 { // BEL
			return i + 1
		}
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' { // ST
			return i + 2
		}
		i++
	}
	return len(s)
}
