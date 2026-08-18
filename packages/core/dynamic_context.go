package core

import (
	"strings"
	"time"

	"github.com/bnema/zut/packages/provider"
)

const internalContextMarker = "internal_context"

// appendDynamicContext persists a provider-visible, host-authored context item
// only when its canonical value changes. It is deliberately separate from the
// stable top-level instructions so changing time or extension state cannot
// rewrite the instruction cache prefix.
func (a *Agent) appendDynamicContext(turnContext string) {
	parts := make([]string, 0, 2)
	if timeContext := a.providerTimeContext().developerText(); timeContext != "" {
		parts = append(parts, timeContext)
	}
	if turnContext = boundedTurnContext(turnContext); turnContext != "" {
		parts = append(parts, "[Extension context]\n"+turnContext)
	}
	text := strings.Join(parts, "\n\n")
	if text == "" {
		return
	}

	message := provider.Message{
		Role:    provider.RoleDeveloper,
		Content: []provider.Content{provider.TextBlock{Text: text}},
		Time:    time.Now(),
		Meta:    map[string]string{internalContextMarker: "true"},
	}

	a.mu.Lock()
	for i := len(a.messages) - 1; i >= 0; i-- {
		if !isInternalContextMessage(a.messages[i]) {
			continue
		}
		if internalContextText(a.messages[i]) == text {
			a.mu.Unlock()
			return
		}
		break
	}
	// The first dynamic item must precede the accepted user task. Persisting it
	// as an append would reverse that order on session reload, so report the
	// resulting transcript replacement instead. Later snapshots extend the
	// history at the next model boundary and remain ordinary appends.
	first := !hasInternalContextMessage(a.messages)
	insertAt := len(a.messages)
	if first {
		insertAt = 0
	}
	a.messages = append(a.messages, provider.Message{})
	copy(a.messages[insertAt+1:], a.messages[insertAt:])
	a.messages[insertAt] = message
	a.rev++
	onCompacted := a.OnTranscriptCompacted
	persisted := append([]provider.Message(nil), a.messages...)
	a.mu.Unlock()
	if first && onCompacted != nil {
		onCompacted(persisted)
		return
	}
	a.fireMessageAppended(message)
}

func hasInternalContextMessage(messages []provider.Message) bool {
	for _, message := range messages {
		if isInternalContextMessage(message) {
			return true
		}
	}
	return false
}

func isInternalContextMessage(message provider.Message) bool {
	return message.Role == provider.RoleDeveloper && message.Meta[internalContextMarker] == "true"
}

func latestInternalContext(messages []provider.Message) (*provider.Message, []provider.Message) {
	filtered := make([]provider.Message, 0, len(messages))
	var latest *provider.Message
	for _, message := range messages {
		if isInternalContextMessage(message) {
			copy := message
			latest = &copy
			continue
		}
		filtered = append(filtered, message)
	}
	return latest, filtered
}

func internalContextText(message provider.Message) string {
	if len(message.Content) != 1 {
		return ""
	}
	text, _ := message.Content[0].(provider.TextBlock)
	return text.Text
}
