package provider

import "strings"

// systemWithDeveloperContext projects host-authored developer messages onto
// providers that expose only top-level system instructions. Stable instructions
// remain separate so cache-aware adapters can place their cache boundary before
// the model-only and per-turn context. OpenAI adapters preserve RoleDeveloper
// as a native developer input instead.
func systemWithDeveloperContext(req Request) (stable, dynamic string, messages []Message) {
	messages = make([]Message, 0, len(req.Messages))
	context := make([]string, 0, len(req.Messages)+1)
	if text := strings.TrimSpace(req.SystemContext); text != "" {
		context = append(context, text)
	}
	for _, message := range req.Messages {
		if message.Role != RoleDeveloper {
			messages = append(messages, message)
			continue
		}
		for _, block := range message.Content {
			if text, ok := block.(TextBlock); ok && strings.TrimSpace(text.Text) != "" {
				context = append(context, text.Text)
			}
		}
	}
	return strings.TrimSpace(req.System), strings.Join(context, "\n\n"), messages
}
