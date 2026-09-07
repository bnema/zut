package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
)

// continuationScriptClient replays a scripted stop reason per provider call
// and records every request for identity/history assertions.
type continuationScriptClient struct {
	mu       sync.Mutex
	requests []provider.Request
	calls    int32
	// continuations is the number of leading calls that return
	// StopContinue before one final StopEnd.
	continuations int32
}

func (c *continuationScriptClient) Name() string { return "continuation-script" }

func (c *continuationScriptClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	call := atomic.AddInt32(&c.calls, 1)
	c.mu.Lock()
	c.requests = append(c.requests, req)
	c.mu.Unlock()
	out := make(chan provider.Event, 4)
	out <- provider.EventStart{Provider: c.Name(), Model: req.Model}
	text := "final"
	stop := provider.StopEnd
	if call <= c.continuations {
		text = fmt.Sprintf("part-%d", call)
		stop = provider.StopContinue
	}
	out <- provider.EventTextDelta{Delta: text}
	out <- provider.EventUsage{Usage: provider.Usage{InputTokens: 1, OutputTokens: 1}}
	out <- provider.EventDone{Stop: stop, Message: provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.TextBlock{Text: text}},
	}}
	close(out)
	return out, nil
}

func (c *continuationScriptClient) Calls() int32 { return atomic.LoadInt32(&c.calls) }

func (c *continuationScriptClient) Requests() []provider.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]provider.Request, len(c.requests))
	copy(out, c.requests)
	return out
}

type continuationEvents struct {
	mu        sync.Mutex
	turnEnds  []EvTurnEnd
	doneCount int
}

func (e *continuationEvents) sink(ev AgentEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if te, ok := ev.(EvTurnEnd); ok {
		e.turnEnds = append(e.turnEnds, te)
	}
	if _, ok := ev.(EvDone); ok {
		e.doneCount++
	}
}

func (e *continuationEvents) snapshot() ([]EvTurnEnd, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]EvTurnEnd, len(e.turnEnds))
	copy(out, e.turnEnds)
	return out, e.doneCount
}

// TestAgentContinuationTwelveStepsThenDone pins AC3: 12 text-only explicit
// continuations followed by a final response produce 13 requests and one
// terminal completion, with no artificial user/tool rows.
func TestAgentContinuationTwelveStepsThenDone(t *testing.T) {
	client := &continuationScriptClient{continuations: 12}
	agent := NewAgent(client, "test-model", "system", Registry{})
	events := &continuationEvents{}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := agent.Prompt(ctx, "start", nil, events.sink); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if got := client.Calls(); got != 13 {
		t.Fatalf("provider calls = %d, want 13", got)
	}
	turnEnds, doneCount := events.snapshot()
	if doneCount != 1 {
		t.Fatalf("terminal EvDone count = %d, want 1", doneCount)
	}
	if len(turnEnds) != 13 {
		t.Fatalf("EvTurnEnd count = %d, want 13", len(turnEnds))
	}
	for i, te := range turnEnds {
		want := provider.StopContinue
		if i == 12 {
			want = provider.StopEnd
		}
		if te.Stop != want || te.Err != nil {
			t.Fatalf("turn %d end = (%q, %v), want (%q, nil)", i+1, te.Stop, te.Err, want)
		}
	}

	msgs := agent.Messages()
	if len(msgs) != 14 {
		t.Fatalf("transcript messages = %d, want 1 user + 13 assistant", len(msgs))
	}
	if msgs[0].Role != provider.RoleUser {
		t.Fatalf("message 0 role = %q, want user", msgs[0].Role)
	}
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role != provider.RoleAssistant {
			t.Fatalf("message %d role = %q, want assistant (no artificial rows)", i, msgs[i].Role)
		}
		if len(msgs[i].Content) == 0 {
			t.Fatalf("message %d has no content", i)
		}
	}

	// Turn/thread identity is stable across the whole logical turn and
	// every continuation sees prior assistant content.
	requests := client.Requests()
	turnID := requests[0].Context.TurnID
	if turnID == "" {
		t.Fatal("first request has empty turn ID")
	}
	for i, req := range requests {
		if req.Context.TurnID != turnID {
			t.Fatalf("request %d turn ID = %q, want stable %q", i, req.Context.TurnID, turnID)
		}
		if want := 1 + i; len(req.Messages) != want {
			t.Fatalf("request %d messages = %d, want %d (prior content retained)", i, len(req.Messages), want)
		}
	}

	// Usage is counted once per inference.
	if got := agent.Cost(); got.InputTokens != 13 || got.OutputTokens != 13 {
		t.Fatalf("cumulative usage = %+v, want 13 input / 13 output", got)
	}
	if got := extractText(msgs[len(msgs)-1]); got != "final" {
		t.Fatalf("final text = %q, want %q", got, "final")
	}
}

