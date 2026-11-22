package daemonproto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"reflect"
)

func eventProjection(ev core.AgentEvent) map[string]any {
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
		if e.Reason != "" {
			m["reason"] = e.Reason
		}
	case core.EvUserMessage:
		m["content"] = contentProjection(e.Message.Content)
		m["time"] = e.Message.Time
		if e.Message.Meta[core.WatcherNoteMetaKey] == "true" {
			m["advisor_note"] = true
		}
	case core.EvAssistantMessage:
		m["content"] = contentProjection(e.Message.Content)
		m["time"] = e.Message.Time
	case core.EvCacheDiagnostics:
		m["eligible"] = e.Eligible
		m["mode"] = e.Mode
		m["transport"] = e.Transport
		m["continuation"] = e.Continuation
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
	case core.EvAdvisorReview:
		m["kind"] = e.Kind
		m["phase"] = e.Phase
	case core.EvServerTool:
		m["id"] = e.ID
		m["name"] = e.Name
		m["phase"] = e.Phase
		if e.Text != "" {
			m["text"] = e.Text
		}
		if e.ErrorCode != "" {
			m["error_code"] = e.ErrorCode
		}
		if e.Redacted {
			m["redacted"] = true
		}
	case core.EvToolCall:
		m["id"] = e.ID
		m["name"] = e.Name
		var args any
		_ = json.Unmarshal(e.Args, &args)
		m["args"] = args
	case core.EvNestedToolStarted:
		m["id"] = e.ID
		m["parent_id"] = e.ParentID
		m["name"] = e.Name
		var args any
		_ = json.Unmarshal(e.Args, &args)
		m["args"] = args
	case core.EvNestedToolResult:
		m["id"] = e.ID
		m["parent_id"] = e.ParentID
		m["name"] = e.Name
		m["is_error"] = e.IsError
		m["denied"] = e.Denied
		if e.Summary != "" {
			m["summary"] = e.Summary
		}
	case core.EvToolExecutionStarted:
		m["id"] = e.ID
		m["name"] = e.Name
	case core.EvToolProgress:
		m["id"] = e.ID
		m["text"] = e.Text
	case core.EvPlanUpdate:
		// Omit an empty call id, matching the SDK's omitempty so both wire
		// projections agree.
		if e.CallID != "" {
			m["id"] = e.CallID
		}
		if e.Update.Explanation != nil {
			m["explanation"] = *e.Update.Explanation
		} else {
			m["explanation"] = nil
		}
		m["plan"] = e.Update.Plan
	case core.EvToolResult:
		m["id"] = e.ID
		m["is_error"] = e.Result.IsError
		m["content"] = contentProjection(e.Result.Content)
	case core.EvUsage:
		m["input"] = e.Usage.InputTokens
		m["output"] = e.Usage.OutputTokens
		m["reasoning"] = nullableReasoningTokens(e.Usage)
		m["cache_read"] = e.Usage.CacheReadTokens
		m["cache_write"] = e.Usage.CacheWriteTokens
		m["cache_measured_prompt"] = e.Usage.CacheMeasuredPromptTokens
		m["cache_measured_read"] = e.Usage.CacheMeasuredReadTokens
		m["cost_usd"] = e.Usage.CostUSD
		if e.External {
			m["external"] = true
		}
		m["cumulative"] = map[string]any{
			"input":                 e.Cumulative.InputTokens,
			"output":                e.Cumulative.OutputTokens,
			"reasoning":             nullableReasoningTokens(e.Cumulative),
			"cache_read":            e.Cumulative.CacheReadTokens,
			"cache_write":           e.Cumulative.CacheWriteTokens,
			"cache_measured_prompt": e.Cumulative.CacheMeasuredPromptTokens,
			"cache_measured_read":   e.Cumulative.CacheMeasuredReadTokens,
			"cost_usd":              e.Cumulative.CostUSD,
		}
	case core.EvTurnRecovery:
		m["reason"] = e.Reason
		m["attempt"] = e.Attempt
	case core.EvRepetitionGuard:
		m["kind"] = e.Kind
		m["stage"] = e.Stage
		m["count"] = e.Count
		m["elapsed_ms"] = e.Elapsed.Milliseconds()
	case core.EvTurnEnd:
		m["stop"] = string(e.Stop)
		if e.Err != nil {
			m["error"] = e.Err.Error()
		}
	}
	return m
}

// contentProjection serialises a transcript content slice into the same
// shape used in eventProjection. Exported alongside eventProjection for the
// RPC mode.
func contentProjection(blocks []provider.Content) []map[string]any {
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
				"content":  contentProjection(v.Content),
			})
		case provider.ServerToolBlock:
			// Never project Raw: it is an opaque, possibly encrypted blob.
			sm := map[string]any{"type": "server_tool", "kind": v.Kind, "id": v.ID, "name": v.Name}
			if v.AdvisorText != "" {
				sm["advisor_text"] = v.AdvisorText
			}
			if v.Redacted {
				sm["redacted"] = true
			}
			if v.ErrorCode != "" {
				sm["error_code"] = v.ErrorCode
			}
			out = append(out, sm)
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

