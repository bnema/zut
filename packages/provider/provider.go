// Package provider defines the LLM client abstraction used by zut.
//
// It supports exactly two providers: Anthropic (Messages API) and
// OpenAI (Chat Completions API). Everything above this package operates
// on the types declared here and does not know about HTTP or SSE.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Role is the speaker of a Message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
	// RoleDeveloper is host-authored provider context. Core persists it for
	// replay but presentation layers must not render it as ordinary chat.
	RoleDeveloper Role = "developer"
)

// Content is a block inside a Message. One of TextBlock, ImageBlock,
// ToolCallBlock, or ToolResultBlock.
type Content interface {
	isContent()
}

// TextBlock is plain text content.
type TextBlock struct {
	Text             string `json:"text"`
	ThoughtSignature string `json:"thought_signature,omitempty"`
}

func (TextBlock) isContent() {}

// ImageBlock is an inline image (PNG/JPEG/GIF/WebP).
type ImageBlock struct {
	MimeType         string `json:"mime_type"`
	Data             []byte `json:"data"` // raw bytes; encoded to base64 on the wire
	ThoughtSignature string `json:"thought_signature,omitempty"`
}

func (ImageBlock) isContent() {}

// ToolCallBlock is an assistant-issued call to a tool.
type ToolCallBlock struct {
	ID               string          `json:"id"`
	Name             string          `json:"name"`
	Arguments        json.RawMessage `json:"arguments"`
	ThoughtSignature string          `json:"thought_signature,omitempty"`
	// Origin is host-only routing metadata. It is never serialized to a
	// provider or stored in the transcript.
	Origin string `json:"-"`
}

func (ToolCallBlock) isContent() {}

// ToolTiming records the wall-clock bounds and monotonic duration of a tool
// invocation. It is optional so sessions written before timing was added
// remain valid and compact.
type ToolTiming struct {
	StartedAt   time.Time     `json:"started_at,omitempty"`
	CompletedAt time.Time     `json:"completed_at,omitempty"`
	Duration    time.Duration `json:"duration,omitempty"`
}

// ToolResultBlock is the result of a tool execution, attached to a
// Message with Role == RoleTool.
type ToolResultBlock struct {
	CallID  string      `json:"call_id"`
	Content []Content   `json:"content"`
	IsError bool        `json:"is_error"`
	Timing  *ToolTiming `json:"timing,omitempty"`
}

func (ToolResultBlock) isContent() {}

// ReasoningBlock carries the assistant's chain-of-thought metadata so
// providers that require it on follow-up requests (OpenAI Codex with
// thinking enabled) can replay the same payload they emitted earlier.
// Summary is the human-readable reasoning summary (may be empty); the
// encrypted blob is opaque to zut. ID is the provider-issued reasoning
// item id.
type ReasoningBlock struct {
	ID        string `json:"reasoning_id,omitempty"`
	Summary   string `json:"summary,omitempty"`
	Encrypted string `json:"encrypted_content,omitempty"`
}

func (ReasoningBlock) isContent() {}

// RepairOrphanedToolResults removes tool_result content blocks (and
// entire messages that become empty) when the matching tool_use ID
// does not appear anywhere in the given messages. Resume tails,
// compaction repair, and provider request builders all need this so
// the upstream API never sees a tool_call_id with no corresponding
// assistant tool_call earlier in the same request.
func RepairOrphanedToolResults(msgs []Message) []Message {
	useIDs := map[string]bool{}
	for _, m := range msgs {
		for _, c := range m.Content {
			if tc, ok := c.(ToolCallBlock); ok {
				useIDs[tc.ID] = true
			}
		}
	}
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		var filtered []Content
		for _, c := range m.Content {
			if tr, ok := c.(ToolResultBlock); ok {
				if !useIDs[tr.CallID] {
					continue
				}
			}
			filtered = append(filtered, c)
		}
		if len(filtered) > 0 {
			copy := m
			copy.Content = filtered
			out = append(out, copy)
		}
	}
	return out
}

// Message is a single turn in the conversation.
type Message struct {
	Role           Role              `json:"role"`
	Content        []Content         `json:"content"`
	Time           time.Time         `json:"time"`
	Meta           map[string]string `json:"meta,omitempty"`
	AddedToolNames []string          `json:"added_tool_names,omitempty"`
}

// Tool is a tool definition advertised to the LLM.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"` // JSON Schema for arguments
	// Deferred hides the definition until a tool result activates it.
	Deferred bool `json:"deferred,omitempty"`
}

// activeToolDefinitions returns eager tools plus deferred tools activated by
// prior tool results. Providers with a native position-sensitive deferred-tool
// format can use the activation markers directly instead.
func activeToolDefinitions(tools []Tool, messages []Message) []Tool {
	activated := activatedToolNames(messages)
	active := make([]Tool, 0, len(tools))
	for _, tool := range tools {
		if !tool.Deferred || activated[tool.Name] {
			active = append(active, tool)
		}
	}
	return active
}

