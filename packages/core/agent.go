package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bnema/zut/packages/provider"
)

var ErrStreamIdleTimeout = errors.New("provider stream idle timeout")

type queuedMessage struct {
	text     string
	accepted time.Time
}

// Agent is a stateful conversation bound to a provider client, a model,
// and a set of tools.
type Agent struct {
	Client provider.Client
	Model  string
	// System is mutex-protected; use SetSystemPrompt to write it and
	// PromptConfig to read it.
	System string
	// Tools is mutex-protected; use SetTools to write it and PromptConfig
	// or ToolsSnapshot to read it.
	Tools       Registry
	MaxSteps    int
	Reasoning   string
	Temperature *float32
	// FastMode requests the OpenAI fast service tier. The provider boundary
	// rejects it for providers that do not support that contract.
	FastMode bool

	// ContextWindow is the effective input context capacity retained from
	// model resolution. Hosts use it for proactive compaction even when the
	// model was synthesized for an open-catalog provider.
	ContextWindow int

	// MaxTokens caps the model's output tokens per turn. Zero leaves
	// the field unset on the provider request, letting each provider
	// apply its own default (which can be conservative, e.g. Bedrock
	// defaults to 4096, truncating long writes/edits). Hosts populate
	// this from the resolved model's MaxOutput so large single-turn
	// responses aren't silently cut off with stopReason=length.
	MaxTokens int

	// BeforeToolExecute, if set, is called immediately before each
	// tool runs. Returning (allowed=false, reason) short-circuits
	// the call with an error result containing reason. Optionally,
	// returning a non-nil modifiedArgs replaces the JSON args the
	// tool will see, which lets guards redact / augment / patch the
	// model's request without rewriting the transcript. Empty or
	// malformed modifiedArgs is ignored.
	BeforeToolExecute func(call provider.ToolCallBlock) (allowed bool, reason string, modifiedArgs json.RawMessage)

	// BeforeTurn, if set, is called before each turn's model call.
	// Returning (allowed=false, reason) aborts the turn; reason is
	// surfaced as an assistant-like status line. Used for rate-
	// limiting, business-hour gates, and deny-by-default setups.
	BeforeTurn func(step int) (allowed bool, reason string)

	// BeforeTurnContext is the richer turn-start hook. It receives the
	// current turn context so blocking interception can honor cancellation.
	// It may return a bounded, non-transcript context string that is appended
	// to the provider request's system prompt for this turn only. When set, it
	// replaces BeforeTurn so a host can combine approval and context
	// collection into one lifecycle round trip.
	BeforeTurnContext func(ctx context.Context, step int) (allowed bool, reason, context string)

	// CommitToolResult, if set, commits extension/tool metadata immediately
	// after each tool call completes. Returning an error converts the result to
	// an error before it is appended to the transcript or sent to the provider.
	CommitToolResult func(id string, result ToolResult) error
	// OnToolResult observes the final result after CommitToolResult succeeds or
	// converts it to an error.
	OnToolResult func(id string, result ToolResult)

	// BeforeAssistantMessage, if set, is called after the model's
	// final assistant message is assembled but before it's appended
	// to the transcript. Returning (allowed=false) suppresses both
	// the transcript append and the UI event. A non-empty
	// replacement rewrites the visible text for the user while
	// leaving the model's original text in the transcript (so the
	// model can still see what it said in subsequent turns).
	BeforeAssistantMessage func(text string) (allowed bool, reason, replacement string)

	// MaxRetries controls agent-level retries for transient provider
	// failures that arrive after the HTTP stream opens (for example
	// Anthropic overloaded_error). Zero disables this retry layer.
	// RetryBaseDelay is doubled for each attempt; zero uses 2s.
	MaxRetries     int
	RetryBaseDelay time.Duration
	// StreamIdleTimeout cancels an individual provider stream when it emits no
	// events for this duration. Zero leaves the stream unbounded.
	StreamIdleTimeout time.Duration
	streamIdleTimer   func(time.Duration) (<-chan time.Time, func(), func())

	// OnEvent, if set, mirrors every AgentEvent the loop emits to
	// this callback in addition to the per-Prompt sink. Used by the
	// extension manager to fan events out to subscribed extensions
	// without each caller having to compose sinks manually.
	OnEvent func(AgentEvent)

	// OnMessageAppended, if set, fires every time a message is
	// appended to the in-memory transcript by the agent loop — the
	// initial user prompt, each finalised assistant message, and
	// each tool-results message (plus the synthetic OpenAI image
	// mirror, if any). Hosts wire this to the on-disk session so
	// that turns are durable as soon as they happen, instead of
	// only being flushed on a clean exit.
	OnMessageAppended func(provider.Message)

	// OnUsage, if set, fires after every turn's usage row arrives,
	// carrying the cumulative usage for the session. Hosts wire
	// this to the on-disk session so the persisted total stays
	// current and a crash recovers the right cost figure.
	OnUsage func(cumulative provider.Usage)

	// OnRetryLifecycle receives sanitized retry audit records. It never
	// receives provider error text or response bodies.
	OnRetryLifecycle func(RetryLifecycleRecord)

	// OnTranscriptCompacted, if set, fires after Compact replaces the
	// in-memory transcript with the synthetic summary plus kept tail.
	// Hosts wire this to append an explicit compaction checkpoint to
	// the session log; per-message append hooks do not fire for this
	// wholesale transcript replacement.
	OnTranscriptCompacted func(messages []provider.Message)

	mu       sync.Mutex
	messages []provider.Message
	// rev increments whenever the transcript slice is replaced or a
	// message is appended. The TUI uses it as a cheap redraw cache key
	// so editor-only typing doesn't copy/rebuild a long transcript on
	// every keypress.
	rev  uint64
	cost CostTracker

	// queued holds user messages submitted while the agent is busy.
	// The loop appends them as normal user messages at safe
	// boundaries: before the next model call after a tool batch, or
	// after a text-only assistant turn finishes. It never interrupts
	// a running tool or cancels an in-flight provider request.
	queued      []queuedMessage
	timeContext agentTimeContext
}

