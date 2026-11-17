package sdk

import (
	"encoding/json"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// Event is the unit emitted by Runtime.Prompt's channel. Type
// identifies the event kind; remaining fields are populated based on
// kind. The wire schema is the same as the JSON-RPC stream notifications
// emitted by `zut rpc`, so consumers can share parsing code.
type Event struct {
	Type string `json:"type"`

	// Common fields.
	Step int    `json:"step,omitempty"`
	Time string `json:"time,omitempty"`

	// request_started / retry_scheduled
	Provider    string `json:"provider,omitempty"`
	Model       string `json:"model,omitempty"`
	Scope       string `json:"scope,omitempty"`
	Attempt     int    `json:"attempt,omitempty"`
	MaxAttempts int    `json:"max_attempts,omitempty"`
	// DelayMS is set for retry_scheduled, including a valid zero delay.
	DelayMS *int64 `json:"delay_ms,omitempty"`
	// Reason is an optional user-visible retry reason for retry_scheduled.
	Reason string `json:"reason,omitempty"`

	// text_delta
	Delta string `json:"delta,omitempty"`

	// tool_call
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`

	// nested_tool_started / nested_tool_result (experimental codemode)
	ParentID string `json:"parent_id,omitempty"`
	Denied   bool   `json:"denied,omitempty"`
	Summary  string `json:"summary,omitempty"`

	// tool_progress; server_tool plaintext result
	Text string `json:"text,omitempty"`

	// server_tool: a provider-executed tool such as the native advisor.
	// Phase is "start" or "result". ErrorCode is set on failure; Redacted
	// marks advice the provider returned encrypted.
	Phase     string `json:"phase,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
	Redacted  bool   `json:"redacted,omitempty"`

	// tool_result
	IsError bool           `json:"is_error,omitempty"`
	Result  []ContentBlock `json:"content,omitempty"`

	// plan_update
	Explanation *string     `json:"explanation,omitempty"`
	Plan        *[]PlanStep `json:"plan,omitempty"`

	// usage
	Usage      *Usage `json:"usage,omitempty"`
	Cumulative *Usage `json:"cumulative,omitempty"`
	// External marks usage spent by a tool (for example an advisor model).
	// It counts toward cost but is not the current context size.
	External bool `json:"external,omitempty"`

	// cache_diagnostics
	CacheDiagnostics *CacheDiagnostics `json:"cache_diagnostics,omitempty"`

	// user_message / assistant_message
	Message *Message `json:"message,omitempty"`

	// repetition_guard
	RepetitionKind    string `json:"kind,omitempty"`
	RepetitionStage   string `json:"stage,omitempty"`
	RepetitionCount   int    `json:"count,omitempty"`
	RepetitionElapsed int64  `json:"elapsed_ms,omitempty"`

	// turn_end
	Stop string `json:"stop,omitempty"`

	// error
	Error string `json:"error,omitempty"`
}

// PlanStep is one model-maintained task in a plan update.
type PlanStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

// Message is one transcript entry — a user prompt, assistant reply,
// or tool result.
type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
	Time    string         `json:"time,omitempty"`
}

// ContentBlock is one piece of message content. Discriminate on Type:
//   - "text"        → Text
//   - "image"       → MimeType + Data (or Bytes for size-only)
//   - "tool_call"   → ID + Name + Args
//   - "tool_result" → CallID + IsError + Content (recursive)
//   - "server_tool" → Kind + ID + Name, optional AdvisorText / Redacted /
//     ErrorCode. The provider's opaque payload is never exposed, so these
//     blocks are display-only and are not replayed.
type ContentBlock struct {
	Type string `json:"type"`

	Text     string          `json:"text,omitempty"`
	MimeType string          `json:"mime_type,omitempty"`
	Data     []byte          `json:"data,omitempty"`
	Bytes    int             `json:"bytes,omitempty"`
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name,omitempty"`
	Args     json.RawMessage `json:"args,omitempty"`
	CallID   string          `json:"call_id,omitempty"`
	IsError  bool            `json:"is_error,omitempty"`
	Content  []ContentBlock  `json:"content,omitempty"`

	// server_tool
	Kind        string `json:"kind,omitempty"`
	AdvisorText string `json:"advisor_text,omitempty"`
	Redacted    bool   `json:"redacted,omitempty"`
	ErrorCode   string `json:"error_code,omitempty"`
}

