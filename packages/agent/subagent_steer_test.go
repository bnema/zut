package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// A follow-up steered while the child waits on the provider reaches the next
// model request of the same turn and is persisted as a user message, so the
// child reads it without waiting for its turn to end (issue 226).
func TestResidentAgentRuntimeDeliversSteerMidTurn(t *testing.T) {
	firstRequest := make(chan struct{})
	releaseFirst := make(chan struct{})
	var requests atomic.Int32
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		number := requests.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		encoded, _ := json.Marshal(body["messages"])
		mu.Lock()
		bodies = append(bodies, string(encoded))
		mu.Unlock()
		w.Header().Set("content-type", "text/event-stream")
		if number == 1 {
			close(firstRequest)
			<-releaseFirst
			writeOpenAIChunk(t, w, map[string]any{
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "id": "call-1", "type": "function",
					"function": map[string]any{"name": "read", "arguments": `{"path":"missing.txt"}`},
				}}}, "finish_reason": "tool_calls"}},
			})
			return
		}
		writeOpenAIChunk(t, w, map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": "wrapped up"}, "finish_reason": "stop"}},
		})
	}))
	defer server.Close()

	runtime, journal, _ := newResidentChildTestRunner(t, "steer-child", Args{
		Provider: "openai", Model: "gpt-4o", APIKey: "synthetic", BaseURL: server.URL,
		CWD: t.TempDir(), NoContextFiles: true, NoSkill: true,
	})
	if runtime.Steer("too early") {
		t.Fatal("Steer accepted a follow-up with no running turn")
	}
	done := make(chan error, 1)
	go func() { done <- runtime.Run(t.Context(), "investigate") }()
	select {
	case <-firstRequest:
	case <-time.After(5 * time.Second):
		t.Fatal("first provider request did not arrive")
	}
	if !runtime.Steer("stop investigating and report now") {
		t.Fatal("Steer rejected a follow-up during a running turn")
	}
	if got := runtime.PendingSteers(); got != 1 {
		t.Fatalf("pending steers = %d, want 1", got)
	}
	close(releaseFirst)
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := runtime.PendingSteers(); got != 0 {
		t.Fatalf("pending steers after turn = %d, want 0", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || !strings.Contains(bodies[1], "stop investigating and report now") {
		t.Fatalf("provider requests = %#v, want the steer in the second request", bodies)
	}
	messages, err := subagents.ReadResidentTranscriptMessages(journal.Dir())
	if err != nil {
		t.Fatal(err)
	}
	var users []string
	for _, message := range messages {
		if message.Role != provider.RoleUser {
			continue
		}
		for _, content := range message.Content {
			if text, ok := content.(provider.TextBlock); ok {
				users = append(users, text.Text)
			}
		}
	}
	if len(users) != 2 || users[1] != "stop investigating and report now" {
		t.Fatalf("journaled user messages = %#v", users)
	}
}

// The handshake queues steering before the first idle timer starts, without sleeps.
type stalledSteerClient struct {
	started   chan struct{}
	release   chan struct{}
	calls     int
	delivered bool
}

func (*stalledSteerClient) Name() string { return "stalled-steer" }
func (c *stalledSteerClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.calls++
	out := make(chan provider.Event, 1)
	if c.calls == 1 {
		close(c.started)
		select {
		case <-c.release:
		case <-ctx.Done():
		}
		go func() { <-ctx.Done(); close(out) }()
		return out, nil
	}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if text, ok := b.(provider.TextBlock); ok && text.Text == "report now" {
				c.delivered = true
			}
		}
	}
	out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "done"}}}}
	close(out)
	return out, nil
}
func TestResidentAgentRuntimeDeliversSteerOnIdleRetry(t *testing.T) {
	client := &stalledSteerClient{started: make(chan struct{}), release: make(chan struct{})}
	a := core.NewAgent(client, "model", "", core.Registry{})
	a.StreamIdleTimeout = 5 * time.Millisecond
	a.MaxRetries = 0
	runtime := &residentAgentRuntime{agent: a, run: func(ctx context.Context, prompt string) error { return a.Prompt(ctx, prompt, nil, nil) }}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx, "investigate") }()
	select {
	case <-client.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !runtime.Steer("report now") {
		t.Fatal("steer rejected")
	}
	close(client.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if client.calls != 2 || !client.delivered {
		t.Fatalf("calls=%d delivered=%v", client.calls, client.delivered)
	}
	if pending := runtime.DrainSteers(); len(pending) != 0 {
		t.Fatalf("undelivered: %v", pending)
	}
}