// NewAgent returns an Agent with sensible defaults.
func NewAgent(client provider.Client, model, system string, tools Registry) *Agent {
	return &Agent{
		Client:         client,
		Model:          model,
		System:         system,
		Tools:          tools,
		MaxSteps:       0, // 0 = unlimited
		MaxRetries:     3,
		RetryBaseDelay: 2 * time.Second,
		timeContext:    newAgentTimeContext(time.Now()),
	}
}

// SetFastMode changes the service-tier preference for the next model call.
// It is safe to use while a turn is in flight; the active turn keeps the
// value it already captured.
func (a *Agent) SetFastMode(enabled bool) {
	a.mu.Lock()
	a.FastMode = enabled
	a.mu.Unlock()
}

// FastModeEnabled returns the service-tier preference for a model call.
func (a *Agent) FastModeEnabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.FastMode
}

// QueueMessage queues text to be injected as a user message at the
// next safe boundary of the active agent loop. It is non-blocking in
// the sense that it never waits for model/tool work; it only takes
// the transcript mutex briefly. Empty/whitespace-only messages are
// ignored.
func (a *Agent) QueueMessage(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	a.mu.Lock()
	a.queued = append(a.queued, queuedMessage{text: text, accepted: time.Now()})
	a.mu.Unlock()
	return true
}

// PendingQueuedMessages returns a snapshot of user messages waiting
// to be injected. Used by hosts to render the visible "sliding in"
// chips without consuming them.
func (a *Agent) PendingQueuedMessages() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.queued))
	for i, queued := range a.queued {
		out[i] = queued.text
	}
	return out
}

// QueuedMessageCount returns the number of messages waiting to be
// injected at the next safe boundary.
func (a *Agent) QueuedMessageCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.queued)
}

// PopQueuedMessage removes and returns the most recently queued
// message. Hosts use this for the slide-back keybinding.
func (a *Agent) PopQueuedMessage() (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := len(a.queued)
	if n == 0 {
		return "", false
	}
	text := a.queued[n-1].text
	a.queued = a.queued[:n-1]
	return text, true
}

// DrainQueuedMessages discards and returns every queued message.
// Hosts use this on explicit cancel/clear so stale follow-ups do
// not run after the user aborted the turn.
func (a *Agent) DrainQueuedMessages() []string {
	queued := a.drainQueuedMessages()
	out := make([]string, len(queued))
	for i, message := range queued {
		out[i] = message.text
	}
	return out
}

func (a *Agent) drainQueuedMessages() []queuedMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := append([]queuedMessage(nil), a.queued...)
	a.queued = nil
	return out
}

func (a *Agent) appendQueuedAsUser(messages []queuedMessage, sink func(AgentEvent)) {
	for _, queued := range messages {
		accepted := queued.accepted
		if accepted.IsZero() {
			accepted = time.Now()
		}
		msg := provider.Message{
			Role:    provider.RoleUser,
			Content: []provider.Content{provider.TextBlock{Text: queued.text}},
			Time:    accepted,
		}
		a.mu.Lock()
		a.messages = append(a.messages, msg)
		a.rev++
		a.mu.Unlock()
		a.fireMessageAppended(msg)
		if sink != nil {
			sink(EvUserMessage{Message: msg})
		}
	}
}

// Messages returns a copy of the current transcript.
func (a *Agent) Messages() []provider.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]provider.Message, len(a.messages))
	copy(out, a.messages)
	return out
}

// MessageCount returns the current transcript length without copying it.
func (a *Agent) MessageCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.messages)
}

// MessagesFrom returns a copy of the transcript suffix beginning at from and
// the total transcript length observed under the same lock.
func (a *Agent) MessagesFrom(from int) ([]provider.Message, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if from < 0 {
		from = 0
	}
	if from > len(a.messages) {
		from = len(a.messages)
	}
	out := make([]provider.Message, len(a.messages)-from)
	copy(out, a.messages[from:])
	return out, len(a.messages)
}

// Revision returns a monotonically increasing transcript version.
// It is cheap to query and changes whenever Messages() would return
// different transcript content because of append/set operations.
func (a *Agent) Revision() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rev
}