// Image is a single user-attached image for Prompt.
type Image struct {
	MimeType string
	Data     []byte
}

// CacheDiagnostics reports sanitized provider cache-routing state.
type CacheDiagnostics struct {
	Eligible     bool   `json:"eligible"`
	Mode         string `json:"mode"`
	Transport    string `json:"transport"`
	Continuation string `json:"continuation"`
}

// Usage is per-turn or cumulative token / cost counts.
type Usage struct {
	Input               int     `json:"input"`
	Output              int     `json:"output"`
	Reasoning           *int    `json:"reasoning"`
	CacheRead           int     `json:"cache_read"`
	CacheWrite          int     `json:"cache_write"`
	CacheMeasuredPrompt int     `json:"cache_measured_prompt,omitempty"`
	CacheMeasuredRead   int     `json:"cache_measured_read,omitempty"`
	CostUSD             float64 `json:"cost_usd"`
}

// State is a snapshot of the runtime's current state.
type State struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Reasoning    string `json:"reasoning"`
	CWD          string `json:"cwd"`
	Busy         bool   `json:"busy"`
	MessageCount int    `json:"message_count"`
}

// CompactResult describes the outcome of Compact.
type CompactResult struct {
	Summary  string    `json:"summary"`
	Messages []Message `json:"messages"`
}

// ModelInfo describes one model known to the runtime.
type ModelInfo struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	ContextWindow int    `json:"context_window"`
	MaxOutput     int    `json:"max_output"`
	Reasoning     bool   `json:"reasoning"`
}

// ---- internal converters ----

func sdkReasoningTokens(usage provider.Usage) *int {
	if !usage.ReasoningTokensKnown {
		return nil
	}
	count := usage.ReasoningTokens
	return &count
}

