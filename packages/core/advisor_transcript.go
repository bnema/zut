package core

import (
	"fmt"
	"strings"

	"github.com/bnema/zut/packages/provider"
)

// SerializeAdvisorTranscript renders the executor's context as plain text for
// an advisor model: the system prompt, the tool definitions the model can
// currently see, and every message in the provider-visible projection.
//
// The tool call whose ID is pendingCallID (the in-flight advisor call) is
// omitted. Other tool calls without a matching result are marked
// "(result pending)". Reasoning blocks are omitted and images are rendered as
// placeholders. Deferred tools are listed only once a tool result activated
// them, matching what providers send.
func SerializeAdvisorTranscript(system string, tools []provider.Tool, msgs []provider.Message, pendingCallID string) string {
	return SerializeAdvisorContext(system, tools, msgs) + "\n=== transcript ===\n" + SerializeAdvisorMessages(msgs, pendingCallID)
}

// SerializeAdvisorContext renders the executor's system prompt and the tool
// definitions it can currently see. msgs decide which deferred tools are
// active.
func SerializeAdvisorContext(system string, tools []provider.Tool, msgs []provider.Message) string {
	msgs = projectProviderMessages(msgs)
	var sb strings.Builder

	sb.WriteString("=== system prompt ===\n")
	sb.WriteString(system)
	sb.WriteString("\n")

	activated := make(map[string]bool)
	for _, m := range msgs {
		for _, name := range m.AddedToolNames {
			activated[name] = true
		}
	}
	sb.WriteString("\n=== tools ===\n")
	for _, t := range tools {
		if t.Deferred && !activated[t.Name] {
			continue
		}
		fmt.Fprintf(&sb, "\n## %s\n", t.Name)
		if t.Description != "" {
			sb.WriteString(t.Description)
			sb.WriteString("\n")
		}
		if len(t.Schema) > 0 {
			sb.WriteString("schema: ")
			sb.Write(t.Schema)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// SerializeAdvisorMessages renders msgs (a whole transcript or a later slice
// of it) in the provider-visible projection. See SerializeAdvisorTranscript.
func SerializeAdvisorMessages(msgs []provider.Message, pendingCallID string) string {
	msgs = projectProviderMessages(msgs)
	var sb strings.Builder
	answered := make(map[string]bool)
	for _, m := range msgs {
		for _, c := range m.Content {
			if r, ok := c.(provider.ToolResultBlock); ok {
				answered[r.CallID] = true
			}
		}
	}

	for _, m := range msgs {
		var body strings.Builder
		for _, c := range m.Content {
			switch v := c.(type) {
			case provider.TextBlock:
				body.WriteString(v.Text)
				body.WriteString("\n")
			case provider.ImageBlock:
				writeImagePlaceholder(&body, v)
			case provider.ToolCallBlock:
				if pendingCallID != "" && v.ID == pendingCallID {
					continue
				}
				fmt.Fprintf(&body, "[tool_call id=%s %s %s]", v.ID, v.Name, string(v.Arguments))
				if !answered[v.ID] {
					body.WriteString(" (result pending)")
				}
				body.WriteString("\n")
			case provider.ServerToolBlock:
				if line := v.TranscriptLine(); line != "" {
					body.WriteString(line)
					body.WriteString("\n")
				}
			case provider.ToolResultBlock:
				if pendingCallID != "" && v.CallID == pendingCallID {
					continue
				}
				fmt.Fprintf(&body, "[tool_result id=%s", v.CallID)
				if v.IsError {
					body.WriteString(" error")
				}
				body.WriteString("]\n")
				for _, inner := range v.Content {
					switch iv := inner.(type) {
					case provider.TextBlock:
						body.WriteString(iv.Text)
						body.WriteString("\n")
					case provider.ImageBlock:
						writeImagePlaceholder(&body, iv)
					}
				}
			}
		}
		if body.Len() == 0 {
			continue
		}
		sb.WriteString(transcriptRoleHeader(m.Role))
		sb.WriteString(body.String())
	}
	return sb.String()
}

func transcriptRoleHeader(role provider.Role) string {
	switch role {
	case provider.RoleUser:
		return "\n--- user ---\n"
	case provider.RoleAssistant:
		return "\n--- assistant ---\n"
	case provider.RoleTool:
		return "\n--- tool ---\n"
	}
	return ""
}

func writeImagePlaceholder(sb *strings.Builder, v provider.ImageBlock) {
	fmt.Fprintf(sb, "[image: %s, %d bytes]\n", v.MimeType, len(v.Data))
}
