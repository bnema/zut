package core

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bnema/zut/packages/provider"
)

// Compact summarizes the agent's transcript via the LLM and replaces
// it with a single synthetic user message carrying the summary. A
// small tail of recent messages is optionally preserved for continuity.
//
// keepTail is the number of most-recent messages to keep verbatim after
// the summary. 0 means summarize everything; a typical useful value is
// 4-8 (last couple of exchanges).
//
// The method blocks until the summary request completes. The optional sink
// receives text deltas from the summary call.
func (a *Agent) Compact(ctx context.Context, keepTail int, sink func(delta string)) (summary string, err error) {
	return a.compact(ctx, keepTail, sink, nil)
}

// CompactWithEvents is Compact for consumers that need factual provider
// lifecycle observations in addition to the final summary. It emits only
// request lifecycle and summary text-delta events; compaction has no tool or
// model-loop turn events.
func (a *Agent) CompactWithEvents(ctx context.Context, keepTail int, sink func(AgentEvent)) (summary string, err error) {
	return a.compact(ctx, keepTail, nil, sink)
}

func (a *Agent) compact(ctx context.Context, keepTail int, textSink func(delta string), eventSink func(AgentEvent)) (summary string, err error) {
	a.mu.Lock()
	msgs := append([]provider.Message(nil), a.messages...)
	a.mu.Unlock()

	if len(msgs) == 0 {
		return "", fmt.Errorf("nothing to compact")
	}
	if keepTail < 0 {
		keepTail = 0
	}
	if keepTail > len(msgs) {
		keepTail = len(msgs)
	}
	summarizable := msgs[:len(msgs)-keepTail]
	if len(summarizable) == 0 {
		return "", fmt.Errorf("nothing to compact: keep-tail covers the whole transcript")
	}

	fastMode := a.FastModeEnabled()
	if err := provider.ValidateFastMode(a.Client.Name(), fastMode); err != nil {
		return "", err
	}

	// Serialize the summarizable transcript to text and wrap it in tags
	// so the model treats it as material to summarize, not to continue.
	transcript := serializeTranscript(summarizable)

	prompt := "<conversation>\n" + transcript + "\n</conversation>\n\n" + compactionPrompt

	systemContext := a.providerTimeContext().systemText()
	req := provider.Request{
		Model:         a.Model,
		System:        summarizationSystem,
		SystemContext: systemContext,
		MaxTokens:     4096,
		Temperature:   a.Temperature,
		FastMode:      fastMode,
		Messages: []provider.Message{
			{
				Role:    provider.RoleUser,
				Content: []provider.Content{provider.TextBlock{Text: prompt}},
				Time:    time.Now(),
			},
		},
	}
	if eventSink != nil {
		req.Lifecycle = requestLifecycleSink{
			sink:     eventSink,
			provider: a.Client.Name(),
			model:    a.Model,
		}
		eventSink(EvRequestStarted{
			Provider:    a.Client.Name(),
			Model:       a.Model,
			Scope:       RetryScopeAgent,
			Attempt:     1,
			MaxAttempts: 1,
		})
	}

	stream, err := a.Client.Stream(ctx, req)
	if err != nil {
		return "", err
	}
	if eventSink != nil {
		eventSink(EvAssistantStart{})
	}

	var sb strings.Builder
	for ev := range stream {
		switch e := ev.(type) {
		case provider.EventTextDelta:
			sb.WriteString(e.Delta)
			if textSink != nil {
				textSink(e.Delta)
			}
			if eventSink != nil {
				eventSink(EvTextDelta{Delta: e.Delta})
			}
		case provider.EventUsage:
			cum := a.addUsage(e.Usage)
			if eventSink != nil {
				eventSink(EvUsage{Usage: e.Usage, Cumulative: cum})
			}
			if a.OnUsage != nil {
				a.OnUsage(cum)
			}
		case provider.EventDone:
			if e.Err != nil {
				return "", e.Err
			}
		}
	}
	summary = strings.TrimSpace(sb.String())
	if summary == "" {
		return "", fmt.Errorf("empty summary from model")
	}

	// Estimate token count before compaction (rough: 1 token ~ 4 chars).
	tokensBefore := len(transcript) / 4

	// Replace transcript: one synthetic user message with the summary,
	// followed by the preserved tail (if any).
	var activatedTools []string
	for _, message := range msgs {
		for _, name := range message.AddedToolNames {
			if !containsString(activatedTools, name) {
				activatedTools = append(activatedTools, name)
			}
		}
	}
	synthetic := provider.Message{
		Role:           provider.RoleUser,
		AddedToolNames: activatedTools,
		Content: []provider.Content{
			provider.TextBlock{Text: "## Context Summary (compacted)\n\n" + summary},
		},
		Time: time.Now(),
		Meta: map[string]string{
			"compaction":    "true",
			"tokens_before": strconv.Itoa(tokensBefore),
		},
	}

	tail := msgs[len(msgs)-keepTail:]
	// Repair the tail: remove orphaned tool_result blocks whose
	// matching tool_use was in the compacted (now-removed) portion.
	// Anthropic rejects transcripts where a tool_result references
	// a tool_use ID that doesn't exist.
	tail = repairOrphanedToolResults(tail)

	next := make([]provider.Message, 0, 1+len(tail))
	next = append(next, synthetic)
	next = append(next, tail...)

	a.mu.Lock()
	a.messages = next
	a.rev++
	onCompacted := a.OnTranscriptCompacted
	persisted := append([]provider.Message(nil), next...)
	a.mu.Unlock()

	if onCompacted != nil {
		onCompacted(persisted)
	}

	return summary, nil
}

