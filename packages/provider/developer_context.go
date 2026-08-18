package provider

import "strings"

// systemWithDeveloperContext projects host-authored developer messages onto
// providers that expose only a top-level system instruction. OpenAI adapters
// preserve RoleDeveloper as a native developer input instead.
func systemWithDeveloperContext(req Request) (string, []Message) {
	messages := make([]Message, 0, len(req.Messages))
	context := make([]string, 0)
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
	if len(context) == 0 {
		return req.SystemPrompt(), messages
	}
	system := strings.TrimSpace(req.SystemPrompt())
	if system != "" {
		system += "\n\n"
	}
	return system + strings.Join(context, "\n\n"), messages
}
