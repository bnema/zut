package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
)

// responsesReasoningRecorder serves synthetic Responses SSE while recording
// raw request bodies for stateless-replay assertions.
type responsesReasoningRecorder struct {
	mu     sync.Mutex
	bodies []string
}

func (r *responsesReasoningRecorder) handler(summary string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.bodies = append(r.bodies, string(body))
		r.mu.Unlock()
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\"}}\n\n")
		encoded, _ := json.Marshal(summary)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":"+string(encoded)+"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-test\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	}
}

func (r *responsesReasoningRecorder) requestBodies() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.bodies...)
}

func consumeResponsesStream(t *testing.T, client provider.Client, req provider.Request) {
	t.Helper()
	stream, err := client.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	for ev := range stream {
		if done, ok := ev.(provider.EventDone); ok && done.Err != nil {
			t.Fatalf("stream error: %v", done.Err)
		}
	}
}

func findStoredReasoning(msgs []provider.Message, id string) *provider.ReasoningBlock {
	for _, msg := range msgs {
		for _, c := range msg.Content {
			if rb, ok := c.(provider.ReasoningBlock); ok && rb.ID == id {
				clone := rb
				return &clone
			}
		}
	}
	return nil
}

func TestSessionResponsesReasoningReplay(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	session, err := NewSession(root, cwd, "openai-responses", "gpt-5", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "hello"}},
		Time:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(provider.Message{
		Role: provider.RoleAssistant,
		Content: []provider.Content{
			provider.ReasoningBlock{ID: "rs_stale"},
			provider.TextBlock{Text: "visible answer"},
			provider.ToolCallBlock{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`)},
		},
		Time: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(provider.Message{
		Role: provider.RoleTool,
		Content: []provider.Content{provider.ToolResultBlock{
			CallID:  "call-1",
			Content: []provider.Content{provider.TextBlock{Text: "result"}},
		}},
		Time: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	path := session.Path
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := ReadSessionSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if stored := findStoredReasoning(snapshot.Messages, "rs_stale"); stored == nil {
		t.Fatalf("snapshot lost ID-only reasoning: %#v", snapshot.Messages)
	}

	forkedPath, err := BranchSession(path, root, cwd, "test", len(snapshot.Messages))
	if err != nil {
		t.Fatal(err)
	}
	forked, err := ReadSessionSnapshot(forkedPath)
	if err != nil {
		t.Fatal(err)
	}
	if stored := findStoredReasoning(forked.Messages, "rs_stale"); stored == nil {
		t.Fatalf("forked transcript lost ID-only reasoning: %#v", forked.Messages)
	}

	for _, msgs := range [][]provider.Message{snapshot.Messages, forked.Messages} {
		recorder := &responsesReasoningRecorder{}
		server := httptest.NewServer(recorder.handler("ok"))
		client := provider.NewOpenAIResponsesNamed("synthetic-key", server.URL, "openai-responses")
		consumeResponsesStream(t, client, provider.Request{
			Model:    "gpt-5",
			Messages: msgs,
		})
		server.Close()

		bodies := recorder.requestBodies()
		if len(bodies) != 1 {
			t.Fatalf("requests = %d, want 1", len(bodies))
		}
		body := bodies[0]
		if strings.Contains(body, `"type":"reasoning"`) || strings.Contains(body, "rs_stale") {
			t.Fatalf("ID-only reasoning leaked into Responses request: %s", body)
		}
		for _, want := range []string{"visible answer", "lookup", "result"} {
			if !strings.Contains(body, want) {
				t.Fatalf("request body missing %q: %s", want, body)
			}
		}
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("source session bytes changed during replay")
	}
	reread, err := ReadSessionSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if stored := findStoredReasoning(reread.Messages, "rs_stale"); stored == nil {
		t.Fatal("stored reasoning was modified by replay")
	}
}

func TestCompactResponsesReasoningReplay(t *testing.T) {
	recorder := &responsesReasoningRecorder{}
	server := httptest.NewServer(recorder.handler("synthetic summary"))
	defer server.Close()
	client := provider.NewOpenAIResponsesNamed("synthetic-key", server.URL, "openai-responses")

	agent := NewAgent(client, "gpt-5", "system", Registry{})
	agent.SetMessages([]provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "start"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{
			provider.ToolCallBlock{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`)},
		}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "continue"}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{
			CallID:  "call-1",
			Content: []provider.Content{provider.TextBlock{Text: "orphaned result"}},
		}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{
			provider.ReasoningBlock{ID: "rs_stale"},
			provider.TextBlock{Text: "tail answer"},
		}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "next question"}}},
	})

	if _, err := agent.Compact(context.Background(), 4, nil); err != nil {
		t.Fatal(err)
	}
	bodies := recorder.requestBodies()
	if len(bodies) < 1 {
		t.Fatal("summary request was not recorded")
	}
	if strings.Contains(bodies[0], `"type":"reasoning"`) || strings.Contains(bodies[0], "rs_stale") {
		t.Fatalf("summary request contains reasoning item: %s", bodies[0])
	}

	compacted := agent.Messages()
	if stored := findStoredReasoning(compacted, "rs_stale"); stored == nil {
		t.Fatalf("compacted tail lost retained reasoning block: %#v", compacted)
	}

	consumeResponsesStream(t, client, provider.Request{Model: "gpt-5", Messages: compacted})
	bodies = recorder.requestBodies()
	if len(bodies) != 2 {
		t.Fatalf("requests = %d, want summary plus replay", len(bodies))
	}
	replay := bodies[1]
	if strings.Contains(replay, "rs_stale") || strings.Contains(replay, `"type":"reasoning"`) {
		t.Fatalf("retained invalid reference reached Responses request: %s", replay)
	}
	if strings.Contains(replay, "orphaned result") || strings.Contains(replay, "call-1") {
		t.Fatalf("orphaned tool result reached Responses request: %s", replay)
	}
	for _, want := range []string{"tail answer", "next question"} {
		if !strings.Contains(replay, want) {
			t.Fatalf("replay request missing %q: %s", want, replay)
		}
	}
}
