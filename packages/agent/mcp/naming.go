package mcp

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"strings"
)

const (
	// ToolPrefix starts every model-facing MCP tool name.
	ToolPrefix = "mcp__"
	// maxToolNameLength is the strictest provider limit (Anthropic and OpenAI
	// accept ^[a-zA-Z0-9_-]{1,64}$).
	maxToolNameLength = 64
	nameHashLength    = 8
)

// sanitizeName keeps ASCII letters, digits, '_' and '-'; everything else
// becomes '_'.
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

func shortHash(parts ...string) string {
	sum := sha1.Sum([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])[:nameHashLength]
}

// QualifiedName returns the model-facing name for server/tool before
// collision handling: mcp__<server>__<tool>, sanitized and length-bounded.
func QualifiedName(server, tool string) string {
	name := ToolPrefix + sanitizeName(server) + "__" + sanitizeName(tool)
	if len(name) <= maxToolNameLength {
		return name
	}
	return truncateWithHash(name, server, tool)
}

func truncateWithHash(name, server, tool string, salt ...string) string {
	suffix := "_" + shortHash(append([]string{server, tool}, salt...)...)
	keep := maxToolNameLength - len(suffix)
	if keep > len(name) {
		keep = len(name)
	}
	return name[:keep] + suffix
}

// assignNames maps each (server, tool) pair to a unique model-facing name.
// Pairs whose sanitized names collide receive a stable hash suffix.
func assignNames(pairs [][2]string, reserved func(string) bool) map[[2]string]string {
	counts := map[string]int{}
	for _, p := range pairs {
		counts[QualifiedName(p[0], p[1])]++
	}
	out := make(map[[2]string]string, len(pairs))
	used := map[string]bool{}
	for _, p := range pairs {
		name := QualifiedName(p[0], p[1])
		if counts[name] > 1 || used[name] || (reserved != nil && reserved(name)) {
			name = truncateWithHash(name, p[0], p[1])
			for attempt := 1; used[name] || (reserved != nil && reserved(name)); attempt++ {
				name = truncateWithHash(name, p[0], p[1], strings.Repeat("x", attempt))
			}
		}
		used[name] = true
		out[p] = name
	}
	return out
}

// NormalizeSchema converts an MCP inputSchema into a provider-safe function
// schema: an object root without top-level composition keywords, which some
// providers reject.
func NormalizeSchema(raw json.RawMessage) json.RawMessage {
	empty := json.RawMessage(`{"type":"object","properties":{}}`)
	if len(trimSpace(raw)) == 0 {
		return empty
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil || schema == nil {
		return empty
	}
	for _, key := range []string{"oneOf", "anyOf", "allOf", "not", "$schema", "$id"} {
		delete(schema, key)
	}
	schema["type"] = "object"
	if props, ok := schema["properties"].(map[string]any); !ok || props == nil {
		schema["properties"] = map[string]any{}
	}
	if req, ok := schema["required"]; ok {
		if list, ok := req.([]any); !ok || len(list) == 0 {
			delete(schema, "required")
		}
	}
	out, err := json.Marshal(schema)
	if err != nil {
		return empty
	}
	return out
}
