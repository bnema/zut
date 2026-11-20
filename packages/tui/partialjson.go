package tui

import "strings"

// ExtractPartialStringField scans raw (a partial JSON object's bytes)
// for the given top-level string field and returns the unescaped
// value seen so far. If the value is still being written, it returns
// what's available with ok=true but done=false. If the closing
// unescaped quote has been reached, done=true.
//
// This is deliberately small and best-effort: zut uses it to show
// the live body of a `write` tool call while the model is still
// typing it, before the full JSON object has been received. It
// assumes the field is a top-level key (no nested lookup), matches
// the first occurrence, and tolerates unfinished `\uXXXX` escapes
// by dropping a trailing incomplete escape sequence.
//
// A production-grade JSON parser would be overkill for this use
// case; we only care about extracting one field incrementally.
func ExtractPartialStringField(raw, field string) (value string, ok, done bool) {
	needle := "\"" + field + "\":"
	idx := strings.Index(raw, needle)
	if idx < 0 {
		return "", false, false
	}
	// Skip over the key and any whitespace up to the opening quote.
	rest := raw[idx+len(needle):]
	j := 0
	for j < len(rest) && (rest[j] == ' ' || rest[j] == '\t' || rest[j] == '\n' || rest[j] == '\r') {
		j++
	}
	if j >= len(rest) || rest[j] != '"' {
		// Field wasn't a string, or the opening quote hasn't arrived.
		return "", false, false
	}
	j++ // past opening quote

	var sb strings.Builder
	sb.Grow(len(rest) - j)
	for j < len(rest) {
		c := rest[j]
		if c == '\\' {
			// Escape sequence. Need at least one more byte; if not
			// present yet, stop emitting here and wait for more.
			if j+1 >= len(rest) {
				return sb.String(), true, false
			}
			esc := rest[j+1]
			switch esc {
			case '"':
				sb.WriteByte('"')
				j += 2
			case '\\':
				sb.WriteByte('\\')
				j += 2
			case '/':
				sb.WriteByte('/')
				j += 2
			case 'n':
				sb.WriteByte('\n')
				j += 2
			case 't':
				sb.WriteByte('\t')
				j += 2
			case 'r':
				sb.WriteByte('\r')
				j += 2
			case 'b':
				sb.WriteByte('\b')
				j += 2
			case 'f':
				sb.WriteByte('\f')
				j += 2
			case 'u':
				// \uXXXX — needs 4 more hex digits. If we don't have
				// them yet, drop the incomplete sequence and wait.
				if j+6 > len(rest) {
					return sb.String(), true, false
				}
				r := parseHex4(rest[j+2 : j+6])
				if r < 0 {
					// Malformed; stop, return what we have.
					return sb.String(), true, false
				}
				sb.WriteRune(rune(r))
				j += 6
			default:
				// Unknown escape; keep the backslash and the next
				// byte as literals so the render shows something.
				sb.WriteByte(c)
				sb.WriteByte(esc)
				j += 2
			}
			continue
		}
		if c == '"' {
			// End of string.
			return sb.String(), true, true
		}
		sb.WriteByte(c)
		j++
	}
	// Ran out of input before finding the closing quote.
	return sb.String(), true, false
}

// ExtractLastNewText finds the most recent `"newText"` field
// inside an array of edit objects, scanning from the end of raw
// backwards so we get the one currently being streamed rather
// than an earlier completed edit. Returns the partial string
// value the same way ExtractPartialStringField does, plus the
// 1-indexed edit number in the array (so the UI can show
// "edit 2 of N" or similar).
//
// This is aimed at the `edit` tool's streaming shape:
//
//	{"path":"...","edits":[{"oldText":"x","newText":"y"},
//	                         {"oldText":"a","newText":"b<streaming>
//
// We want to show `b<streaming>` while it grows.
func ExtractLastNewText(raw string) (value string, ok, done bool, editIdx int) {
	return ExtractLastStringField(raw, "newText")
}

// ExtractLastStringField finds the last `"<field>":` key in raw and
// returns its partial string value the same way ExtractPartialStringField
// does, plus the 1-indexed occurrence number of that key (0 when the key
// has not appeared yet).
func ExtractLastStringField(raw, field string) (value string, ok, done bool, idx int) {
	needle := "\"" + field + "\":"
	last := strings.LastIndex(raw, needle)
	if last < 0 {
		return "", false, false, 0
	}
	idx = strings.Count(raw[:last], needle) + 1
	suffix := raw[last+len(needle):]
	j := 0
	for j < len(suffix) && (suffix[j] == ' ' || suffix[j] == '\t' || suffix[j] == '\n' || suffix[j] == '\r') {
		j++
	}
	if j >= len(suffix) || suffix[j] != '"' {
		return "", false, false, idx
	}
	// Reuse the single-field extractor by feeding it a synthetic
	// {"<field>":...} wrapper so all its escape handling stays in
	// one place.
	value, ok, done = ExtractPartialStringField("{"+needle+suffix[j:], field)
	return value, ok, done, idx
}

// LiveEdit is the edit currently streaming in an `edit` call's args.
type LiveEdit struct {
	Idx     int    // 1-indexed edit number; 0 when no oldText has started
	OldText string // partial or complete oldText of edit Idx
	OldDone bool
	NewText string // partial newText of edit Idx; "" until it starts
	NewDone bool
}

// ExtractCurrentEdit returns the edit being streamed. The current edit is
// the last oldText; its newText is the last newText only when both share
// the same index, otherwise the newText has not started yet.
func ExtractCurrentEdit(raw string) LiveEdit {
	oldText, _, oldDone, idx := ExtractLastStringField(raw, "oldText")
	e := LiveEdit{Idx: idx, OldText: oldText, OldDone: oldDone}
	if idx == 0 {
		return e
	}
	if newText, _, newDone, newIdx := ExtractLastStringField(raw, "newText"); newIdx == idx {
		e.NewText, e.NewDone = newText, newDone
	}
	return e
}

func parseHex4(s string) int {
	if len(s) != 4 {
		return -1
	}
	n := 0
	for i := 0; i < 4; i++ {
		var d int
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		default:
			return -1
		}
		n = n<<4 | d
	}
	return n
}
