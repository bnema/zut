package provider

// WithMessageOrigin records the producing route without changing shared metadata.
// Explicit provenance survives session replay, compaction, and model switches.
func WithMessageOrigin(msg Message, providerName, model string) Message {
	if msg.Role != RoleAssistant || (providerName == "" && model == "") {
		return msg
	}
	meta := make(map[string]string, len(msg.Meta)+2)
	for k, v := range msg.Meta {
		meta[k] = v
	}
	if meta["provider"] == "" && providerName != "" {
		meta["provider"] = providerName
	}
	if meta["model"] == "" && model != "" {
		meta["model"] = model
	}
	msg.Meta = meta
	return msg
}

// replayContent projects model-private state out of foreign assistant turns.
// The stored transcript and ordinary text/tool blocks remain intact. Unknown
// provenance retains historical behavior; each wire adapter still validates
// the shape of opaque items it accepts.
func replayContent(msg Message, providerName, model string) []Content {
	if (msg.Meta["provider"] == "" || msg.Meta["provider"] == providerName) &&
		(msg.Meta["model"] == "" || msg.Meta["model"] == model) {
		return msg.Content
	}
	content := make([]Content, 0, len(msg.Content))
	for _, block := range msg.Content {
		switch v := block.(type) {
		case ReasoningBlock:
			continue
		case TextBlock:
			v.ThoughtSignature = ""
			block = v
		case ImageBlock:
			v.ThoughtSignature = ""
			block = v
		case ToolCallBlock:
			v.ThoughtSignature = ""
			block = v
		}
		content = append(content, block)
	}
	return content
}
