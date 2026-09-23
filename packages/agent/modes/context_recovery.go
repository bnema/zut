package modes

import (
	"context"
	"fmt"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// ContextRecoveryOptions configures the host-owned, one-shot recovery for a
// provider context-overflow error. It deliberately does not add a terminal
// continuation policy to core.Agent.
type ContextRecoveryOptions struct {
	// PersistCompaction records the compacted transcript before the existing
	// prompt is continued. A persistence failure restores the pre-compaction
	// transcript and prevents the continuation request.
	PersistCompaction func([]provider.Message) error

	// ContextWindow and AutoCompactThreshold enable proactive compaction before
	// Prompt. The threshold uses the same supported percentage values and 85%
	// default as interactive mode; zero disables it. Context usage includes
	// uncached input, cache reads, and cache writes from the latest request.
	ContextWindow        int
	AutoCompactThreshold *int

	// SuppressInitialOverflowEvent hides the recoverable first-attempt
	// EvTurnEnd overflow from sinks whose protocol cannot retract a terminal
	// error frame, such as JSONL. A retry overflow is never suppressed.
	SuppressInitialOverflowEvent bool
}

// ContextRecoveryResult describes a completed host-level context recovery.
type ContextRecoveryResult struct {
	// OutputStart is the first message to append after a persisted compaction.
	// When Compacted is false, it is the message count before Prompt.
	OutputStart int
	Compacted   bool
}

// PromptWithContextRecovery proactively compacts a transcript whose latest
// request reached the configured context percentage, then appends prompt and
// runs it once. If that provider request still exceeds its context window, it
// compacts the existing transcript and continues the already-appended prompt
// exactly once. Cancellation, a compaction failure, and a second overflow
// remain terminal outcomes.
func PromptWithContextRecovery(ctx context.Context, ag *core.Agent, prompt string, images []provider.ImageBlock, sink func(core.AgentEvent), options ContextRecoveryOptions) (result ContextRecoveryResult, err error) {
	result.OutputStart = len(ag.Messages())
	if sink == nil {
		sink = func(core.AgentEvent) {}
	}

	usage := ag.LastTurnUsage()
	contextUsed := usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens
	threshold := NormalizeAutoCompactThreshold(options.AutoCompactThreshold)
	proactiveSummaryOverflow := false
	if ShouldAutoCompact(contextUsed, options.ContextWindow, threshold) {
		messages := ag.Messages()
		// A sole transcript message is commonly the worker's only task.
		// Replacing it before it has run loses the exact instruction.
		if len(messages) > 1 {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			compacted, compactErr := compactContext(ctx, ag, messages, options.PersistCompaction)
			if compactErr != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return result, ctxErr
				}
				if !provider.IsContextOverflowError(compactErr) {
					return result, fmt.Errorf("proactively compact transcript: %w", compactErr)
				}
				// The summary request can itself exceed a provider limit. The
				// transcript is unchanged, so make one normal prompt attempt. If
				// that also overflows, do not repeat the doomed summary request.
				proactiveSummaryOverflow = true
			} else {
				result.OutputStart = len(compacted)
				result.Compacted = true
				if err := ctx.Err(); err != nil {
					return result, err
				}
			}
		}
	}

	firstSink := sink
	if options.SuppressInitialOverflowEvent && !proactiveSummaryOverflow {
		firstSink = func(event core.AgentEvent) {
			if turnEnd, ok := event.(core.EvTurnEnd); ok && provider.IsContextOverflowError(turnEnd.Err) {
				return
			}
			sink(event)
		}
	}

	if err = ag.Prompt(ctx, prompt, images, firstSink); err == nil || ctx.Err() != nil || !provider.IsContextOverflowError(err) || proactiveSummaryOverflow {
		return result, err
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	messages := ag.Messages()
	if len(messages) <= 1 {
		// The failed prompt is the only message. Summarizing it would replace
		// the user's task with an LLM-generated paraphrase.
		return result, err
	}
	compacted, compactErr := compactContext(ctx, ag, messages, options.PersistCompaction)
	if compactErr != nil {
		return result, fmt.Errorf("compact transcript after initial overflow: %w", compactErr)
	}

	result.OutputStart = len(compacted)
	result.Compacted = true
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	return result, ag.Continue(ctx, sink)
}

func compactContext(ctx context.Context, ag *core.Agent, messages []provider.Message, persist func([]provider.Message) error) ([]provider.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	keepTail := min(4, len(messages)-1)
	if _, err := ag.Compact(ctx, keepTail, nil); err != nil {
		return nil, err
	}

	compacted := ag.Messages()
	if err := ctx.Err(); err != nil {
		ag.RestoreMessages(messages)
		return nil, err
	}
	if persist != nil {
		if err := ctx.Err(); err != nil {
			ag.RestoreMessages(messages)
			return nil, err
		}
		if err := persist(compacted); err != nil {
			// The session did not receive the compaction checkpoint. Restore the
			// exact pre-compaction transcript so a later suffix flush cannot mix
			// the old durable generation with the new in-memory generation.
			ag.RestoreMessages(messages)
			return nil, fmt.Errorf("persist compacted transcript: %w", err)
		}
	}
	return compacted, nil
}