func activatedToolNames(messages []Message) map[string]bool {
	activated := make(map[string]bool)
	for _, message := range messages {
		for _, name := range message.AddedToolNames {
			activated[name] = true
		}
	}
	return activated
}

// Usage aggregates token counts and cost for a turn.
type Usage struct {
	InputTokens          int  `json:"input_tokens"`
	OutputTokens         int  `json:"output_tokens"`
	ReasoningTokens      int  `json:"reasoning_tokens"`
	ReasoningTokensKnown bool `json:"reasoning_tokens_known,omitempty"`
	CacheReadTokens      int  `json:"cache_read_tokens"`
	CacheWriteTokens     int  `json:"cache_write_tokens"`
	// CacheMeasuredPromptTokens and CacheMeasuredReadTokens describe the
	// portion of the prompt for which the provider explicitly reported cache
	// details. Zero values mean cache details were unavailable, including in
	// historical session rows.
	CacheMeasuredPromptTokens int     `json:"cache_measured_prompt_tokens,omitempty"`
	CacheMeasuredReadTokens   int     `json:"cache_measured_read_tokens,omitempty"`
	CostUSD                   float64 `json:"cost_usd"`
}

// PromptTokens returns all input tokens, regardless of cache disposition.
func (u Usage) PromptTokens() int {
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// CacheHitRatio returns the share of provider-measured prompt tokens served
// from cache. The boolean is false when cache details were unavailable, which
// keeps historical or unsupported usage distinct from a known zero-percent
// cache hit rate.
func (u Usage) CacheHitRatio() (float64, bool) {
	if u.CacheMeasuredPromptTokens <= 0 {
		return 0, false
	}
	return float64(u.CacheMeasuredReadTokens) / float64(u.CacheMeasuredPromptTokens), true
}

// Add returns u plus v. A reasoning total is known only when both
// component usage reports include a separate reasoning-token count.
func (u Usage) Add(v Usage) Usage {
	return Usage{
		InputTokens:               u.InputTokens + v.InputTokens,
		OutputTokens:              u.OutputTokens + v.OutputTokens,
		ReasoningTokens:           u.ReasoningTokens + v.ReasoningTokens,
		ReasoningTokensKnown:      u.ReasoningTokensKnown && v.ReasoningTokensKnown,
		CacheReadTokens:           u.CacheReadTokens + v.CacheReadTokens,
		CacheWriteTokens:          u.CacheWriteTokens + v.CacheWriteTokens,
		CacheMeasuredPromptTokens: u.CacheMeasuredPromptTokens + v.CacheMeasuredPromptTokens,
		CacheMeasuredReadTokens:   u.CacheMeasuredReadTokens + v.CacheMeasuredReadTokens,
		CostUSD:                   u.CostUSD + v.CostUSD,
	}
}

// StopReason describes why a turn ended.
type StopReason string

const (
	StopEnd     StopReason = "end"
	StopLength  StopReason = "length"
	StopToolUse StopReason = "tool_use"
	StopError   StopReason = "error"
	StopAborted StopReason = "aborted"
)

// Event is one item from a provider stream.
type Event interface {
	isEvent()
}

type EventStart struct {
	Model    string
	Provider string
}

func (EventStart) isEvent() {}

type EventTextDelta struct {
	Delta string
}

func (EventTextDelta) isEvent() {}

type EventToolStart struct {
	ID   string
	Name string
}

func (EventToolStart) isEvent() {}

type EventToolArgs struct {
	ID    string
	Delta string // partial JSON
}

func (EventToolArgs) isEvent() {}

type EventToolEnd struct {
	ID string
}

func (EventToolEnd) isEvent() {}

type EventUsage struct {
	Usage Usage
}

func (EventUsage) isEvent() {}

// EventDone is always the final event on a stream.
type EventDone struct {
	Stop    StopReason
	Err     error
	Message Message // fully assembled assistant message
}

func (EventDone) isEvent() {}

// RequestLifecycle observes request attempts and retry delays without being
// serialized onto a provider wire. Implementations must call RequestAttempt
// immediately before an outbound attempt and RetryScheduled before sleeping.
type RequestLifecycle interface {
	RequestAttempt(attempt, maxAttempts int)
	RetryScheduled(attempt, maxAttempts int, delay time.Duration)
}

// RequestAttemptIDLifecycle optionally receives a provider-generated identity
// for each network attempt. It is deliberately separate from RequestContext:
// SessionID and TurnID remain stable core identities, while request IDs are
// transport-local and must never be serialized into provider payloads.
type RequestAttemptIDLifecycle interface {
	RequestAttemptID(requestID string, attempt, maxAttempts int)
}

// CacheDiagnostics contains only allowlisted cache-routing state. It must not
// contain prompt text, durable identities, request bodies, or credentials.
type CacheDiagnostics struct {
	Eligible     bool
	Mode         string
	Transport    string
	Continuation string
}

// RequestCacheDiagnosticsLifecycle receives sanitized cache-routing state when
// an adapter can determine it. It is optional to preserve existing clients.
type RequestCacheDiagnosticsLifecycle interface {
	CacheDiagnostics(CacheDiagnostics)
}

func ReportCacheDiagnostics(lifecycle RequestLifecycle, diagnostics CacheDiagnostics) {
	if withDiagnostics, ok := lifecycle.(RequestCacheDiagnosticsLifecycle); ok {
		withDiagnostics.CacheDiagnostics(diagnostics)
	}
}

// RequestFailureReason is an allowlisted request-failure category. It is safe
// to persist; raw provider errors and response bodies are not.
type RequestFailureReason string

const (
	RequestFailureOverload  RequestFailureReason = "overload"
	RequestFailureRateLimit RequestFailureReason = "rate_limit"
	RequestFailureQuota     RequestFailureReason = "quota"
	RequestFailureServer    RequestFailureReason = "server"
	RequestFailureNetwork   RequestFailureReason = "network"
	RequestFailureTimeout   RequestFailureReason = "timeout"
	RequestFailureClient    RequestFailureReason = "client"
	RequestFailureUnknown   RequestFailureReason = "unknown"
)

// RequestFailureLifecycle optionally augments RequestLifecycle with sanitized
// failed-attempt metadata. Keeping it separate preserves existing lifecycle
// implementations.
type RequestFailureLifecycle interface {
	RequestFailed(attempt, maxAttempts int, reason RequestFailureReason, terminal bool)
}

// RequestContext identifies the cache affinity, conversation thread, and
// accepted turn for one logical agent request. Values are opaque
// provider-neutral correlation IDs. Providers may translate them into cache
// controls or routing fields, but must not replace them with transport-attempt
// IDs.
type RequestContext struct {
	CacheSessionID string
	ThreadID       string
	TurnID         string
}

// Request is a single LLM call.
type Request struct {
	Model         string
	System        string
	SystemContext string
	Messages      []Message
	Tools         []Tool
	MaxTokens     int
	Temperature   *float32
	// Reasoning is "", "minimum", "low", "medium", "high", "xhigh", or
	// "max". Empty disables reasoning. The max tier is sent natively only to
	// models that support it and clamped elsewhere.
	Reasoning string
	// FastMode requests OpenAI's fast service tier. Providers that do not
	// implement the OpenAI service-tier contract reject the request.
	FastMode bool
	// Lifecycle receives in-memory request-attempt notifications. It is not
	// included in any provider request payload.
	Lifecycle RequestLifecycle
	// Context carries the stable logical session and turn identities for this
	// request. Provider clients own any per-network-attempt identity.
	Context RequestContext
}

// SystemPrompt returns the stable system prompt plus optional model-only
// session context without requiring callers to mutate their system prompt.
func (r Request) SystemPrompt() string {
	if r.SystemContext == "" {
		return r.System
	}
	if r.System == "" {
		return r.SystemContext
	}
	return r.System + "\n\n" + r.SystemContext
}

// OpenAI's current Fast mode is represented as the priority service tier on
// the wire, including the ChatGPT/Codex Responses endpoint.
const fastModeServiceTier = "priority"

// SupportsFastMode reports whether a provider uses an OpenAI request
// contract that supports the fast service tier.
func SupportsFastMode(providerName string) bool {
	switch strings.ToLower(strings.TrimSpace(providerName)) {
	case "openai", "openai-codex", "openai-responses":
		return true
	default:
		return false
	}
}

// ValidateFastMode rejects fast-mode requests before a provider performs
// any network or credential work. Keep this check at the provider boundary
// so direct Client users get the same contract as the core agent loop.
func ValidateFastMode(providerName string, enabled bool) error {
	if !enabled || SupportsFastMode(providerName) {
		return nil
	}
	return fmt.Errorf("fast mode is only supported for OpenAI providers, not %q", providerName)
}

// Client is an LLM streaming client.
type Client interface {
	// Name returns "anthropic" or "openai".
	Name() string
	// Stream starts a request. The returned channel delivers events
	// and is closed after EventDone. Errors during request setup are
	// returned directly; runtime errors arrive as EventDone{Err: ...}.
	Stream(ctx context.Context, req Request) (<-chan Event, error)
}

// ClientCloser is implemented by clients that retain resources beyond one
// Stream call. Hosts with an explicit runtime lifetime should close it when
// that runtime stops. Most HTTP/SSE clients need no cleanup.
type ClientCloser interface {
	Close() error
}