func toEvent(ev core.AgentEvent) Event {
	out := Event{Type: ev.Type()}
	switch e := ev.(type) {
	case core.EvTurnStart:
		out.Step = e.Step
	case core.EvRequestStarted:
		out.Provider = e.Provider
		out.Model = e.Model
		out.Scope = string(e.Scope)
		out.Attempt = e.Attempt
		out.MaxAttempts = e.MaxAttempts
	case core.EvRetryScheduled:
		out.Scope = string(e.Scope)
		out.Attempt = e.Attempt
		out.MaxAttempts = e.MaxAttempts
		delayMS := e.Delay.Milliseconds()
		out.DelayMS = &delayMS
		out.Reason = e.Reason
	case core.EvUserMessage:
		out.Message = &Message{Role: string(e.Message.Role), Content: convertContent(e.Message.Content)}
	case core.EvAssistantMessage:
		out.Message = &Message{Role: string(e.Message.Role), Content: convertContent(e.Message.Content)}
	case core.EvTextDelta:
		out.Delta = e.Delta
	case core.EvToolUseStart:
		out.ID = e.ID
		out.Name = e.Name
	case core.EvToolUseArgs:
		out.ID = e.ID
		out.Delta = e.Delta
	case core.EvToolUseEnd:
		out.ID = e.ID
	case core.EvServerTool:
		out.ID, out.Name, out.Phase = e.ID, e.Name, e.Phase
		out.Text, out.ErrorCode, out.Redacted = e.Text, e.ErrorCode, e.Redacted
	case core.EvToolCall:
		out.ID = e.ID
		out.Name = e.Name
		out.Args = e.Args
	case core.EvNestedToolStarted:
		out.ID, out.ParentID, out.Name = e.ID, e.ParentID, e.Name
		out.Args = e.Args
	case core.EvNestedToolResult:
		out.ID, out.ParentID, out.Name = e.ID, e.ParentID, e.Name
		out.IsError, out.Denied, out.Summary = e.IsError, e.Denied, e.Summary
	case core.EvToolExecutionStarted:
		out.ID = e.ID
		out.Name = e.Name
	case core.EvToolProgress:
		out.ID = e.ID
		out.Text = e.Text
	case core.EvPlanUpdate:
		out.ID = e.CallID
		out.Explanation = e.Update.Explanation
		plan := make([]PlanStep, len(e.Update.Plan))
		for idx, step := range e.Update.Plan {
			plan[idx] = PlanStep{Step: step.Step, Status: string(step.Status)}
		}
		out.Plan = &plan
	case core.EvToolResult:
		out.ID = e.ID
		out.IsError = e.Result.IsError
		out.Result = convertContent(e.Result.Content)
	case core.EvCacheDiagnostics:
		out.CacheDiagnostics = &CacheDiagnostics{
			Eligible:     e.Eligible,
			Mode:         e.Mode,
			Transport:    e.Transport,
			Continuation: e.Continuation,
		}
	case core.EvUsage:
		out.Usage = &Usage{
			Input:               e.Usage.InputTokens,
			Output:              e.Usage.OutputTokens,
			Reasoning:           sdkReasoningTokens(e.Usage),
			CacheRead:           e.Usage.CacheReadTokens,
			CacheWrite:          e.Usage.CacheWriteTokens,
			CacheMeasuredPrompt: e.Usage.CacheMeasuredPromptTokens,
			CacheMeasuredRead:   e.Usage.CacheMeasuredReadTokens,
			CostUSD:             e.Usage.CostUSD,
		}
		out.External = e.External
		out.Cumulative = &Usage{
			Input:               e.Cumulative.InputTokens,
			Output:              e.Cumulative.OutputTokens,
			Reasoning:           sdkReasoningTokens(e.Cumulative),
			CacheRead:           e.Cumulative.CacheReadTokens,
			CacheWrite:          e.Cumulative.CacheWriteTokens,
			CacheMeasuredPrompt: e.Cumulative.CacheMeasuredPromptTokens,
			CacheMeasuredRead:   e.Cumulative.CacheMeasuredReadTokens,
			CostUSD:             e.Cumulative.CostUSD,
		}
	case core.EvRepetitionGuard:
		out.RepetitionKind = string(e.Kind)
		out.RepetitionStage = string(e.Stage)
		out.RepetitionCount = e.Count
		out.RepetitionElapsed = e.Elapsed.Milliseconds()
	case core.EvTurnEnd:
		out.Stop = string(e.Stop)
		if e.Err != nil {
			out.Error = e.Err.Error()
		}
	case core.EvError:
		if e.Err != nil {
			out.Error = e.Err.Error()
		}
	}
	return out
}

func convertContent(blocks []provider.Content) []ContentBlock {
	out := make([]ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		switch v := b.(type) {
		case provider.TextBlock:
			out = append(out, ContentBlock{Type: "text", Text: v.Text})
		case provider.ImageBlock:
			out = append(out, ContentBlock{Type: "image", MimeType: v.MimeType, Bytes: len(v.Data)})
		case provider.ToolCallBlock:
			out = append(out, ContentBlock{Type: "tool_call", ID: v.ID, Name: v.Name, Args: v.Arguments})
		case provider.ToolResultBlock:
			out = append(out, ContentBlock{
				Type:    "tool_result",
				CallID:  v.CallID,
				IsError: v.IsError,
				Content: convertContent(v.Content),
			})
		case provider.ServerToolBlock:
			out = append(out, ContentBlock{
				Type: "server_tool", Kind: v.Kind, ID: v.ID, Name: v.Name,
				AdvisorText: v.AdvisorText, Redacted: v.Redacted, ErrorCode: v.ErrorCode,
			})
		}
	}
	return out
}

func rebuildContent(blocks []ContentBlock) []provider.Content {
	out := make([]provider.Content, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, provider.TextBlock{Text: b.Text})
		case "image":
			out = append(out, provider.ImageBlock{MimeType: b.MimeType, Data: b.Data})
		case "tool_call":
			args := b.Args
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			out = append(out, provider.ToolCallBlock{ID: b.ID, Name: b.Name, Arguments: args})
		case "tool_result":
			out = append(out, provider.ToolResultBlock{
				CallID:  b.CallID,
				IsError: b.IsError,
				Content: rebuildContent(b.Content),
			})
		}
	}
	return out
}
