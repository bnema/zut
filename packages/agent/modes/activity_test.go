package modes

import (
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestAgentActivityDescribesLifecycle(t *testing.T) {
	a := newAgentActivity("anthropic", "claude-test")
	check := func(want string) {
		t.Helper()
		if got := a.label(); got != want {
			t.Fatalf("label = %q, want %q", got, want)
		}
	}

	check("Preparing request")
	a.apply(core.EvRequestStarted{Provider: "anthropic", Model: "claude-test", Scope: core.RetryScopeProvider, Attempt: 1, MaxAttempts: 3})
	check("Sending request to anthropic")
	a.apply(core.EvAssistantStart{})
	check("Waiting for claude-test to respond")
	a.apply(core.EvTextDelta{Delta: "hello"})
	check("Receiving response from claude-test")
	a.apply(core.EvToolUseStart{ID: "one", Name: "read"})
	check("Preparing tool: read")
	a.apply(core.EvToolCall{ID: "one", Name: "read"})
	a.apply(core.EvToolCall{ID: "two", Name: "bash"})
	a.apply(core.EvToolExecutionStarted{ID: "one", Name: "read"})
	check("Running tool: read")
	a.apply(core.EvToolResult{ID: "one", Result: core.ToolResult{}})
	check("Preparing tool: bash")
	a.apply(core.EvToolExecutionStarted{ID: "two", Name: "bash"})
	a.apply(core.EvToolResult{ID: "two", Result: core.ToolResult{}})
	check("Sending tool results to anthropic")
	a.apply(core.EvRetryScheduled{Scope: core.RetryScopeAgent, Attempt: 2, MaxAttempts: 4, Delay: 2 * time.Second})
	check("Retrying request in 2s (attempt 2/4)")
}

func TestActivityLabelsSanitizeAndFallback(t *testing.T) {
	a := activity{kind: activitySendingRequest, provider: " \x1b[31mremote\n"}
	if got, want := a.label(), "Sending request to [31mremote"; got != want {
		t.Fatalf("sanitized label = %q, want %q", got, want)
	}
	a = activity{kind: activityRunningTool}
	if got, want := a.label(), "Running tool: tool"; got != want {
		t.Fatalf("fallback label = %q, want %q", got, want)
	}
}

func TestAgentActivityIgnoresUnknownToolResult(t *testing.T) {
	a := newAgentActivity("provider", "model")
	a.apply(core.EvToolResult{ID: "unknown", Result: core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "ignored"}}}})
	if got, want := a.label(), "Preparing request"; got != want {
		t.Fatalf("label = %q, want %q", got, want)
	}
}

func TestAgentActivityClearsDiscardedToolCallsOnAgentRetry(t *testing.T) {
	a := newAgentActivity("provider", "model")
	a.apply(core.EvToolCall{ID: "discarded", Name: "stale"})
	a.apply(core.EvRetryScheduled{Scope: core.RetryScopeAgent, Attempt: 2, MaxAttempts: 2, Delay: time.Millisecond})
	a.apply(core.EvToolCall{ID: "actual", Name: "read"})
	a.apply(core.EvToolResult{ID: "actual", Result: core.ToolResult{}})

	if got, want := a.label(), "Sending tool results to provider"; got != want {
		t.Fatalf("label = %q, want %q", got, want)
	}
}

func TestAgentActivityIdleRetryMessage(t *testing.T) {
	a := newAgentActivity("provider", "model")
	a.apply(core.EvRetryScheduled{Scope: core.RetryScopeAgent, Message: "idle timeout (1/1)"})
	if got := a.label(); got != "Retrying request: idle timeout (1/1)" {
		t.Fatalf("label = %q", got)
	}
	a.apply(core.EvRetryScheduled{Scope: core.RetryScopeAgent, Attempt: 2, MaxAttempts: 3, Delay: time.Second})
	if got := a.label(); got != "Retrying request in 1s (attempt 2/3)" {
		t.Fatalf("label = %q", got)
	}
	event := EventToJSON(core.EvRetryScheduled{Message: "idle timeout (1/1)"})
	if event["message"] != "idle timeout (1/1)" {
		t.Fatalf("event = %v", event)
	}
}
