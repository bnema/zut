package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
)

// continuationEchoTool is a deterministic test tool: it records how often
// it ran so the continuation regression can assert exactly one execution.
type continuationEchoTool struct {
	calls int32
}

func (t *continuationEchoTool) Name() string            { return "echo" }
func (t *continuationEchoTool) Description() string     { return "echoes" }
func (t *continuationEchoTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (t *continuationEchoTool) Execute(_ context.Context, _ json.RawMessage, _ func(string)) (ToolResult, error) {
	atomic.AddInt32(&t.calls, 1)
	return ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: "tool ok"}},
	}, nil
}

func (t *continuationEchoTool) Calls() int32 { return atomic.LoadInt32(&t.calls) }

// TestAgentContinuationExplicitEndTurnFalse scripts a real Responses SSE
// stack: interim text with explicit end_turn:false, then a tool call, then
// final text. The agent must issue three requests, execute the tool once,
// keep transcript pairing/order without injected user rows, and finish with
// exactly one terminal EvDone.
func TestAgentContinuationExplicitEndTurnFalse(t *testing.T) {
	var (
		mu       sync.Mutex
		requests int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		w.Header().Set("content-type", "text/event-stream")
		switch n {
		case 1:
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\"}}\n\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"part one\"}\n\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"end_turn\":false,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		case 2:
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call-1\",\"name\":\"echo\"}}\n\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"{}\"}\n\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\"}}\n\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-2\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		default:
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\"}}\n\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"final\"}\n\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-3\",\"end_turn\":true,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		}
	}))
	defer srv.Close()

	client := provider.NewOpenAIResponsesNamed("synthetic-key", srv.URL, "openai-responses")
	tool := &continuationEchoTool{}
	agent := NewAgent(client, "gpt-5.2", "system", Registry{"echo": tool})

	var (
		evMu     sync.Mutex
		events   []AgentEvent
		doneSeen int
	)
	sink := func(ev AgentEvent) {
		evMu.Lock()
		defer evMu.Unlock()
		events = append(events, ev)
		if _, ok := ev.(EvDone); ok {
			doneSeen++
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := agent.Prompt(ctx, "start", nil, sink); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}

	mu.Lock()
	gotRequests := requests
	mu.Unlock()
	if gotRequests != 3 {
		t.Fatalf("provider requests = %d, want 3 (continuation + tool + final)", gotRequests)
	}
	if got := tool.Calls(); got != 1 {
		t.Fatalf("tool executions = %d, want 1", got)
	}

	evMu.Lock()
	defer evMu.Unlock()
	if doneSeen != 1 {
		t.Fatalf("terminal EvDone count = %d, want 1", doneSeen)
	}

	// Transcript pairing/order: user, interim assistant, tool call,
	// tool result, final assistant. No injected user rows.
	msgs := agent.Messages()
	if len(msgs) != 5 {
		var roles []string
		for _, m := range msgs {
			roles = append(roles, string(m.Role))
		}
		t.Fatalf("transcript messages = %d (%v), want 5", len(msgs), roles)
	}
	wantRoles := []provider.Role{
		provider.RoleUser,
		provider.RoleAssistant,
		provider.RoleAssistant,
		provider.RoleTool,
		provider.RoleAssistant,
	}
	for i, want := range wantRoles {
		if msgs[i].Role != want {
			t.Fatalf("message %d role = %q, want %q", i, msgs[i].Role, want)
		}
	}
	if got := extractText(msgs[1]); got != "part one" {
		t.Fatalf("interim assistant text = %q, want %q", got, "part one")
	}
	var callID string
	for _, c := range msgs[2].Content {
		if tc, ok := c.(provider.ToolCallBlock); ok {
			callID = tc.ID
			if tc.Name != "echo" {
				t.Fatalf("tool call name = %q, want echo", tc.Name)
			}
		}
	}
	if callID == "" {
		t.Fatalf("assistant tool message carries no tool call: %#v", msgs[2].Content)
	}
	var resultCallID string
	for _, c := range msgs[3].Content {
		if tr, ok := c.(provider.ToolResultBlock); ok {
			resultCallID = tr.CallID
		}
	}
	if resultCallID != callID {
		t.Fatalf("tool result call ID = %q, want paired %q", resultCallID, callID)
	}
	if got := extractText(msgs[4]); !strings.Contains(got, "final") {
		t.Fatalf("final assistant text = %q, want it to contain %q", got, "final")
	}
}