// SetTools swaps the tool registry. Used by /reload-ext to hand
// the agent a fresh registry after extension subprocesses have been
// respawned (and their freshly-registered tools merged in).
func (a *Agent) SetTools(reg Registry) {
	a.mu.Lock()
	a.Tools = reg
	a.mu.Unlock()
}

// SetSystemPrompt replaces the system prompt used for subsequent turns.
func (a *Agent) SetSystemPrompt(system string) {
	a.mu.Lock()
	a.System = system
	a.mu.Unlock()
}

// SetPromptConfig atomically replaces the system prompt and tool registry.
// It returns the previous registry so the caller can close resources that are
// no longer owned by the agent.
func (a *Agent) SetPromptConfig(system string, tools Registry) Registry {
	a.mu.Lock()
	defer a.mu.Unlock()
	oldTools := a.Tools
	a.System = system
	a.Tools = tools
	return oldTools
}

// PromptConfig returns a consistent snapshot of the system prompt and tools
// used to construct a provider request.
func (a *Agent) PromptConfig() (string, Registry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	tools := make(Registry, len(a.Tools))
	for name, tool := range a.Tools {
		tools[name] = tool
	}
	return a.System, tools
}

// ToolsSnapshot returns a copy of the current tool registry.
func (a *Agent) ToolsSnapshot() Registry {
	a.mu.Lock()
	defer a.mu.Unlock()
	tools := make(Registry, len(a.Tools))
	for name, tool := range a.Tools {
		tools[name] = tool
	}
	return tools
}

// SetMessages replaces the transcript (used when resuming a session).
func (a *Agent) SetMessages(msgs []provider.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = append(a.messages[:0], msgs...)
	a.rev++
}

// AppendUserContext adds a user-role message to the transcript without
// starting a model turn. Hosts use it for context gathered outside the agent
// loop, such as the output of an explicitly invoked shell command.
func (a *Agent) AppendUserContext(text string, meta map[string]string) {
	msg := provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: text}},
		Time:    time.Now(),
		Meta:    meta,
	}
	a.mu.Lock()
	a.messages = append(a.messages, msg)
	a.rev++
	a.mu.Unlock()
	a.fireMessageAppended(msg)
}

// Cost returns the cumulative usage.
func (a *Agent) Cost() provider.Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cost.Total
}

func (a *Agent) addUsage(usage provider.Usage) provider.Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cost.Add(usage)
}

// SeedCost sets the cumulative usage as a baseline before the first
// turn runs. Used when transferring state from another agent (model
// or provider switch) so the running cost meter doesn't reset to 0.
func (a *Agent) SeedCost(u provider.Usage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cost.Seed(u)
}

// LastTurnUsage returns the per-turn usage of the most recent
// completed turn. Drives the "context used" gauge in the status bar
// without waiting for the next turn to land.
func (a *Agent) LastTurnUsage() provider.Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cost.LastTurn
}

// SeedLastTurnUsage primes the per-turn snapshot. Used on resume so
// the gauge reflects the prompt size of the last turn in the session
// file instead of starting at zero.
func (a *Agent) SeedLastTurnUsage(u provider.Usage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cost.LastTurn = u
}

// fireMessageAppended invokes OnMessageAppended without holding the
// agent mutex, so the host's persistence callback can take its own
// locks without deadlocking the agent loop. Tolerates a nil hook so
// non-persisting callers (tests, RPC mode) don't have to set it.
func (a *Agent) fireMessageAppended(m provider.Message) {
	if a.OnMessageAppended != nil {
		a.OnMessageAppended(m)
	}
}

// Prompt sends a user message and runs the agent loop until the model
// stops or an error occurs. Events are delivered via sink in order.
// sink must not block the caller for long; buffer as needed.
func (a *Agent) Prompt(ctx context.Context, text string, images []provider.ImageBlock, sink func(AgentEvent)) error {
	if text == "" && len(images) == 0 {
		return errors.New("prompt is empty")
	}
	if sink == nil {
		sink = func(AgentEvent) {}
	}
	sink = a.wrapSink(sink)
	content := []provider.Content{}
	if text != "" {
		content = append(content, provider.TextBlock{Text: text})
	}
	for _, img := range images {
		content = append(content, img)
	}
	user := provider.Message{Role: provider.RoleUser, Content: content, Time: time.Now()}

	a.mu.Lock()
	a.messages = append(a.messages, user)
	a.rev++
	a.mu.Unlock()
	a.fireMessageAppended(user)
	sink(EvUserMessage{Message: user})

	return a.runLoop(ctx, sink)
}

// Continue runs the agent loop against the existing transcript. Used
// after appending tool results manually or to retry.
func (a *Agent) Continue(ctx context.Context, sink func(AgentEvent)) error {
	if sink == nil {
		sink = func(AgentEvent) {}
	}
	sink = a.wrapSink(sink)
	return a.runLoop(ctx, sink)
}

