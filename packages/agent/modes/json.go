package modes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// RunJSON runs the agent to completion, writing one JSON object per
// AgentEvent as newline-delimited JSON.
func RunJSON(ctx context.Context, ag *core.Agent, prompt string, images []provider.ImageBlock, out io.Writer) error {
	_, err := RunJSONWithContextRecovery(ctx, ag, prompt, images, out, nil)
	return err
}

// RunJSONWithContextRecovery is RunJSON with an optional checkpoint writer
// for a one-shot context-overflow compaction. It suppresses the recoverable
// first turn_end so stdout never contains a terminal error frame before the
// continued turn succeeds. OutputStart identifies the suffix to persist after
// a successful checkpoint.
func RunJSONWithContextRecovery(ctx context.Context, ag *core.Agent, prompt string, images []provider.ImageBlock, out io.Writer, persistCompaction func([]provider.Message) error) (ContextRecoveryResult, error) {
	enc := json.NewEncoder(out)
	var writeMu sync.Mutex
	var writeErr error
	write := func(v any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		if writeErr == nil {
			writeErr = enc.Encode(v)
		}
	}

	var runErr error
	sink := func(ev core.AgentEvent) {
		write(EventToJSON(ev))
	}

	recovery, err := PromptWithContextRecovery(ctx, ag, prompt, images, sink, ContextRecoveryOptions{
		PersistCompaction:            persistCompaction,
		SuppressInitialOverflowEvent: true,
	})
	if err != nil {
		runErr = err
	}

	if runErr != nil {
		write(map[string]any{"type": "error", "message": runErr.Error()})
	}
	writeMu.Lock()
	outputErr := writeErr
	writeMu.Unlock()
	return recovery, errors.Join(runErr, outputErr)
}

// EventToJSON converts an AgentEvent to a JSON-friendly map. The on-wire
// schema is deliberately simple and flat. Exported so the RPC mode can
// reuse the same serialisation as `zut --json`.
func EventToJSON(ev core.AgentEvent) map[string]any {
	m := map[string]any{"type": ev.Type()}
	switch e := ev.(type) {
	case core.EvTurnStart:
		m["step"] = e.Step
	case core.EvRequestStarted:
		m["provider"] = e.Provider
		m["model"] = e.Model
		m["scope"] = string(e.Scope)
		m["attempt"] = e.Attempt
		m["max_attempts"] = e.MaxAttempts
	case core.EvRetryScheduled:
		m["scope"] = string(e.Scope)
		m["attempt"] = e.Attempt
		m["max_attempts"] = e.MaxAttempts
		m["delay_ms"] = e.Delay.Milliseconds()
	case core.EvUserMessage:
		m["content"] = ContentToJSON(e.Message.Content)
		m["time"] = e.Message.Time
	case core.EvAssistantMessage:
		m["content"] = ContentToJSON(e.Message.Content)
		m["time"] = e.Message.Time
	case core.EvTextDelta:
		m["delta"] = e.Delta
	case core.EvToolUseStart:
		m["id"] = e.ID
		m["name"] = e.Name
	case core.EvToolUseArgs:
		m["id"] = e.ID
		m["delta"] = e.Delta
	case core.EvToolUseEnd:
		m["id"] = e.ID
	case core.EvToolCall:
		m["id"] = e.ID
		m["name"] = e.Name
		var args any
		_ = json.Unmarshal(e.Args, &args)
		m["args"] = args
	case core.EvToolExecutionStarted:
		m["id"] = e.ID
		m["name"] = e.Name
	case core.EvToolProgress:
		m["id"] = e.ID
		m["text"] = e.Text
	case core.EvToolResult:
		m["id"] = e.ID
		m["is_error"] = e.Result.IsError
		m["content"] = ContentToJSON(e.Result.Content)
	case core.EvUsage:
		m["input"] = e.Usage.InputTokens
		m["output"] = e.Usage.OutputTokens
		m["reasoning"] = nullableReasoningTokens(e.Usage)
		m["cache_read"] = e.Usage.CacheReadTokens
		m["cache_write"] = e.Usage.CacheWriteTokens
		m["cost_usd"] = e.Usage.CostUSD
		m["cumulative"] = map[string]any{
			"input":       e.Cumulative.InputTokens,
			"output":      e.Cumulative.OutputTokens,
			"reasoning":   nullableReasoningTokens(e.Cumulative),
			"cache_read":  e.Cumulative.CacheReadTokens,
			"cache_write": e.Cumulative.CacheWriteTokens,
			"cost_usd":    e.Cumulative.CostUSD,
		}
	case core.EvTurnEnd:
		m["stop"] = string(e.Stop)
		if e.Err != nil {
			m["error"] = e.Err.Error()
		}
	}
	return m
}

// ContentToJSON serialises a transcript content slice into the same
// shape used in EventToJSON. Exported alongside EventToJSON for the
// RPC mode.
func ContentToJSON(blocks []provider.Content) []map[string]any {
	out := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		switch v := b.(type) {
		case provider.TextBlock:
			out = append(out, map[string]any{"type": "text", "text": v.Text})
		case provider.ImageBlock:
			out = append(out, map[string]any{"type": "image", "mime_type": v.MimeType, "bytes": len(v.Data)})
		case provider.ToolCallBlock:
			var args any
			_ = json.Unmarshal(v.Arguments, &args)
			out = append(out, map[string]any{"type": "tool_call", "id": v.ID, "name": v.Name, "args": args})
		case provider.ToolResultBlock:
			out = append(out, map[string]any{
				"type":     "tool_result",
				"call_id":  v.CallID,
				"is_error": v.IsError,
				"content":  ContentToJSON(v.Content),
			})
		}
	}
	return out
}

func nullableReasoningTokens(usage provider.Usage) any {
	if !usage.ReasoningTokensKnown {
		return nil
	}
	return usage.ReasoningTokens
}
