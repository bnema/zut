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

	latestContext, msgs := latestInternalContext(msgs)
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

	req := provider.Request{
		Context:     a.beginTurn(),
		Model:       a.Model,
		System:      summarizationSystem,
		MaxTokens:   4096,
		Temperature: a.Temperature,
		FastMode:    fastMode,
		Messages: []provider.Message{
			{
				Role:    provider.RoleUser,
				Content: []provider.Content{provider.TextBlock{Text: prompt}},
				Time:    time.Now(),
			},
		},
	}
	if eventSink != nil || a.OnRetryLifecycle != nil {
		lifecycleEventSink := eventSink
		if lifecycleEventSink == nil {
			lifecycleEventSink = func(AgentEvent) {}
		}
		req.Lifecycle = &requestLifecycleSink{
			sink:     lifecycleEventSink,
			observe:  a.fireRetryLifecycle,
			provider: a.Client.Name(),
			model:    a.Model,
		}
	}
	if eventSink != nil {
		eventSink(EvRequestStarted{
			Provider:    a.Client.Name(),
			Model:       a.Model,
			Scope:       RetryScopeAgent,
			Attempt:     1,
			MaxAttempts: 1,
		})
	}

	a.beginProviderRequest()
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

	// Carry the active plan over the compaction boundary. The step text is
	// otherwise lost with the summarized transcript, so the model could not
	// resolve the indices its indexed update and remove actions need. The block
	// is merged into the internal-context message that survives compaction;
	// with no plan the retained message is left byte-for-byte unchanged.
	if latestContext != nil {
		if block := planContextBlock(a.CurrentPlan()); block != "" {
			*latestContext = mergePlanContextBlock(*latestContext, block)
		}
	}

	next := make([]provider.Message, 0, 2+len(tail))
	if latestContext != nil {
		next = append(next, *latestContext)
	}
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

// planContextStepLimit bounds the steps carried into the retained internal
// context. It is deliberately the only size bound on a plan: state, storage,
// and the tool itself have no cap, but the post-compaction block the model
// reads on every later request stays small.
const planContextStepLimit = 12

// planContextHeader opens the plan block. The whole block is always appended
// last, so the final occurrence of this line marks where a previous block
// starts and mergePlanContextBlock can replace rather than duplicate it.
const planContextHeader = "## Current plan"

// planContextBlock renders the plan carried across compaction. Step lines use
// the same markers as show output, so the model can map them to the listing and
// to the 1-based indices the tool accepts.
func planContextBlock(steps []PlanStep) string {
	if len(steps) == 0 {
		return ""
	}
	shown := steps
	if len(shown) > planContextStepLimit {
		shown = shown[:planContextStepLimit]
	}
	var b strings.Builder
	b.WriteString(planContextHeader)
	for idx, step := range shown {
		fmt.Fprintf(&b, "\n%d. [%s] %s", idx+1, planStepMarker(step.Status), step.Step)
	}
	if remaining := len(steps) - len(shown); remaining > 0 {
		fmt.Fprintf(&b, "\n…and %d more (call action:\"show\")", remaining)
	}
	return b.String()
}

// mergePlanContextBlock returns message with block merged into its internal
// context text. An earlier plan block is replaced, so compacting twice cannot
// duplicate it. A message that is not a single text block is returned
// unchanged rather than risk dropping content.
func mergePlanContextBlock(message provider.Message, block string) provider.Message {
	if block == "" || len(message.Content) != 1 {
		return message
	}
	text, ok := message.Content[0].(provider.TextBlock)
	if !ok {
		return message
	}
	base := stripPlanContextBlock(text.Text)
	merged := block
	if base != "" {
		merged = base + "\n\n" + block
	}
	message.Content = []provider.Content{provider.TextBlock{Text: merged}}
	return message
}

// stripPlanContextBlock removes a previously injected plan block. The block is
// always appended last, so the final header occurrence is its start.
func stripPlanContextBlock(text string) string {
	idx := strings.LastIndex(text, planContextHeader)
	if idx < 0 {
		return text
	}
	return strings.TrimRight(text[:idx], " \t\n")
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