// wrapSink composes the per-call sink with a.OnEvent (if set) so the
// extension manager (or any other observer) sees every AgentEvent
// without having to thread itself through every Prompt callsite.
func (a *Agent) wrapSink(sink func(AgentEvent)) func(AgentEvent) {
	if a.OnEvent == nil {
		return sink
	}
	obs := a.OnEvent
	return func(ev AgentEvent) {
		obs(ev)
		sink(ev)
	}
}

func (a *Agent) runLoop(ctx context.Context, sink func(AgentEvent)) error {
	for step := 1; a.MaxSteps <= 0 || step <= a.MaxSteps; step++ {
		// Messages queued while the agent was busy are delivered
		// before the next model call. This is the safe boundary:
		// any previous tool batch has already completed and its
		// results have been appended, but no new provider request has
		// started yet.
		if pending := a.drainQueuedMessages(); len(pending) > 0 {
			a.appendQueuedAsUser(pending, sink)
		}

		sink(EvTurnStart{Step: step})
		turnContext := ""
		if a.BeforeTurnContext != nil {
			var allowed bool
			var reason string
			allowed, reason, turnContext = a.BeforeTurnContext(ctx, step)
			if !allowed {
				if reason == "" {
					reason = "turn blocked by extension guard"
				}
				sink(EvTurnEnd{Stop: provider.StopError, Err: fmt.Errorf("%s", reason)})
				sink(EvDone{})
				return nil
			}
		} else if a.BeforeTurn != nil {
			if allowed, reason := a.BeforeTurn(step); !allowed {
				if reason == "" {
					reason = "turn blocked by extension guard"
				}
				sink(EvTurnEnd{Stop: provider.StopError, Err: fmt.Errorf("%s", reason)})
				sink(EvDone{})
				return nil
			}
		}

		var (
			stop         provider.StopReason
			assistantMsg provider.Message
			err          error
		)
		maxAttempts := max(a.MaxRetries+1, 1)
		for attempt := 0; ; attempt++ {
			stop, assistantMsg, err = a.oneTurn(ctx, sink, turnContext, attempt, maxAttempts)
			sink(EvTurnEnd{Stop: stop, Err: err})
			if err == nil {
				break
			}
			retryable := a.canRetryError(err, attempt)
			var retryCtxErr error
			if retryable {
				retryCtxErr = ctx.Err()
				if retryCtxErr != nil {
					retryable = false
				}
			}
			reason := classifyRetryReason(err)
			if !errors.Is(err, context.Canceled) {
				a.fireRetryLifecycle(RetryLifecycleRecord{
					Event:       RetryLifecycleRequestFailed,
					Scope:       RetryScopeAgent,
					Attempt:     attempt + 1,
					MaxAttempts: maxAttempts,
					Reason:      reason,
					Terminal:    !retryable,
				})
			}
			if retryCtxErr != nil {
				return retryCtxErr
			}
			if !retryable {
				break
			}
			a.dropLastAssistantMessage()
			delay := a.retryDelay(attempt)
			if retryErr := ctx.Err(); retryErr != nil {
				return retryErr
			}
			a.fireRetryLifecycle(RetryLifecycleRecord{
				Event:       RetryLifecycleRetryScheduled,
				Scope:       RetryScopeAgent,
				Attempt:     attempt + 2,
				MaxAttempts: maxAttempts,
				Reason:      reason,
				DelayMS:     delay.Milliseconds(),
			})
			sink(EvRetryScheduled{
				Scope:       RetryScopeAgent,
				Attempt:     attempt + 2,
				MaxAttempts: maxAttempts,
				Delay:       delay,
			})
			if sleepErr := sleepRetry(ctx, delay); sleepErr != nil {
				return sleepErr
			}
		}
		if err != nil {
			return err
		}

		if stop == provider.StopToolUse {
			// Execute each tool call, append a single tool-results message, continue.
			toolMsg, hadError := a.executeTools(ctx, assistantMsg, sink)
			a.mu.Lock()
			a.messages = append(a.messages, toolMsg)
			a.rev++
			// OpenAI's chat-completions tool message shape is text-centric.
			// Vision models reliably consume images when they arrive as user
			// content, so when a tool result contains images we mirror them
			// into a synthetic user message immediately after the tool result.
			// This keeps the transcript self-contained for providers that can
			// see image blocks in tool messages while making OpenAI vision
			// models actually receive the image bytes.
			//
			// The OpenAI Responses route ("openai-codex") has the same
			// text-centric tool-output shape: a function_call_output only
			// carries a string, so images in a tool result never reach the
			// model. Both providers serialize images correctly when they
			// arrive as user content, so the mirror covers them both.
			var imageMirror provider.Message
			if a.Client != nil && (a.Client.Name() == "openai" || a.Client.Name() == "openai-codex") {
				if mirror := mirrorToolImagesAsUser(toolMsg); len(mirror.Content) > 0 {
					a.messages = append(a.messages, mirror)
					a.rev++
					imageMirror = mirror
				}
			}
			a.mu.Unlock()
			a.fireMessageAppended(toolMsg)
			if len(imageMirror.Content) > 0 {
				a.fireMessageAppended(imageMirror)
			}
			// If context was cancelled during tool execution, bail out.
			if err := ctx.Err(); err != nil {
				sink(EvDone{})
				return err
			}
			_ = hadError
			continue
		}

		// If the assistant stopped without tool calls but a message was
		// queued while it was speaking, loop once more so that message
		// is appended and answered instead of waiting until a later
		// top-level prompt.
		if ctx.Err() == nil && a.QueuedMessageCount() > 0 {
			continue
		}

		// Terminal stop (end, length, error, aborted).
		sink(EvDone{})
		return nil
	}
	if a.MaxSteps > 0 {
		sink(EvDone{})
		return fmt.Errorf("max steps (%d) exceeded", a.MaxSteps)
	}
	return nil
}

