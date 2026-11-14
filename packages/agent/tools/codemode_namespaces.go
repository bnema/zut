package tools

import (
	"strings"

	"github.com/bnema/zut/packages/core"
)

// codemodeIdentifier uses portable ASCII Python identifiers. MCP registry names
// already use ASCII; keywords and leading digits need additional escaping.
func codemodeIdentifier(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	name := b.String()
	if name == "" {
		return "_"
	}
	if name[0] >= '0' && name[0] <= '9' {
		name = "_" + name
	}
	switch name {
	case "False", "None", "True", "and", "as", "assert", "async", "await", "break", "class", "continue", "def", "del", "elif", "else", "except", "finally", "for", "from", "global", "if", "import", "in", "is", "lambda", "nonlocal", "not", "or", "pass", "raise", "return", "try", "while", "with", "yield":
		name += "_"
	}
	return name
}

func codemodeMCPParts(name string, tool core.Tool) (string, string, bool) {
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return "", "", false
	}
	if owner, ok := tool.(interface{ Server() string }); ok {
		// Match MCP's registry sanitization, which preserves '-' as well as '_'.
		var server strings.Builder
		for _, r := range owner.Server() {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
				server.WriteRune(r)
			} else {
				server.WriteByte('_')
			}
		}
		key := server.String()
		if key == "" {
			key = "_"
		}
		method, found := strings.CutPrefix(rest, key+"__")
		// A truncated server component cannot be recovered safely from its name.
		return owner.Server(), method, found && method != ""
	}
	server, method, found := strings.Cut(rest, "__")
	// Without ownership metadata, multiple separators have no unique parse.
	return server, method, found && server != "" && method != "" && !strings.Contains(method, "__")
}

func codemodeNamespaces(tools core.Registry) map[string]map[string]string {
	out := map[string]map[string]string{}
	owners := map[string]string{}
	blockedServers := map[string]bool{}
	blockedMethods := map[string]map[string]bool{}
	for name, tool := range tools {
		if !codemodeAllowed(name) {
			continue
		}
		server, method, ok := codemodeMCPParts(name, tool)
		if !ok {
			continue
		}
		s, m := codemodeIdentifier(server), codemodeIdentifier(method)
		// Dunder attributes are reserved for Python's protocol machinery.
		if strings.HasPrefix(s, "__") || strings.HasPrefix(m, "__") {
			continue
		}
		if previous, exists := owners[s]; exists && previous != server {
			blockedServers[s] = true
		}
		owners[s] = server
		if out[s] == nil {
			out[s] = map[string]string{}
			blockedMethods[s] = map[string]bool{}
		}
		if _, exists := out[s][m]; exists {
			blockedMethods[s][m] = true
		}
		out[s][m] = name
	}
	for server, methods := range out {
		if blockedServers[server] {
			delete(out, server)
			continue
		}
		for method := range blockedMethods[server] {
			delete(methods, method)
		}
		if len(methods) == 0 {
			delete(out, server)
		}
	}
	return out
}
