package tools

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// These declarations are documentation, not executable schema validators.
func codemodeSchemaType(schema map[string]any, depth int) string {
	if depth > 8 {
		return "Any"
	}
	if values, ok := schema["enum"].([]any); ok && len(values) > 0 {
		literals := []string{}
		for _, value := range values {
			var literal string
			switch v := value.(type) {
			case string:
				literal = strconv.Quote(v)
			case bool:
				if v {
					literal = "True"
				} else {
					literal = "False"
				}
			case nil:
				literal = "None"
			case float64:
				literal = strconv.FormatFloat(v, 'g', -1, 64)
			default:
				return "Any"
			}
			literals = append(literals, literal)
			if len(strings.Join(literals, ", ")) > 512 {
				return "Any"
			}
		}
		return "Literal[" + strings.Join(literals, ", ") + "]"
	}
	if types, ok := schema["type"].([]any); ok {
		parts := []string{}
		for _, typ := range types {
			parts = append(parts, codemodeSchemaType(map[string]any{"type": typ, "items": schema["items"]}, depth+1))
		}
		if len(parts) > 0 {
			return strings.Join(parts, " | ")
		}
	}
	switch schema["type"] {
	case "string":
		return "str"
	case "integer":
		return "int"
	case "number":
		return "float"
	case "boolean":
		return "bool"
	case "null":
		return "None"
	case "array":
		items, _ := schema["items"].(map[string]any)
		return "list[" + codemodeSchemaType(items, depth+1) + "]"
	case "object":
		return "dict"
	default:
		return "Any"
	}
}

func codemodeSignature(name string, raw json.RawMessage) string {
	var schema map[string]any
	_ = json.Unmarshal(raw, &schema)
	props, _ := schema["properties"].(map[string]any)
	required := map[string]bool{}
	if names, ok := schema["required"].([]any); ok {
		for _, value := range names {
			if s, ok := value.(string); ok {
				required[s] = true
			}
		}
	}
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	params, docs := []string{}, []string{}
	// Leave space for the return annotation and truncation notice. Whole parameter
	// declarations are omitted rather than cutting the stub in the middle of one.
	budget := 3000
	used := map[string]bool{}
	omitted := false
	for _, name := range names {
		prop, _ := props[name].(map[string]any)
		identifier := codemodeIdentifier(name)
		if identifier != name || used[identifier] {
			omitted = true
			continue
		}
		used[identifier] = true
		param := identifier + ": " + codemodeSchemaType(prop, 0)
		if !required[name] {
			param += " = ..."
		}
		if len(param)+2 > budget {
			omitted = true
			continue
		}
		budget -= len(param) + 2
		params = append(params, param)
		if description, ok := prop["description"].(string); ok && description != "" {
			description = strings.Join(strings.Fields(description), " ")
			// Escape docstring delimiters, slashes, and controls with JSON quoting.
			quoted := strconv.Quote(codemodeUTF8Prefix(description, 160))
			docs = append(docs, "    "+name+": "+quoted[1:len(quoted)-1])
		}
	}
	header := "def " + codemodeUTF8Prefix(codemodeIdentifier(name), 160) + "("
	if len(params) > 0 {
		header += "*, " + strings.Join(params, ", ")
	}
	header += ") -> ToolResult:\n"
	if omitted {
		docs = append([]string{"    Some parameters omitted; see raw schema."}, docs...)
	}
	if len(docs) == 0 {
		return header + "    ..."
	}
	out := header + "    \"\"\"\n"
	for _, doc := range docs {
		if len(out)+len(doc)+32 > 4096 {
			break
		}
		out += doc + "\n"
	}
	return out + "    \"\"\"\n    ..."
}
