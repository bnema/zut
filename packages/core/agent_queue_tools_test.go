package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

type queueToolsClient struct {
	turnRecoveryScriptClient
	requests []provider.Request
}

func (c *queueToolsClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.requests = append(c.requests, req)
	return c.turnRecoveryScriptClient.Stream(ctx, req)
}

type queueDuringTool struct {
	agent  *Agent
	prompt *QueuedMessage
	calls  int
}

func (*queueDuringTool) Name() string            { return "queue" }
func (*queueDuringTool) Description() string     { return "queue input" }
func (*queueDuringTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (q *queueDuringTool) Execute(context.Context, json.RawMessage, func(string)) (ToolResult, error) {
	q.calls++
	if q.calls == 1 && q.prompt != nil && !q.agent.QueuePrompt(*q.prompt) {
		return ToolResult{}, fmt.Errorf("queue rejected")
	}
	return ToolResult{Content: []provider.Content{provider.TextBlock{Text: "done"}}}, nil
}

func TestQueuedInputBetweenTools(t *testing.T) {
	for _, name := range []string{"user", "host", "none"} {
		t.Run(name, func(t *testing.T) {
			var calls []provider.Content
			for i := 1; i <= 3; i++ {
				calls = append(calls, provider.ToolCallBlock{ID: fmt.Sprint(i), Name: "queue", Arguments: json.RawMessage(`{}`)})
			}
			client := &queueToolsClient{turnRecoveryScriptClient: turnRecoveryScriptClient{turns: []turnRecoveryTurn{{content: calls, stop: provider.StopToolUse}, {content: []provider.Content{provider.TextBlock{Text: "finished"}}, stop: provider.StopEnd}}}}
			tool := &queueDuringTool{}
			if name != "none" {
				tool.prompt = &QueuedMessage{Text: "new direction", HostEvent: name == "host"}
			}
			agent := NewAgent(client, "test", "", Registry{"queue": tool})
			tool.agent = agent
			var committed, observed []ToolResult
			agent.CommitToolResult = func(_ string, r ToolResult) error { committed = append(committed, r); return nil }
			agent.OnToolResult = func(_ string, r ToolResult) { observed = append(observed, r) }
			var events []EvToolResult
			if err := agent.Prompt(context.Background(), "start", nil, func(ev AgentEvent) {
				if r, ok := ev.(EvToolResult); ok {
					events = append(events, r)
				}
			}); err != nil {
				t.Fatal(err)
			}
			wantCalls := 3
			if name == "user" {
				wantCalls = 1
			}
			if tool.calls != wantCalls {
				t.Fatalf("executions = %d, want %d", tool.calls, wantCalls)
			}
			if len(committed) != 3 || len(observed) != 3 || len(events) != 3 {
				t.Fatalf("result hooks/events: %d/%d/%d", len(committed), len(observed), len(events))
			}
			if len(client.requests) != 2 {
				t.Fatalf("requests = %d", len(client.requests))
			}
			messages := client.requests[1].Messages
			if len(messages) < 3 || messages[2].Role != provider.RoleTool {
				t.Fatalf("missing tool message: %+v", messages)
			}
			results := messages[2].Content
			if len(results) != 3 {
				t.Fatalf("results = %d", len(results))
			}
			for i, content := range results {
				result := content.(provider.ToolResultBlock)
				skipped := name == "user" && i > 0
				if result.CallID != fmt.Sprint(i+1) || result.IsError != skipped || committed[i].IsError != skipped || observed[i].IsError != skipped || events[i].ID != result.CallID || events[i].Result.IsError != skipped {
					t.Fatalf("result %d: %+v", i, result)
				}
				if skipped && !strings.HasPrefix(result.Content[0].(provider.TextBlock).Text, "Skipped:") {
					t.Fatalf("missing skip explanation: %+v", result)
				}
			}
			if name != "none" {
				if len(messages) != 4 || messages[3].Role != provider.RoleUser || messages[3].Content[len(messages[3].Content)-1].(provider.TextBlock).Text != "new direction" {
					t.Fatalf("queued input not after results: %+v", messages)
				}
			} else if len(messages) != 3 {
				t.Fatalf("unexpected messages: %+v", messages)
			}
			// The actual executed-call subset, not synthetic skip results, feeds the guard.
			if agent.QueuedMessageCount() != 0 {
				t.Fatal("queue not drained")
			}
		})
	}
}

func TestSkippedToolsDoNotProduceRepetitionObservations(t *testing.T) {
	tool := &queueDuringTool{prompt: &QueuedMessage{Text: "steer"}}
	agent := NewAgent(nil, "test", "", Registry{"queue": tool})
	tool.agent = agent
	msg := provider.Message{Content: []provider.Content{
		provider.ToolCallBlock{ID: "1", Name: "queue", Arguments: json.RawMessage(`{}`)},
		provider.ToolCallBlock{ID: "2", Name: "queue", Arguments: json.RawMessage(`{}`)},
	}}
	_, _, observations := agent.executeToolsWithObservations(context.Background(), msg, func(AgentEvent) {})
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want only executed call", len(observations))
	}
}
