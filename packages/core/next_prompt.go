package core

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/bnema/zut/packages/provider"
	"github.com/google/uuid"
)

// NextPromptNoPrediction is the sentinel the model returns when it has no
// confident guess for the user's next message.
const NextPromptNoPrediction = "NO_PREDICTION"

const nextPromptMaxRunes = 300

const nextPromptInstruction = `[Side request: ignore the coding task for this one reply and do not call any tools.]
Guess the next message I am most likely to type to you now.
- Write it as me: match the language, tone, and usual length of the messages I typed myself. Ignore tool output and your own wording when judging my style.
- Base it on your latest reply: approve it, ask for the obvious next step, or redirect if something looks off.
- Return only that message on one line, normally under 20 words. No quotes, Markdown, labels, or explanation.
- If you cannot guess with confidence, reply exactly NO_PREDICTION.`

// PredictNextPrompt asks the model for the user's likely next message. The
// request reuses the exact prefix of a normal agent turn (see
// buildTurnRequest) plus one trailing user message, so providers can serve it
// from the prompt cache. It never mutates the transcript, emits events, runs
// tools, or updates usage. It returns "" with a nil error when the model has
// no confident prediction.
func (a *Agent) PredictNextPrompt(ctx context.Context) (string, error) {
	if a.Client == nil {
		return "", fmt.Errorf("next prompt prediction: provider client is nil")
	}
	fastMode := a.FastModeEnabled()
	if err := provider.ValidateFastMode(a.Client.Name(), fastMode); err != nil {
		return "", fmt.Errorf("next prompt prediction: %w", err)
	}
	req := a.buildTurnRequest(fastMode)
	if len(req.Messages) == 0 {
		return "", nil
	}
	// buildTurnRequest returns a fresh slice; appending cannot alias the
	// transcript's backing array.
	req.Messages = append(req.Messages, provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: nextPromptInstruction}},
	})
	// CacheSessionID and ThreadID match the agent's turns because providers
	// key prompt-cache affinity and routing on them. TurnID is fresh: this is
	// a distinct logical request and must not reuse an accepted turn's ID.
	a.mu.Lock()
	a.ensureRequestIdentityLocked()
	req.Context = provider.RequestContext{CacheSessionID: a.cacheSessionID, ThreadID: a.sessionID, TurnID: uuid.NewString()}
	a.mu.Unlock()

	stream, err := a.Client.Stream(ctx, req)
	if err != nil {
		return "", fmt.Errorf("next prompt prediction request: %w", err)
	}
	var (
		text     strings.Builder
		done     bool
		finalErr error
		finalMsg provider.Message
	)
	streamOpen := true
	for streamOpen && !done {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("next prompt prediction stream: %w", ctx.Err())
		case event, ok := <-stream:
			if !ok {
				streamOpen = false
				continue
			}
			switch e := event.(type) {
			case provider.EventTextDelta:
				text.WriteString(e.Delta)
			case provider.EventDone:
				done = true
				finalErr = e.Err
				finalMsg = e.Message
			}
		}
	}
	if finalErr != nil {
		return "", fmt.Errorf("next prompt prediction stream: %w", finalErr)
	}
	if !done {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("next prompt prediction stream: %w", err)
		}
		return "", fmt.Errorf("next prompt prediction stream ended without a result")
	}
	if text.Len() == 0 {
		text.WriteString(textFromMessage(finalMsg))
	}
	return normalizeNextPrompt(text.String()), nil
}

// normalizeNextPrompt reduces a model reply to one safe line, or "" when the
// reply is empty, declined, or too long to be a plausible user message.
func normalizeNextPrompt(text string) string {
	text = stripANSI(text)
	text = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) || r == 0x7f {
			return -1
		}
		return r
	}, text)
	text = strings.Join(strings.Fields(text), " ")
	if n := len(text); n >= 2 {
		first, last := text[0], text[n-1]
		if first == last && (first == '"' || first == '\'' || first == '`') {
			text = strings.TrimSpace(text[1 : n-1])
		}
	}
	if text == "" || text == NextPromptNoPrediction || len([]rune(text)) > nextPromptMaxRunes {
		return ""
	}
	return text
}

// stripANSI removes CSI and OSC escape sequences. Other ESC bytes are dropped
// later as control characters.
func stripANSI(text string) string {
	if !strings.ContainsRune(text, 0x1b) {
		return text
	}
	var out strings.Builder
	for i := 0; i < len(text); i++ {
		if text[i] != 0x1b || i+1 >= len(text) {
			out.WriteByte(text[i])
			continue
		}
		switch text[i+1] {
		case '[':
			j := i + 2
			for j < len(text) && (text[j] < 0x40 || text[j] > 0x7e) {
				j++
			}
			i = j
		case ']':
			j := i + 2
			for j < len(text) && text[j] != 0x07 && !(text[j] == 0x1b && j+1 < len(text) && text[j+1] == '\\') {
				j++
			}
			if j < len(text) && text[j] == 0x1b {
				j++
			}
			i = j
		default:
			out.WriteByte(text[i])
		}
	}
	return out.String()
}
