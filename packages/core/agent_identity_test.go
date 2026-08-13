package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
)

type identityRecordingClient struct {
	mu       sync.Mutex
	requests []provider.Request
	failOnce bool
}

func (c *identityRecordingClient) Name() string { return "identity-recording" }

func (c *identityRecordingClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	fail := c.failOnce
	c.failOnce = false
	c.mu.Unlock()

	out := make(chan provider.Event, 2)
	out <- provider.EventStart{Provider: c.Name(), Model: req.Model}
	if fail {
		out <- provider.EventDone{Stop: provider.StopError, Err: errors.New("anthropic overloaded_error: overloaded")}
	} else {
		out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
			Role: provider.RoleAssistant,
			Content: []provider.Content{
				provider.TextBlock{Text: "done"},
			},
		}}
	}
	close(out)
	return out, nil
}

func (c *identityRecordingClient) Requests() []provider.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	requests := make([]provider.Request, len(c.requests))
	copy(requests, c.requests)
	return requests
}

func TestAgentRequestContextPreservesSessionAndTurnIdentities(t *testing.T) {
	client := &identityRecordingClient{failOnce: true}
	agent := NewAgent(client, "test-model", "system", Registry{})
	agent.MaxRetries = 1
	agent.RetryBaseDelay = time.Millisecond
	if err := agent.BindSessionID("session-1"); err != nil {
		t.Fatalf("BindSessionID: %v", err)
	}

	if err := agent.Prompt(context.Background(), "first", nil, nil); err != nil {
		t.Fatalf("first prompt: %v", err)
	}
	if err := agent.Prompt(context.Background(), "second", nil, nil); err != nil {
		t.Fatalf("second prompt: %v", err)
	}

	requests := client.Requests()
	if len(requests) != 3 {
		t.Fatalf("request count = %d, want 3", len(requests))
	}
	for i, request := range requests {
		if request.Context.SessionID != "session-1" {
			t.Fatalf("request %d session ID = %q, want session-1", i, request.Context.SessionID)
		}
		if request.Context.TurnID == "" {
			t.Fatalf("request %d has empty turn ID", i)
		}
	}
	if requests[0].Context.TurnID != requests[1].Context.TurnID {
		t.Fatalf("retry turn IDs = %q and %q, want one stable ID", requests[0].Context.TurnID, requests[1].Context.TurnID)
	}
	if requests[1].Context.TurnID == requests[2].Context.TurnID {
		t.Fatalf("prompt turn IDs = %q and %q, want distinct IDs", requests[1].Context.TurnID, requests[2].Context.TurnID)
	}
	if err := agent.BindSessionID("other-session"); err == nil {
		t.Fatal("BindSessionID succeeded after the first provider request")
	}
}

func TestAgentNoSessionIdentityIsStableForRun(t *testing.T) {
	client := &identityRecordingClient{}
	agent := NewAgent(client, "test-model", "system", Registry{})

	if err := agent.Prompt(context.Background(), "first", nil, nil); err != nil {
		t.Fatalf("first prompt: %v", err)
	}
	if err := agent.Prompt(context.Background(), "second", nil, nil); err != nil {
		t.Fatalf("second prompt: %v", err)
	}
	requests := client.Requests()
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if requests[0].Context.SessionID == "" || requests[0].Context.SessionID != requests[1].Context.SessionID {
		t.Fatalf("session IDs = %q and %q, want stable non-empty identity", requests[0].Context.SessionID, requests[1].Context.SessionID)
	}
}
