package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/bnema/zut/packages/provider"
)

// SessionTitleMaxChars is the maximum length of an automatically generated
// session title. The limit is measured in Unicode code points so a title never
// cuts a UTF-8 sequence in half.
const SessionTitleMaxChars = 40

const sessionTitleMaxOutputTokens = 16
const sessionTitlePromptMaxChars = 12000

const sessionTitleSystemPrompt = `You create concise titles for coding sessions.
Return only the subject title for the user's task. Maximum 40 characters. Use one
line. Do not include quotes, Markdown, explanations, or trailing punctuation.`

// GenerateSessionTitle asks the provider for a short title without touching a
// conversation transcript. It is intentionally separate from Agent.Prompt:
// title generation is UI/session metadata, not an assistant turn.
func GenerateSessionTitle(ctx context.Context, client provider.Client, model, prompt string) (string, error) {
	if client == nil {
		return "", fmt.Errorf("title generation: provider client is nil")
	}
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("title generation: prompt is empty")
	}

	promptRunes := []rune(prompt)
	if len(promptRunes) > sessionTitlePromptMaxChars {
		prompt = string(promptRunes[:sessionTitlePromptMaxChars])
	}
	stream, err := client.Stream(ctx, provider.Request{
		Model:     model,
		System:    sessionTitleSystemPrompt,
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: prompt}}}},
		MaxTokens: sessionTitleMaxOutputTokens,
	})
	if err != nil {
		return "", fmt.Errorf("title generation request: %w", err)
	}

	text, err := collectStreamText(ctx, stream, "title generation", 0)
	if err != nil {
		return "", err
	}
	if title := NormalizeSessionTitle(text); title != "" {
		return title, nil
	}
	return "", fmt.Errorf("title generation returned an empty title")
}

// NormalizeSessionTitle turns model or user text into one safe, compact
// session-title value. It is also used for local fallbacks before a title is
// sent to a terminal escape sequence.
func NormalizeSessionTitle(text string) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == 0x7f {
			return -1
		}
		return r
	}, text)
	text = strings.TrimSpace(text)
	text = strings.Trim(text, "\"'`")
	text = strings.TrimSpace(text)
	for _, prefix := range []string{"title:", "subject:"} {
		if len(text) >= len(prefix) && strings.EqualFold(text[:len(prefix)], prefix) {
			text = strings.TrimSpace(text[len(prefix):])
			break
		}
	}
	text = strings.Join(strings.Fields(text), " ")
	text = strings.Trim(text, "\"'`")
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) > SessionTitleMaxChars {
		if SessionTitleMaxChars == 1 {
			return string(runes[:1])
		}
		return string(runes[:SessionTitleMaxChars-1]) + "…"
	}
	return text
}

// errStreamTextLimit reports that collectStreamText stopped at its byte cap.
var errStreamTextLimit = errors.New("stream text exceeds limit")

// collectStreamText gathers the text of one provider stream for a hidden side
// request. It falls back to the final message text when no deltas arrive and
// ignores tool calls and reasoning. maxBytes > 0 stops reading once the text
// exceeds it and returns errStreamTextLimit. The stream is drained in the
// background on every return so the provider goroutine never blocks. op
// prefixes error messages.
func collectStreamText(ctx context.Context, stream <-chan provider.Event, op string, maxBytes int) (string, error) {
	defer func() { go drainProviderStream(stream) }()
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
			return "", fmt.Errorf("%s stream: %w", op, ctx.Err())
		case event, ok := <-stream:
			if !ok {
				streamOpen = false
				continue
			}
			switch e := event.(type) {
			case provider.EventTextDelta:
				text.WriteString(e.Delta)
				if maxBytes > 0 && text.Len() > maxBytes {
					return "", errStreamTextLimit
				}
			case provider.EventDone:
				done = true
				finalErr = e.Err
				finalMsg = e.Message
			}
		}
	}
	if finalErr != nil {
		return "", fmt.Errorf("%s stream: %w", op, finalErr)
	}
	if !done {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("%s stream: %w", op, err)
		}
		return "", fmt.Errorf("%s stream ended without a result", op)
	}
	if text.Len() == 0 {
		text.WriteString(textFromMessage(finalMsg))
		if maxBytes > 0 && text.Len() > maxBytes {
			return "", errStreamTextLimit
		}
	}
	return text.String(), nil
}

func textFromMessage(message provider.Message) string {
	var text strings.Builder
	for _, content := range message.Content {
		if block, ok := content.(provider.TextBlock); ok {
			text.WriteString(block.Text)
		}
	}
	return text.String()
}