// EncodeAgentEvent extends the existing headless projection with a typed value
// for fields the display projection omits. Errors preserve their text and
// cancellation identity. Durations in the projection are milliseconds.
func EncodeAgentEvent(ev core.AgentEvent) (string, json.RawMessage, error) {
	if ev == nil {
		return "", nil, fmt.Errorf("nil agent event")
	}
	if v := reflect.ValueOf(ev); v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "", nil, fmt.Errorf("nil agent event")
		}
		var ok bool
		ev, ok = v.Elem().Interface().(core.AgentEvent)
		if !ok {
			return "", nil, fmt.Errorf("unsupported agent event")
		}
	}
	m := eventProjection(ev)
	var value any = ev
	switch e := ev.(type) {
	case core.EvTurnEnd:
		var msg *string
		if e.Err != nil {
			s := e.Err.Error()
			msg = &s
		}
		value = struct {
			Stop     provider.StopReason
			Error    *string
			Canceled bool `json:",omitempty"`
		}{e.Stop, msg, errors.Is(e.Err, context.Canceled)}
	case core.EvError:
		var msg *string
		if e.Err != nil {
			s := e.Err.Error()
			msg = &s
		}
		value = struct {
			Error    *string
			Canceled bool `json:",omitempty"`
		}{msg, errors.Is(e.Err, context.Canceled)}
		if msg != nil {
			m["message"] = *msg
		}
	}
	raw, err := marshalPayload(value)
	if err != nil {
		return "", nil, err
	}
	m["value"] = raw
	if _, err = agentTarget(ev.Type()); err != nil {
		return "", nil, err
	}
	b, err := json.Marshal(m)
	return ev.Type(), b, err
}

type unknownAgentEvent string

func (e unknownAgentEvent) Type() string { return string(e) }

// DecodeAgentEvent preserves unknown kinds as ignorable events, retaining sequence progress.
func DecodeAgentEvent(kind string, data json.RawMessage) (core.AgentEvent, error) {
	p, targetErr := agentTarget(kind)
	var err error
	var w struct {
		Type  string          `json:"type"`
		Value json.RawMessage `json:"value"`
	}
	if err = json.Unmarshal(data, &w); err != nil {
		return nil, err
	}
	if w.Type != kind {
		return nil, fmt.Errorf("agent event kind mismatch")
	}
	if targetErr != nil {
		return unknownAgentEvent(kind), nil
	}
	if len(w.Value) == 0 {
		return nil, fmt.Errorf("missing lossless agent value")
	}
	switch kind {
	case "turn_end", "error":
		var v struct {
			Stop     provider.StopReason
			Error    *string
			Canceled bool
		}
		if err = json.Unmarshal(w.Value, &v); err != nil {
			return nil, err
		}
		var e error
		if v.Error != nil {
			e = errors.New(*v.Error)
			if v.Canceled {
				e = canceledEventError{message: *v.Error}
			}
		}
		if kind == "turn_end" {
			return core.EvTurnEnd{Stop: v.Stop, Err: e}, nil
		}
		return core.EvError{Err: e}, nil
	}
	if err = unmarshalPayload(w.Value, p); err != nil {
		return nil, err
	}
	return reflect.ValueOf(p).Elem().Interface().(core.AgentEvent), nil
}
func agentTarget(kind string) (any, error) {
	switch kind {
	case "turn_start":
		return &core.EvTurnStart{}, nil
	case "user_message":
		return &core.EvUserMessage{}, nil
	case "assistant_start":
		return &core.EvAssistantStart{}, nil
	case "request_started":
		return &core.EvRequestStarted{}, nil
	case "cache_diagnostics":
		return &core.EvCacheDiagnostics{}, nil
	case "retry_scheduled":
		return &core.EvRetryScheduled{}, nil
	case "text_delta":
		return &core.EvTextDelta{}, nil
	case "tool_call":
		return &core.EvToolCall{}, nil
	case "nested_tool_started":
		return &core.EvNestedToolStarted{}, nil
	case "nested_tool_result":
		return &core.EvNestedToolResult{}, nil
	case "tool_execution_started":
		return &core.EvToolExecutionStarted{}, nil
	case "tool_use_start":
		return &core.EvToolUseStart{}, nil
	case "tool_use_args":
		return &core.EvToolUseArgs{}, nil
	case "tool_use_end":
		return &core.EvToolUseEnd{}, nil
	case "server_tool":
		return &core.EvServerTool{}, nil
	case "advisor_review":
		return &core.EvAdvisorReview{}, nil
	case "tool_progress":
		return &core.EvToolProgress{}, nil
	case "plan_update":
		return &core.EvPlanUpdate{}, nil
	case "tool_result":
		return &core.EvToolResult{}, nil
	case "usage":
		return &core.EvUsage{}, nil
	case "assistant_message":
		return &core.EvAssistantMessage{}, nil
	case "turn_end":
		return &core.EvTurnEnd{}, nil
	case "turn_recovery":
		return &core.EvTurnRecovery{}, nil
	case "repetition_guard":
		return &core.EvRepetitionGuard{}, nil
	case "done":
		return &core.EvDone{}, nil
	case "error":
		return &core.EvError{}, nil
	default:
		return nil, fmt.Errorf("unknown agent event %q", kind)
	}
}

// canceledEventError retains diagnostic text while restoring errors.Is semantics.
type canceledEventError struct{ message string }

func (e canceledEventError) Error() string { return e.message }
func (e canceledEventError) Unwrap() error { return context.Canceled }
