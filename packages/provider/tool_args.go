package provider

import (
	"encoding/json"
	"strings"
)

// normalizeToolArgs turns streamed tool-call arguments into valid JSON.
// Models sometimes emit raw control characters (tabs, newlines) inside JSON
// strings; those are escaped so the call keeps its intended arguments.
// Empty or otherwise unrepairable input becomes "{}" so the message can
// always be serialized to transcripts and sent back to the provider.
func normalizeToolArgs(raw string) json.RawMessage {
	if raw == "" {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(raw)) {
		return json.RawMessage(raw)
	}
	if repaired := escapeControlCharsInStrings(raw); json.Valid([]byte(repaired)) {
		return json.RawMessage(repaired)
	}
	return json.RawMessage("{}")
}

// escapeControlCharsInStrings escapes raw U+0000–U+001F bytes that appear
// inside JSON string literals. Bytes outside strings are left untouched.
func escapeControlCharsInStrings(raw string) string {
	const hex = "0123456789abcdef"
	var b strings.Builder
	b.Grow(len(raw) + 8)
	inString, escaped := false, false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			b.WriteByte(c)
			continue
		}
		switch {
		case escaped:
			escaped = false
			b.WriteByte(c)
		case c == '\\':
			escaped = true
			b.WriteByte(c)
		case c == '"':
			inString = false
			b.WriteByte(c)
		case c == '\t':
			b.WriteString(`\t`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
