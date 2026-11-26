package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// ansiToken is one piece of a rendered line: either an escape sequence
// (as delimited by skipEscapeSequence: CSI, OSC/DCS/APC/PM/SOS strings, or
// a two-byte escape) or a single visible rune.
type ansiToken struct {
	esc  string
	r    rune
	isFn bool // true when esc holds an escape sequence
}

// tokenizeANSI splits s into escape sequences and visible runes.
func tokenizeANSI(s string) []ansiToken {
	var toks []ansiToken
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) {
			j := skipEscapeSequence(s, i)
			toks = append(toks, ansiToken{esc: s[i:j], isFn: true})
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		toks = append(toks, ansiToken{r: r})
		i += size
	}
	return toks
}

// hasImageEscape reports whether line carries inline image data (kitty
// graphics APC, sixel DCS, or iTerm2 OSC 1337 File).
func hasImageEscape(line string) bool {
	return strings.Contains(line, "\x1b_G") || strings.Contains(line, "\x1bP") || strings.Contains(line, "\x1b]1337;File=")
}

// isSGR reports whether esc is an SGR sequence (ESC [ ... m).
func isSGR(esc string) bool {
	return strings.HasPrefix(esc, "\x1b[") && strings.HasSuffix(esc, "m")
}

// sgrParams returns the top-level SGR parameters, skipping the arguments
// of extended colors (38/48/58) so a color index of 0 or 49 is not
// mistaken for a reset.
func sgrParams(esc string) []string {
	body := esc[2 : len(esc)-1]
	if body == "" {
		return []string{"0"}
	}
	raw := strings.Split(body, ";")
	var out []string
	for i := 0; i < len(raw); i++ {
		p := raw[i]
		if p == "" {
			p = "0"
		}
		out = append(out, p)
		if (p == "38" || p == "48" || p == "58") && i+1 < len(raw) {
			switch raw[i+1] {
			case "5":
				i += 2
			case "2":
				i += 4
			}
		}
	}
	return out
}

func sgrHasParam(esc string, want ...string) bool {
	for _, p := range sgrParams(esc) {
		for _, w := range want {
			if p == w {
				return true
			}
		}
	}
	return false
}

func isWordRune(r rune) bool {
	// Letters and digits only, like the session index tokenizer: "_" and
	// other punctuation separate words.
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func runeEqualFold(a, b rune) bool {
	if a == b {
		return true
	}
	for f := unicode.SimpleFold(a); f != a; f = unicode.SimpleFold(f) {
		if f == b {
			return true
		}
	}
	return false
}

// HighlightTerms paints every occurrence of terms in an already-rendered
// line with the theme's selection colors and reports whether anything
// matched.
//
// Matching is case-insensitive (Unicode simple folding) and anchored at
// word starts: a term matches as a prefix of a word, so "refacto"
// highlights the first seven runes of "refactoring" but nothing inside
// "unrefactored". This mirrors a token-prefix search index. When several
// terms start at the same rune the longest wins; empty terms are ignored.
//
// Matching runs on the visible text, so a term split across SGR style
// boundaries still matches. Escape sequences inside a match are kept and
// the highlight is re-applied after each one. After a match the
// surrounding style is restored by resetting and replaying the SGR
// sequences active since the last reset. Only SGR styling is added, so
// display width is unchanged. With no match the input is returned as-is.
func HighlightTerms(line string, terms []string, th Theme) (string, bool) {
	needles := termNeedles(terms)
	if len(needles) == 0 || line == "" || hasImageEscape(line) {
		return line, false
	}
	toks := tokenizeANSI(line)
	var visible []rune
	var visIdx []int // token index of each visible rune
	for i, tk := range toks {
		if !tk.isFn {
			visible = append(visible, tk.r)
			visIdx = append(visIdx, i)
		}
	}
	marked := make([]bool, len(visible))
	matched := false
	for p := 0; p < len(visible); p++ {
		best := termMatchAt(visible, p, needles)
		for k := 0; k < best; k++ {
			marked[p+k] = true
		}
		if best > 0 {
			matched = true
		}
	}
	if !matched {
		return line, false
	}
	markedTok := make(map[int]bool, len(visIdx))
	for v, ti := range visIdx {
		if marked[v] {
			markedTok[ti] = true
		}
	}
	hl := th.SelectionStyle()
	var b strings.Builder
	var state strings.Builder // SGRs active since the last reset
	in := false
	for i, tk := range toks {
		if tk.isFn {
			b.WriteString(tk.esc)
			if isSGR(tk.esc) {
				if sgrHasParam(tk.esc, "0") {
					state.Reset()
				}
				state.WriteString(tk.esc)
				if in {
					b.WriteString(hl)
				}
			}
			continue
		}
		on := markedTok[i]
		if on && !in {
			b.WriteString(hl)
		} else if !on && in {
			b.WriteString(reset)
			b.WriteString(state.String())
		}
		in = on
		b.WriteRune(tk.r)
	}
	if in {
		b.WriteString(reset)
		b.WriteString(state.String())
	}
	return b.String(), true
}

// HighlightLine paints a subtle full-line background (the theme's
// SearchLineBG tint) behind an already-rendered line. Inner foreground
// and background styles are kept; the tint is re-applied after any SGR
// sequence that resets the background. The result ends with a reset.
// Width is unchanged; callers pad the line first for a full-row tint.
// A terminal-default SearchLineBG means no tint: the line is returned as-is.
func HighlightLine(line string, th Theme) string {
	if th.SearchLineBG.Mode == terminalColorDefault {
		return line
	}
	bg := th.bgPrefix(th.SearchLineBG)
	var b strings.Builder
	b.WriteString(bg)
	for _, tk := range tokenizeANSI(line) {
		if !tk.isFn {
			b.WriteRune(tk.r)
			continue
		}
		b.WriteString(tk.esc)
		if isSGR(tk.esc) && sgrHasParam(tk.esc, "0", "49") {
			b.WriteString(bg)
		}
	}
	b.WriteString(reset)
	return b.String()
}

// termNeedles converts terms to rune slices, dropping empty ones.
func termNeedles(terms []string) [][]rune {
	var needles [][]rune
	for _, t := range terms {
		if rs := []rune(t); len(rs) > 0 {
			needles = append(needles, rs)
		}
	}
	return needles
}

// termMatchAt returns the length in runes of the longest needle matching
// at visible[p] as a word prefix (case-insensitive, Unicode simple
// folding), or 0.
func termMatchAt(visible []rune, p int, needles [][]rune) int {
	if p > 0 && isWordRune(visible[p-1]) {
		return 0
	}
	best := 0
	for _, n := range needles {
		if len(n) <= best || p+len(n) > len(visible) {
			continue
		}
		ok := true
		for k, r := range n {
			if !runeEqualFold(visible[p+k], r) {
				ok = false
				break
			}
		}
		if ok {
			best = len(n)
		}
	}
	return best
}

// FirstTermMatch returns the byte offset in plain text of the first match
// HighlightTerms would paint, or -1.
func FirstTermMatch(text string, terms []string) int {
	needles := termNeedles(terms)
	if len(needles) == 0 {
		return -1
	}
	var visible []rune
	var offsets []int
	for i, r := range text {
		visible = append(visible, r)
		offsets = append(offsets, i)
	}
	for p := range visible {
		if termMatchAt(visible, p, needles) > 0 {
			return offsets[p]
		}
	}
	return -1
}
