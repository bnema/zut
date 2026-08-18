package core

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
)

type compactLifecycleClient struct {
	hadLifecycle bool
	reportRetry  bool
	req          provider.Request
	summary      string
	usage        []provider.Usage
}

func (c *compactLifecycleClient) Name() string { return "compact-lifecycle" }

func (c *compactLifecycleClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.req = req
	c.hadLifecycle = req.Lifecycle != nil
	if req.Lifecycle != nil {
		maxAttempts := 1
		if c.reportRetry {
			maxAttempts = 2
		}
		req.Lifecycle.RequestAttempt(1, maxAttempts)
		if c.reportRetry {
			if failures, ok := req.Lifecycle.(provider.RequestFailureLifecycle); ok {
				failures.RequestFailed(1, 2, provider.RequestFailureOverload, false)
			}
			req.Lifecycle.RetryScheduled(2, 2, 250*time.Millisecond)
		}
	}
	summary := c.summary
	if summary == "" {
		summary = "summary"
	}
	out := make(chan provider.Event, 2+len(c.usage))
	out <- provider.EventTextDelta{Delta: summary}
	for _, usage := range c.usage {
		out <- provider.EventUsage{Usage: usage}
	}
	out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.TextBlock{Text: summary}},
	}}
	close(out)
	return out, nil
}

func TestCompactPreservesOnlyLatestInternalContext(t *testing.T) {
	client := &compactLifecycleClient{}
	agent := NewAgent(client, "compact-model", "system", Registry{})
	agent.SetMessages([]provider.Message{
		{Role: provider.RoleDeveloper, Content: []provider.Content{provider.TextBlock{Text: "old"}}, Meta: map[string]string{internalContextMarker: "true"}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "first"}}},
		{Role: provider.RoleDeveloper, Content: []provider.Content{provider.TextBlock{Text: "new"}}, Meta: map[string]string{internalContextMarker: "true"}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "answer"}}},
	})
	if _, err := agent.Compact(context.Background(), 0, nil); err != nil {
		t.Fatal(err)
	}
	messages := agent.Messages()
	if len(messages) != 2 || messages[0].Role != provider.RoleDeveloper || internalContextText(messages[0]) != "new" {
		t.Fatalf("compacted messages = %#v, want latest internal context then summary", messages)
	}
}

func TestCompactStartsProviderRequestIdentity(t *testing.T) {
	client := &compactLifecycleClient{}
	agent := NewAgent(client, "compact-model", "system", Registry{})
	agent.SetMessages([]provider.Message{{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "source"}},
	}})
	if _, err := agent.Compact(context.Background(), 0, nil); err != nil {
		t.Fatal(err)
	}
	if client.req.Context.CacheSessionID == "" || client.req.Context.ThreadID == "" || client.req.Context.TurnID == "" {
		t.Fatalf("compaction request context = %#v, want all identities", client.req.Context)
	}
	if err := agent.BindRequestIdentity("other-cache", "other-thread"); err == nil {
		t.Fatal("BindRequestIdentity succeeded after compaction provider request")
	}
}

func TestCompactWithEventsAccumulatesAndPersistsUsage(t *testing.T) {
	client := &compactLifecycleClient{usage: []provider.Usage{{InputTokens: 2}, {OutputTokens: 3, CostUSD: 0.25}}}
	agent := NewAgent(client, "compact-model", "system", Registry{})
	agent.SetMessages([]provider.Message{{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "source"}},
	}})
	root := t.TempDir()
	sess, err := NewSession(root, t.TempDir(), "provider", "compact-model", "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	agent.OnUsage = func(cumulative provider.Usage) {
		if err := sess.AppendUsage(cumulative, cumulative); err != nil {
			t.Fatalf("persist usage: %v", err)
		}
	}
	var events []AgentEvent
	if _, err := agent.CompactWithEvents(context.Background(), 0, func(event AgentEvent) {
		events = append(events, event)
	}); err != nil {
		t.Fatalf("CompactWithEvents: %v", err)
	}
	if got, want := agent.Cost(), (provider.Usage{InputTokens: 2, OutputTokens: 3, CostUSD: 0.25}); got != want {
		t.Fatalf("agent cost = %+v, want %+v", got, want)
	}
	cum, _, err := SessionUsageDetail(sess.Path)
	if err != nil {
		t.Fatal(err)
	}
	if cum != agent.Cost() {
		t.Fatalf("persisted cumulative usage = %+v, want %+v", cum, agent.Cost())
	}
	var cumulative []provider.Usage
	for _, event := range events {
		if usage, ok := event.(EvUsage); ok {
			cumulative = append(cumulative, usage.Cumulative)
		}
	}
	if len(cumulative) != 2 || cumulative[0].InputTokens != 2 || cumulative[1] != agent.Cost() {
		t.Fatalf("usage event cumulative values = %+v, want first input=2 and final=%+v", cumulative, agent.Cost())
	}
}