// TestAgentContinuationCanceledAtBoundary pins the canceled path: no
// further inference is requested and the cancellation error surfaces.
func TestAgentContinuationCanceledAtBoundary(t *testing.T) {
	client := &continuationScriptClient{continuations: 12}
	agent := NewAgent(client, "test-model", "system", Registry{})
	events := &continuationEvents{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var once sync.Once
	sink := func(ev AgentEvent) {
		events.sink(ev)
		if te, ok := ev.(EvTurnEnd); ok && te.Stop == provider.StopContinue {
			once.Do(cancel)
		}
	}
	err := agent.Prompt(ctx, "start", nil, sink)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Prompt error = %v, want context.Canceled", err)
	}
	if got := client.Calls(); got != 1 {
		t.Fatalf("provider calls after cancel = %d, want 1", got)
	}
}

// TestAgentContinuationRespectsMaxSteps pins that configured step limits
// still bound continuations with the existing typed error.
func TestAgentContinuationRespectsMaxSteps(t *testing.T) {
	client := &continuationScriptClient{continuations: 12}
	agent := NewAgent(client, "test-model", "system", Registry{})
	agent.MaxSteps = 2

	err := agent.Prompt(context.Background(), "start", nil, nil)
	if !errors.Is(err, ErrMaxSteps) {
		t.Fatalf("Prompt error = %v, want ErrMaxSteps", err)
	}
	if got := client.Calls(); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}
}

// TestAgentContinuationGuardBlocksNextRequest pins that turn guards stay
// active across the continuation boundary.
func TestAgentContinuationGuardBlocksNextRequest(t *testing.T) {
	client := &continuationScriptClient{continuations: 12}
	agent := NewAgent(client, "test-model", "system", Registry{})
	agent.BeforeTurn = func(step int) (bool, string) {
		if step > 1 {
			return false, "blocked at step 2"
		}
		return true, ""
	}
	events := &continuationEvents{}

	if err := agent.Prompt(context.Background(), "start", nil, events.sink); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if got := client.Calls(); got != 1 {
		t.Fatalf("provider calls = %d, want 1 (guard blocks the next request)", got)
	}
	turnEnds, doneCount := events.snapshot()
	if doneCount != 1 {
		t.Fatalf("terminal EvDone count = %d, want 1", doneCount)
	}
	if len(turnEnds) != 2 {
		t.Fatalf("EvTurnEnd count = %d, want continuation + guard", len(turnEnds))
	}
	if turnEnds[0].Stop != provider.StopContinue {
		t.Fatalf("first turn end = %q, want %q", turnEnds[0].Stop, provider.StopContinue)
	}
	if turnEnds[1].Stop != provider.StopError || turnEnds[1].Err == nil {
		t.Fatalf("guard turn end = (%q, %v), want (error, non-nil)", turnEnds[1].Stop, turnEnds[1].Err)
	}
}

// TestAgentContinuationConsumesQueuedInput pins that user text queued
// during a continuation is delivered at the normal boundary, answered by
// the next inference, and never synthesized by the loop itself.
func TestAgentContinuationConsumesQueuedInput(t *testing.T) {
	client := &continuationScriptClient{continuations: 1}
	agent := NewAgent(client, "test-model", "system", Registry{})
	events := &continuationEvents{}

	var once sync.Once
	sink := func(ev AgentEvent) {
		events.sink(ev)
		if te, ok := ev.(EvTurnEnd); ok && te.Stop == provider.StopContinue {
			once.Do(func() {
				if !agent.QueueMessage("follow-up", nil) {
					t.Error("QueueMessage returned false")
				}
			})
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := agent.Prompt(ctx, "start", nil, sink); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if got := client.Calls(); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}
	requests := client.Requests()
	if len(requests) != 2 {
		t.Fatalf("recorded requests = %d, want 2", len(requests))
	}
	var sawQueued bool
	for _, m := range requests[1].Messages {
		if m.Role == provider.RoleUser && strings.Contains(extractText(m), "follow-up") {
			sawQueued = true
		}
	}
	if !sawQueued {
		t.Fatalf("second request did not carry queued input: %#v", requests[1].Messages)
	}
	msgs := agent.Messages()
	var userCount int
	for _, m := range msgs {
		if m.Role == provider.RoleUser {
			userCount++
		}
	}
	if userCount != 2 {
		t.Fatalf("user messages = %d, want initial + queued", userCount)
	}
	if _, doneCount := events.snapshot(); doneCount != 1 {
		t.Fatalf("terminal EvDone count = %d, want 1", doneCount)
	}
}