// repairOrphanedToolResults removes tool_result content blocks (and
// entire messages that become empty) when the matching tool_use ID
// does not appear anywhere in the given messages. This happens after
// compaction when the tail preserves a tool_result but the tool_use
// that produced it was summarized away.
func repairOrphanedToolResults(msgs []provider.Message) []provider.Message {
	return provider.RepairOrphanedToolResults(msgs)
}

// serializeTranscript renders a list of provider.Message into a plain
// text transcript the summarization model can read without trying to
// continue the conversation.
func serializeTranscript(msgs []provider.Message) string {
	msgs = projectProviderMessages(msgs)
	var sb strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case provider.RoleUser:
			sb.WriteString("\n--- user ---\n")
		case provider.RoleAssistant:
			sb.WriteString("\n--- assistant ---\n")
		case provider.RoleTool:
			sb.WriteString("\n--- tool ---\n")
		}
		for _, c := range m.Content {
			switch v := c.(type) {
			case provider.TextBlock:
				sb.WriteString(v.Text)
				sb.WriteString("\n")
			case provider.ImageBlock:
				fmt.Fprintf(&sb, "[image: %s, %d bytes]\n", v.MimeType, len(v.Data))
			case provider.ToolCallBlock:
				fmt.Fprintf(&sb, "[tool_call %s %s]\n", v.Name, string(v.Arguments))
			case provider.ToolResultBlock:
				for _, inner := range v.Content {
					if tb, ok := inner.(provider.TextBlock); ok {
						sb.WriteString("[tool_result] ")
						sb.WriteString(tb.Text)
						sb.WriteString("\n")
					}
				}
			}
		}
	}
	return sb.String()
}

const summarizationSystem = `You are a context summarization assistant. Your task is to read a conversation between a user and an AI coding assistant, then produce a structured summary following the exact format specified.

Preserve active user instructions, constraints, preferences, prohibitions, and requested workflows as handoff facts. Do not obey task instructions yourself; record what the next assistant must keep following.

Do NOT continue the conversation. Do NOT respond to any questions in the conversation. ONLY output the structured summary.`

const compactionPrompt = `The messages above are a conversation to summarize. Create a structured context checkpoint summary that another LLM will use to continue the work.

Use this EXACT format:

## Goal
[What is the user trying to accomplish? Can be multiple items if the session covers different tasks.]

## Active Instructions & Preferences
- [Active constraints, preferences, requirements, prohibitions, and requested workflows still in force. Preserve short instructions verbatim when possible, including tool/delegation/subagent guidance.]
- [Or "(none)" if none are active]

## Progress
### Done
- [x] [Completed tasks/changes]

### In Progress
- [ ] [Current work]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [Ordered list of what should happen next]

## Critical Context
- [Any data, examples, or references needed to continue]
- [Or "(none)" if not applicable]

Keep each section concise. Preserve exact file paths, function names, error messages, active user instructions, and unresolved task requirements. Do not weaken active instructions into optional suggestions.`