func TestCompactWithEventsEmitsRequestLifecycle(t *testing.T) {
	client := &compactLifecycleClient{}
	agent := NewAgent(client, "compact-model", "system", Registry{})
	agent.SetMessages([]provider.Message{{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "source"}},
	}})

	var events []AgentEvent
	summary, err := agent.CompactWithEvents(context.Background(), 0, func(event AgentEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("CompactWithEvents: %v", err)
	}
	if summary != "summary" {
		t.Fatalf("summary = %q, want summary", summary)
	}
	if !client.hadLifecycle {
		t.Fatal("compaction request did not receive a lifecycle observer")
	}

	if len(events) != 4 {
		t.Fatalf("events = %#v, want agent request, provider request, assistant start, text delta", events)
	}
	agentRequest, ok := events[0].(EvRequestStarted)
	if !ok || agentRequest.Scope != RetryScopeAgent || agentRequest.Attempt != 1 || agentRequest.MaxAttempts != 1 {
		t.Fatalf("first event = %#v, want initial agent request", events[0])
	}
	providerRequest, ok := events[1].(EvRequestStarted)
	if !ok || providerRequest.Scope != RetryScopeProvider || providerRequest.Attempt != 1 || providerRequest.MaxAttempts != 1 {
		t.Fatalf("second event = %#v, want initial provider request", events[1])
	}
	if _, ok := events[2].(EvAssistantStart); !ok {
		t.Fatalf("third event = %#v, want assistant start", events[2])
	}
	if delta, ok := events[3].(EvTextDelta); !ok || delta.Delta != "summary" {
		t.Fatalf("fourth event = %#v, want summary text delta", events[3])
	}
}

func TestCompactPersistsProviderRetryLifecycleWithoutEventSink(t *testing.T) {
	client := &compactLifecycleClient{reportRetry: true}
	agent := NewAgent(client, "compact-model", "system", Registry{})
	agent.SetMessages([]provider.Message{textMessage(provider.RoleUser, "source")})

	var records []RetryLifecycleRecord
	agent.OnRetryLifecycle = func(record RetryLifecycleRecord) {
		records = append(records, record)
	}
	if _, err := agent.Compact(context.Background(), 0, nil); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !client.hadLifecycle {
		t.Fatal("event-less compaction request did not receive a lifecycle observer")
	}
	want := []RetryLifecycleRecord{
		{
			Event: RetryLifecycleRequestFailed, Scope: RetryScopeProvider,
			Attempt: 1, MaxAttempts: 2, Reason: RetryReasonOverload,
		},
		{
			Event: RetryLifecycleRetryScheduled, Scope: RetryScopeProvider,
			Attempt: 2, MaxAttempts: 2, Reason: RetryReasonOverload, DelayMS: 250,
		},
	}
	if !reflect.DeepEqual(records, want) {
		t.Fatalf("retry lifecycle = %#v, want %#v", records, want)
	}
}

func TestCompactPromptPreservesActiveInstructions(t *testing.T) {
	const activeInstruction = "delegate independent work to subagents"
	client := &compactLifecycleClient{summary: "Active instruction still in force: " + activeInstruction}
	agent := NewAgent(client, "compact-model", "system", Registry{})
	agent.SetMessages([]provider.Message{
		{
			Role:    provider.RoleUser,
			Content: []provider.Content{provider.TextBlock{Text: "When coding, delegate independent work to subagents."}},
		},
		{
			Role:    provider.RoleAssistant,
			Content: []provider.Content{provider.TextBlock{Text: "I will do that."}},
		},
	})

	if _, err := agent.Compact(context.Background(), 0, nil); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	messages := agent.Messages()
	if len(messages) != 1 {
		t.Fatalf("compacted messages = %d, want 1", len(messages))
	}
	if messages[0].Role != provider.RoleUser {
		t.Fatalf("compacted message role = %q, want user", messages[0].Role)
	}
	if messages[0].Meta["compaction"] != "true" {
		t.Fatalf("compacted message meta = %#v, want compaction=true", messages[0].Meta)
	}
	compacted, ok := messages[0].Content[0].(provider.TextBlock)
	if !ok {
		t.Fatalf("compacted message content = %#v, want TextBlock", messages[0].Content[0])
	}
	if !strings.Contains(compacted.Text, "## Context Summary (compacted)") || !strings.Contains(compacted.Text, activeInstruction) {
		t.Fatalf("compacted transcript did not preserve active instruction:\n%s", compacted.Text)
	}
	if !strings.Contains(client.req.System, "Preserve active user instructions") {
		t.Fatalf("compaction system prompt does not preserve active instructions: %q", client.req.System)
	}
	if len(client.req.Messages) != 1 {
		t.Fatalf("compaction request messages = %d, want 1", len(client.req.Messages))
	}
	text, ok := client.req.Messages[0].Content[0].(provider.TextBlock)
	if !ok {
		t.Fatalf("compaction request content = %#v, want TextBlock", client.req.Messages[0].Content[0])
	}
	for _, want := range []string{"## Active Instructions & Preferences", "delegation/subagent", activeInstruction} {
		if !strings.Contains(text.Text, want) {
			t.Fatalf("compaction prompt missing %q:\n%s", want, text.Text)
		}
	}
}