func (a *Agent) canRetryError(err error, attempt int) bool {
	if err == nil || a.MaxRetries <= 0 || attempt >= a.MaxRetries {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := strings.ToLower(err.Error())
	if msg == "" || isNonRetryableProviderLimit(msg) {
		return false
	}
	needles := []string{
		"overloaded", "provider returned error", "rate limit", "ratelimit", "too many requests",
		"429", "http 429", "500", "http 500", "502", "http 502", "503", "http 503", "504", "http 504",
		"service unavailable", "server error", "internal error", "network error", "connection error",
		"connection refused", "connection lost", "fetch failed", "upstream connect", "reset before headers",
		"socket hang up", "ended without", "stream ended before", "did not get a response", "timed out",
		"timeout", "terminated", "unexpected eof", "transport failure",
		// OpenAI's ChatGPT/Codex backend returns this generic message (with a
		// request ID) for transient server failures and explicitly says
		// "You can retry your request".
		"an error occurred while processing your request",
		// Explicit retry guidance emitted by provider backends (OpenAI
		// Responses, AWS Bedrock stream exceptions) with varying prefixes.
		"you can retry your request", "try your request again", "please retry your request",
		// Capacity messages from the ChatGPT/Codex backend, e.g.
		// "Our servers are currently overloaded. Please try again later."
		// The trailing advice also shows up on its own for transient
		// capacity failures; usage/quota limits are filtered out above by
		// isNonRetryableProviderLimit before this list is consulted.
		"servers are currently overloaded", "servers are busy", "try again later",
	}
	for _, needle := range needles {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func isNonRetryableProviderLimit(msg string) bool {
	needles := []string{
		"usage limit", "monthly usage limit", "freeusagelimit", "gousagelimit",
		"available balance", "insufficient_quota", "out of budget", "quota exceeded", "billing",
	}
	for _, needle := range needles {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func classifyRetryReason(err error) RetryReason {
	if err == nil {
		return RetryReasonUnknown
	}
	msg := strings.ToLower(err.Error())
	if isNonRetryableProviderLimit(msg) {
		return RetryReasonQuota
	}
	if strings.Contains(msg, "overload") || strings.Contains(msg, "servers are busy") {
		return RetryReasonOverload
	}
	if strings.Contains(msg, "rate limit") || strings.Contains(msg, "ratelimit") || strings.Contains(msg, "too many requests") || strings.Contains(msg, "429") {
		return RetryReasonRateLimit
	}
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "timeout") || strings.Contains(msg, "timed out") {
		return RetryReasonTimeout
	}
	for _, needle := range []string{"connection", "network", "unexpected eof", "broken pipe", "socket hang up", "transport failure", "upstream connect"} {
		if strings.Contains(msg, needle) {
			return RetryReasonNetwork
		}
	}
	for _, needle := range []string{"context window", "context length", "maximum context", "too many tokens"} {
		if strings.Contains(msg, needle) {
			return RetryReasonContextWindow
		}
	}
	for _, needle := range []string{"unauthorized", "authentication", "invalid api key", "invalid_api_key", "http 401", "http 403"} {
		if strings.Contains(msg, needle) {
			return RetryReasonAuth
		}
	}
	for _, needle := range []string{"server error", "internal error", "service unavailable", "http 500", "http 502", "http 503", "http 504", "provider returned error"} {
		if strings.Contains(msg, needle) {
			return RetryReasonServer
		}
	}
	for _, needle := range []string{"bad request", "unsupported", "invalid parameter", "http 400", "http 404", "http 422"} {
		if strings.Contains(msg, needle) {
			return RetryReasonClient
		}
	}
	return RetryReasonUnknown
}

func (a *Agent) fireRetryLifecycle(record RetryLifecycleRecord) {
	if a.OnRetryLifecycle != nil {
		a.OnRetryLifecycle(record)
	}
}

func (a *Agent) retryDelay(attempt int) time.Duration {
	base := a.RetryBaseDelay
	if base <= 0 {
		base = 2 * time.Second
	}
	return base * time.Duration(1<<attempt)
}

func sleepRetry(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (a *Agent) dropLastAssistantMessage() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n := len(a.messages); n > 0 && a.messages[n-1].Role == provider.RoleAssistant {
		a.messages = a.messages[:n-1]
		a.rev++
	}
}

// requestLifecycleSink adapts provider-owned pre-stream retries into core
// events without letting provider concerns leak into mode renderers.
type requestLifecycleSink struct {
	sink       func(AgentEvent)
	observe    func(RetryLifecycleRecord)
	provider   string
	model      string
	lastReason RetryReason
}

func (s *requestLifecycleSink) RequestAttempt(attempt, maxAttempts int) {
	s.sink(EvRequestStarted{
		Provider:    s.provider,
		Model:       s.model,
		Scope:       RetryScopeProvider,
		Attempt:     attempt,
		MaxAttempts: maxAttempts,
	})
}

func (s *requestLifecycleSink) RequestFailed(attempt, maxAttempts int, reason provider.RequestFailureReason, terminal bool) {
	s.lastReason = retryReasonFromProvider(reason)
	if s.observe != nil {
		s.observe(RetryLifecycleRecord{
			Event:       RetryLifecycleRequestFailed,
			Scope:       RetryScopeProvider,
			Attempt:     attempt,
			MaxAttempts: maxAttempts,
			Reason:      s.lastReason,
			Terminal:    terminal,
		})
	}
}

func (s *requestLifecycleSink) RetryScheduled(attempt, maxAttempts int, delay time.Duration) {
	s.sink(EvRetryScheduled{
		Scope:       RetryScopeProvider,
		Attempt:     attempt,
		MaxAttempts: maxAttempts,
		Delay:       delay,
	})
	if s.observe != nil {
		reason := s.lastReason
		if reason == "" {
			reason = RetryReasonUnknown
		}
		s.observe(RetryLifecycleRecord{
			Event:       RetryLifecycleRetryScheduled,
			Scope:       RetryScopeProvider,
			Attempt:     attempt,
			MaxAttempts: maxAttempts,
			Reason:      reason,
			DelayMS:     delay.Milliseconds(),
		})
	}
	s.lastReason = ""
}

func retryReasonFromProvider(reason provider.RequestFailureReason) RetryReason {
	switch reason {
	case provider.RequestFailureOverload:
		return RetryReasonOverload
	case provider.RequestFailureRateLimit:
		return RetryReasonRateLimit
	case provider.RequestFailureQuota:
		return RetryReasonQuota
	case provider.RequestFailureServer:
		return RetryReasonServer
	case provider.RequestFailureNetwork:
		return RetryReasonNetwork
	case provider.RequestFailureTimeout:
		return RetryReasonTimeout
	case provider.RequestFailureClient:
		return RetryReasonClient
	default:
		return RetryReasonUnknown
	}
}

// oneTurn calls the LLM once, forwards events, returns the stop reason
// and the assembled assistant message (already appended to the transcript).
func newStreamIdleTimer(timeout time.Duration) (<-chan time.Time, func(), func()) {
	timer := time.NewTimer(timeout)
	stop := func() { timer.Stop() }
	reset := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(timeout)
	}
	return timer.C, stop, reset
}

func (a *Agent) oneTurn(ctx context.Context, sink func(AgentEvent), turnContext string, attempt, maxAttempts int) (provider.StopReason, provider.Message, error) {
	fastMode := a.FastModeEnabled()
	if err := provider.ValidateFastMode(a.Client.Name(), fastMode); err != nil {
		return provider.StopError, provider.Message{}, err
	}
	system, tools := a.PromptConfig()
	systemContext := a.providerTimeContext().systemText()
	if contextText := boundedTurnContext(turnContext); contextText != "" {
		if system != "" {
			system += "\n\n"
		}
		system += "[Extension context for this turn]\n" + contextText
	}
	// Repair pairs before projecting the copied provider-input view. The
	// repair can add stub results for aborted calls; those results must also
	// remain in the request so every tool call still has a matching result.
	messages := repairToolUseResultPairs(a.Messages())
	requestLifecycle := &requestLifecycleSink{
		sink:     sink,
		observe:  a.fireRetryLifecycle,
		provider: a.Client.Name(),
		model:    a.Model,
	}
	req := provider.Request{
		Model:         a.Model,
		System:        system,
		SystemContext: systemContext,
		// Repair any dangling tool_use blocks before sending. A turn
		// aborted mid-flight (cancel, connection drop, ECONNREFUSED to a
		// dev server, etc.) can leave an assistant tool_use with no
		// matching tool_result in the live transcript. The load-time
		// repair in OpenSession only runs on restart, so without this the
		// next in-process request is rejected by providers like Anthropic
		// with "tool_use ids were found without tool_result blocks". The
		// repair is pure and a no-op on already-valid transcripts.
		Messages:    projectProviderMessages(messages),
		Tools:       tools.Specs(),
		Reasoning:   a.Reasoning,
		FastMode:    fastMode,
		MaxTokens:   a.MaxTokens,
		Temperature: a.Temperature,
		Lifecycle:   requestLifecycle,
	}
	sink(EvRequestStarted{
		Provider:    a.Client.Name(),
		Model:       a.Model,
		Scope:       RetryScopeAgent,
		Attempt:     attempt + 1,
		MaxAttempts: maxAttempts,
	})
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	stream, err := a.Client.Stream(streamCtx, req)
	if err != nil {
		return provider.StopError, provider.Message{}, err
	}

	sink(EvAssistantStart{})

	var (
		stop     provider.StopReason
		finalErr error
		finalMsg provider.Message
	)

	var idle <-chan time.Time
	var resetIdle func()
	if a.StreamIdleTimeout > 0 {
		newTimer := a.streamIdleTimer
		if newTimer == nil {
			newTimer = newStreamIdleTimer
		}
		var stopIdle func()
		idle, stopIdle, resetIdle = newTimer(a.StreamIdleTimeout)
		defer stopIdle()
	}
	handleStreamEvent := func(ev provider.Event) (done bool) {
		if resetIdle != nil {
			resetIdle()
		}
		switch e := ev.(type) {
		case provider.EventStart:
			// nothing
		case provider.EventTextDelta:
			sink(EvTextDelta{Delta: e.Delta})
		case provider.EventToolStart:
			sink(EvToolUseStart{ID: e.ID, Name: e.Name})
		case provider.EventToolArgs:
			sink(EvToolUseArgs{ID: e.ID, Delta: e.Delta})
		case provider.EventToolEnd:
			sink(EvToolUseEnd{ID: e.ID})
		case provider.EventUsage:
			cum := a.addUsage(e.Usage)
			sink(EvUsage{Usage: e.Usage, Cumulative: cum})
			if a.OnUsage != nil {
				a.OnUsage(cum)
			}
		case provider.EventDone:
			stop = e.Stop
			finalErr = e.Err
			finalMsg = e.Message
			return true
		}
		return false
	}
	for {
		// Consume no more than one buffered event before honoring cancellation.
		select {
		case ev, ok := <-stream:
			if !ok || handleStreamEvent(ev) {
				goto streamDone
			}
		default:
		}
		if err := streamCtx.Err(); err != nil {
			return provider.StopError, finalMsg, err
		}
		select {
		case <-streamCtx.Done():
			return provider.StopError, finalMsg, streamCtx.Err()
		case <-idle:
			return provider.StopError, finalMsg, ErrStreamIdleTimeout
		case ev, ok := <-stream:
			if !ok || handleStreamEvent(ev) {
				goto streamDone
			}
		}
	}
streamDone:

	// Append assistant message to transcript. Aborted turns (Esc / Ctrl+C)
	// produce partial content. When the partial message is text only we
	// keep whatever was streamed up to the cancel so the user does not
	// lose visible work (a cut-off summary is still useful). If the
	// partial message already contained tool-call blocks we drop the
	// whole thing, because an unmatched tool_use would fail the next
	// turn with a tool_result mismatch error.
	keep := len(finalMsg.Content) > 0
	if stop == provider.StopAborted && keep {
		hasToolCall := false
		for _, c := range finalMsg.Content {
			if _, ok := c.(provider.ToolCallBlock); ok {
				hasToolCall = true
				break
			}
		}
		if hasToolCall {
			keep = false
		}
	}
	if keep {
		emit := finalMsg
		suppress := false

		// BeforeAssistantMessage hook: extensions can suppress or
		// rewrite the visible text. The transcript keeps the
		// model's original output so the model still sees what it
		// said on subsequent turns.
		if a.BeforeAssistantMessage != nil {
			orig := extractText(finalMsg)
			if orig != "" {
				allowed, _, replacement := a.BeforeAssistantMessage(orig)
				if !allowed {
					suppress = true
				} else if replacement != "" && replacement != orig {
					emit = replaceText(finalMsg, replacement)
				}
			}
		}

		a.mu.Lock()
		a.messages = append(a.messages, finalMsg)
		a.rev++
		a.mu.Unlock()
		a.fireMessageAppended(finalMsg)
		if !suppress {
			sink(EvAssistantMessage{Message: emit})
		}
		// Now surface tool calls as EvToolCall events so UIs can render them
		// in order before the tool results arrive.
		for _, c := range finalMsg.Content {
			if tc, ok := c.(provider.ToolCallBlock); ok {
				sink(EvToolCall{ID: tc.ID, Name: tc.Name, Args: tc.Arguments})
			}
		}
	}

	return stop, finalMsg, finalErr
}

// executeTools runs every tool call in the assistant message and returns
// a single tool-role message carrying all results.
func (a *Agent) executeTools(ctx context.Context, msg provider.Message, sink func(AgentEvent)) (provider.Message, bool) {
	var results []provider.Content
	var addedTools []string
	hadError := false
	tools := a.ToolsSnapshot()

	for _, c := range msg.Content {
		tc, ok := c.(provider.ToolCallBlock)
		if !ok {
			continue
		}
		res := a.runOneTool(ctx, tc, tools, sink)
		if a.CommitToolResult != nil {
			if err := a.CommitToolResult(tc.ID, res); err != nil {
				res = ToolResult{
					Content: []provider.Content{provider.TextBlock{Text: "tool result state could not be persisted"}},
					IsError: true,
					Timing:  res.Timing,
				}
			}
		}
		if res.IsError {
			hadError = true
		}
		results = append(results, provider.ToolResultBlock{
			CallID:  tc.ID,
			Content: res.Content,
			IsError: res.IsError,
			Timing:  res.Timing,
		})
		for _, name := range res.ActivateTools {
			if _, err := tools.Get(name); err == nil && !containsString(addedTools, name) {
				addedTools = append(addedTools, name)
			}
		}
		if a.OnToolResult != nil {
			a.OnToolResult(tc.ID, res)
		}
		sink(EvToolResult{ID: tc.ID, Result: res})
	}

	return provider.Message{
		Role:           provider.RoleTool,
		Content:        results,
		Time:           time.Now(),
		AddedToolNames: addedTools,
	}, hadError
}

func (a *Agent) runOneTool(ctx context.Context, tc provider.ToolCallBlock, tools Registry, sink func(AgentEvent)) (res ToolResult) {
	started := time.Now()
	defer func() {
		if recover() != nil {
			res = ToolResult{
				Content: []provider.Content{provider.TextBlock{Text: "tool execution failed"}},
				IsError: true,
			}
		}
		res.Timing = &provider.ToolTiming{
			StartedAt:   started,
			CompletedAt: time.Now(),
			Duration:    time.Since(started),
		}
	}()

	tool, err := tools.Get(tc.Name)
	if err != nil {
		return ToolResult{
			Content: []provider.Content{provider.TextBlock{Text: err.Error()}},
			IsError: true,
		}
	}

	args := tc.Arguments

	// Intercept hook: an extension or other guard can refuse the
	// call before any side effect happens, OR rewrite the args
	// seen by the tool. The model sees the reason as the tool
	// error, learns from it, and (typically) proposes a different
	// action; rewrites are invisible to the model (they apply only
	// to the execution).
	if a.BeforeToolExecute != nil {
		allowed, reason, modified := a.BeforeToolExecute(tc)
		if !allowed {
			if reason == "" {
				reason = "tool call refused by extension guard"
			}
			return ToolResult{
				Content: []provider.Content{provider.TextBlock{Text: reason}},
				IsError: true,
			}
		}
		if len(modified) > 0 && json.Valid(modified) {
			args = modified
		}
	}

	if len(args) == 0 {
		args = json.RawMessage("{}")
	}

	// Tool panics are recovered by the invocation-level defer above so
	// guard, lookup, progress, and Execute panics all receive timing.
	sink(EvToolExecutionStarted{ID: tc.ID, Name: tc.Name})
	out, err := tool.Execute(ctx, args, func(text string) {
		sink(EvToolProgress{ID: tc.ID, Text: text})
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ToolResult{
				Content: []provider.Content{provider.TextBlock{Text: "aborted: " + err.Error()}},
				IsError: true,
			}
		}
		return ToolResult{
			Content: []provider.Content{provider.TextBlock{Text: err.Error()}},
			IsError: true,
		}
	}
	return out
}

// extractText concatenates all TextBlock content in a message. Used
// by BeforeAssistantMessage so guards see a single string instead of
// having to walk provider.Content themselves.
func mirrorToolImagesAsUser(msg provider.Message) provider.Message {
	var content []provider.Content
	for _, c := range msg.Content {
		tr, ok := c.(provider.ToolResultBlock)
		if !ok {
			continue
		}
		for _, inner := range tr.Content {
			switch v := inner.(type) {
			case provider.TextBlock:
				// Keep short textual context so the model understands why
				// the images appeared, but don't duplicate giant read
				// outputs verbatim.
				if len(v.Text) > 0 && len(v.Text) <= 500 {
					content = append(content, v)
				}
			case provider.ImageBlock:
				content = append(content, v)
			}
		}
	}
	if len(content) == 0 {
		return provider.Message{}
	}
	prefix := provider.TextBlock{Text: "Tool output included the following image content:"}
	content = append([]provider.Content{prefix}, content...)
	return provider.Message{Role: provider.RoleUser, Content: content, Time: time.Now()}
}

const (
	maxTurnContextBytes        = 16 * 1024
	turnContextTruncatedMarker = "\n[extension context truncated]"
)

func boundedTurnContext(text string) string {
	text = strings.TrimSpace(text)
	if len(text) <= maxTurnContextBytes {
		return text
	}
	limit := maxTurnContextBytes - len(turnContextTruncatedMarker)
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return strings.TrimSpace(text[:cut]) + turnContextTruncatedMarker
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func extractText(msg provider.Message) string {
	var out string
	for _, c := range msg.Content {
		if tb, ok := c.(provider.TextBlock); ok {
			if out != "" {
				out += "\n"
			}
			out += tb.Text
		}
	}
	return out
}

// replaceText returns a copy of msg with every TextBlock replaced by
// a single TextBlock containing replacement. Non-text content (tool
// calls, etc.) is preserved in order.
func replaceText(msg provider.Message, replacement string) provider.Message {
	out := provider.Message{Role: msg.Role}
	out.Content = make([]provider.Content, 0, len(msg.Content))
	replaced := false
	for _, c := range msg.Content {
		if _, ok := c.(provider.TextBlock); ok {
			if !replaced {
				out.Content = append(out.Content, provider.TextBlock{Text: replacement})
				replaced = true
			}
			continue
		}
		out.Content = append(out.Content, c)
	}
	if !replaced {
		out.Content = append(out.Content, provider.TextBlock{Text: replacement})
	}
	return out
}
