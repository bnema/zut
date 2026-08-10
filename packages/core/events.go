package core

import (
	"encoding/json"
	"time"

	"github.com/bnema/zut/packages/provider"
)

// AgentEvent is the superset of events emitted by an Agent run.
// Consumers discriminate via a type switch. Each concrete type has a
// Type() method for JSON serialization.
type AgentEvent interface {
	Type() string
}

type EvTurnStart struct {
	Step int
}

func (EvTurnStart) Type() string { return "turn_start" }

type EvUserMessage struct {
	Message provider.Message
}

func (EvUserMessage) Type() string { return "user_message" }

type EvAssistantStart struct{}

func (EvAssistantStart) Type() string { return "assistant_start" }

// RetryScope identifies the retry layer that issued an activity event.
type RetryScope string

const (
	RetryScopeProvider RetryScope = "provider"
	RetryScopeAgent    RetryScope = "agent"
)

// RetryLifecycleEvent identifies a sanitized retry audit record.
type RetryLifecycleEvent string

const (
	RetryLifecycleRequestFailed  RetryLifecycleEvent = "request_failed"
	RetryLifecycleRetryScheduled RetryLifecycleEvent = "retry_scheduled"
)

// RetryReason is an allowlisted failure category safe for session persistence.
type RetryReason string

const (
	RetryReasonOverload      RetryReason = "overload"
	RetryReasonRateLimit     RetryReason = "rate_limit"
	RetryReasonQuota         RetryReason = "quota"
	RetryReasonServer        RetryReason = "server"
	RetryReasonNetwork       RetryReason = "network"
	RetryReasonTimeout       RetryReason = "timeout"
	RetryReasonAuth          RetryReason = "auth"
	RetryReasonContextWindow RetryReason = "context_window"
	RetryReasonClient        RetryReason = "client"
	RetryReasonUnknown       RetryReason = "unknown"
)

// RetryLifecycleRecord contains only bounded, privacy-safe retry metadata.
// Raw provider errors and response bodies must never be added to this type.
type RetryLifecycleRecord struct {
	Event       RetryLifecycleEvent `json:"event"`
	Scope       RetryScope          `json:"scope"`
	Attempt     int                 `json:"attempt"`
	MaxAttempts int                 `json:"max_attempts"`
	Reason      RetryReason         `json:"reason"`
	Terminal    bool                `json:"terminal,omitempty"`
	DelayMS     int64               `json:"delay_ms,omitempty"`
}

// EvRequestStarted reports an outbound provider request attempt.
type EvRequestStarted struct {
	Provider    string
	Model       string
	Scope       RetryScope
	Attempt     int
	MaxAttempts int
}

func (EvRequestStarted) Type() string { return "request_started" }

// EvRetryScheduled reports a retry delay before its upcoming attempt.
type EvRetryScheduled struct {
	Scope       RetryScope
	Attempt     int
	MaxAttempts int
	Delay       time.Duration
}

func (EvRetryScheduled) Type() string { return "retry_scheduled" }

type EvTextDelta struct {
	Delta string
}

func (EvTextDelta) Type() string { return "text_delta" }

type EvToolCall struct {
	ID   string
	Name string
	Args json.RawMessage
}

func (EvToolCall) Type() string { return "tool_call" }

// EvToolExecutionStarted fires after guards and confirmation allow a call,
// immediately before the tool starts executing.
type EvToolExecutionStarted struct {
	ID   string
	Name string
}

func (EvToolExecutionStarted) Type() string { return "tool_execution_started" }

// EvToolUseStart fires the moment the provider announces a new
// tool_use block during streaming, before any arg JSON has
// arrived. Gives UIs a hook to pre-render a live "tool is being
// composed" panel so the user sees the model typing the call in
// real time. Name is already final at this point; Args is empty.
type EvToolUseStart struct {
	ID   string
	Name string
}

func (EvToolUseStart) Type() string { return "tool_use_start" }

// EvToolUseArgs fires for each delta fragment of the tool_use
// block's argument JSON. Concatenating every delta for a given
// ID produces the full JSON string; during streaming it's likely
// truncated mid-value. UIs can extract partial string fields
// (e.g. the `content` arg of `write`) with an escape-aware scan.
type EvToolUseArgs struct {
	ID    string
	Delta string
}

func (EvToolUseArgs) Type() string { return "tool_use_args" }

// EvToolUseEnd fires when the provider marks the tool_use block
// complete. At this point the full args JSON is known; a separate
// EvToolCall follows once the assistant message is assembled,
// carrying the parsed block that actually runs.
type EvToolUseEnd struct {
	ID string
}

func (EvToolUseEnd) Type() string { return "tool_use_end" }

type EvToolProgress struct {
	ID   string
	Text string
}

func (EvToolProgress) Type() string { return "tool_progress" }

type EvToolResult struct {
	ID     string
	Result ToolResult
}

func (EvToolResult) Type() string { return "tool_result" }

type EvUsage struct {
	Usage      provider.Usage
	Cumulative provider.Usage
}

func (EvUsage) Type() string { return "usage" }

type EvAssistantMessage struct {
	Message provider.Message
}

func (EvAssistantMessage) Type() string { return "assistant_message" }

type EvTurnEnd struct {
	Stop provider.StopReason
	Err  error
}

func (EvTurnEnd) Type() string { return "turn_end" }

type EvDone struct{}

func (EvDone) Type() string { return "done" }

type EvError struct {
	Err error
}

func (EvError) Type() string { return "error" }
