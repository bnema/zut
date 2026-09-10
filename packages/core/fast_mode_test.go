package core

import (
	"context"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

type fastModeTestClient struct {
	name    string
	lastReq provider.Request
	called  bool
}

func (c *fastModeTestClient) Name() string { return c.name }

func (c *fastModeTestClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.called = true
	c.lastReq = req
	out := make(chan provider.Event, 2)
	out <- provider.EventTextDelta{Delta: "ok"}
	out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.TextBlock{Text: "ok"}},
	}}
	close(out)
	return out, nil
}

func TestAgentPropagatesFastMode(t *testing.T) {
	client := &fastModeTestClient{name: "openai"}
	agent := NewAgent(client, "gpt-5", "", nil)
	agent.FastMode = true

	if err := agent.Prompt(context.Background(), "hello", nil, nil); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if !client.called {
		t.Fatal("provider was not called")
	}
	if !client.lastReq.FastMode {
		t.Fatal("request FastMode = false, want true")
	}
}

func TestAgentPropagatesFastModeToCompaction(t *testing.T) {
	client := &fastModeTestClient{name: "openai"}
	agent := NewAgent(client, "gpt-5", "", nil)
	agent.FastMode = true
	agent.SetMessages([]provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "old"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "answer"}}},
	})

	if _, err := agent.Compact(context.Background(), 1, nil); err != nil {
		t.Fatalf("Compact returned %v", err)
	}
	if !client.lastReq.FastMode {
		t.Fatal("compaction request FastMode = false, want true")
	}
}

func TestAgentRejectsFastModeForNonOpenAIProvider(t *testing.T) {
	client := &fastModeTestClient{name: "anthropic"}
	agent := NewAgent(client, "claude-sonnet-4-5", "", nil)
	agent.FastMode = true

	err := agent.Prompt(context.Background(), "hello", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "only supported for OpenAI providers") {
		t.Fatalf("Prompt error = %v, want unsupported-provider error", err)
	}
	if client.called {
		t.Fatal("provider was called despite unsupported fast mode")
	}
}

func TestAgentRejectsFastModeForNonOpenAIProviderDuringCompaction(t *testing.T) {
	client := &fastModeTestClient{name: "anthropic"}
	agent := NewAgent(client, "claude-sonnet-4-5", "", nil)
	agent.FastMode = true
	agent.SetMessages([]provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "old"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "answer"}}},
	})

	_, err := agent.Compact(context.Background(), 1, nil)
	if err == nil || !strings.Contains(err.Error(), "only supported for OpenAI providers") {
		t.Fatalf("Compact error = %v, want unsupported-provider error", err)
	}
	if client.called {
		t.Fatal("provider was called despite unsupported fast mode")
	}
}
