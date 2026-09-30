package modes

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

type repetitiveInteractiveRequest struct {
	call            int32
	hasQueuedPrompt bool
	userText        []string
}

type repetitiveInteractiveClient struct {
	calls    atomic.Int32
	entered  chan struct{}
	resume   chan struct{}
	requests chan repetitiveInteractiveRequest
}

func (*repetitiveInteractiveClient) Name() string { return "repetitive-interactive-test" }

func (c *repetitiveInteractiveClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	call := c.calls.Add(1)
	hasQueuedPrompt := false
	var userText []string
	for _, message := range req.Messages {
		if message.Role != provider.RoleUser {
			continue
		}
		for _, content := range message.Content {
			if text, ok := content.(provider.TextBlock); ok {
				userText = append(userText, text.Text)
				if strings.Contains(text.Text, "new request") {
					hasQueuedPrompt = true
				}
			}
		}
	}
	if c.requests != nil {
		c.requests <- repetitiveInteractiveRequest{call: call, hasQueuedPrompt: hasQueuedPrompt, userText: userText}
	}
	if call == 8 && c.entered != nil {
		close(c.entered)
		<-c.resume
	}
	if call > 8 {
		out := make(chan provider.Event, 1)
		out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
			Role:    provider.RoleAssistant,
			Content: []provider.Content{provider.TextBlock{Text: "answered"}},
		}}
		close(out)
		return out, nil
	}
	out := make(chan provider.Event, 4)
	out <- provider.EventStart{Provider: c.Name(), Model: req.Model}
	out <- provider.EventToolStart{ID: "call", Name: "repeat"}
	out <- provider.EventToolEnd{ID: "call"}
	out <- provider.EventDone{Stop: provider.StopToolUse, Message: provider.Message{
		Role: provider.RoleAssistant,
		Content: []provider.Content{provider.ToolCallBlock{
			ID: "call", Name: "repeat", Arguments: json.RawMessage(`{}`),
		}},
	}}
	close(out)
	return out, nil
}

type repetitiveInteractiveTool struct{}

func (*repetitiveInteractiveTool) Name() string        { return "repeat" }
func (*repetitiveInteractiveTool) Description() string { return "repeats" }
func (*repetitiveInteractiveTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (*repetitiveInteractiveTool) Execute(context.Context, json.RawMessage, func(string)) (core.ToolResult, error) {
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "same"}}}, nil
}

func TestInteractiveRepetitionGuardPreservesQueuedUserPrompt(t *testing.T) {
	client := &repetitiveInteractiveClient{
		entered:  make(chan struct{}),
		resume:   make(chan struct{}),
		requests: make(chan repetitiveInteractiveRequest, 9),
	}
	defer func() {
		select {
		case <-client.resume:
		default:
			close(client.resume)
		}
	}()
	agent := core.NewAgent(client, "model", "system", core.Registry{"repeat": &repetitiveInteractiveTool{}})
	interactive := NewInteractive(InteractiveConfig{Agent: agent})

	interactive.startTurn(context.Background(), "start")
	select {
	case <-client.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("eighth provider request did not start")
	}
	interactive.submitOrQueue("new request", nil, true)
	for call := 1; call <= 8; call++ {
		select {
		case request := <-client.requests:
			if request.call != int32(call) || request.hasQueuedPrompt {
				t.Fatalf("provider request before loop stop = %#v; want call %d without queued prompt", request, call)
			}
			if call == 8 && len(request.userText) == 0 {
				t.Fatal("eighth provider request was not observed; queueing raced with loop teardown")
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("provider request %d was not observed", call)
		}
	}
	close(client.resume)

	select {
	case request := <-client.requests:
		if request.call != 9 || !request.hasQueuedPrompt {
			t.Fatalf("provider request after loop stop = %#v, want call 9 with queued user prompt", request)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued user prompt was not sent after loop stop")
	}
	waitInteractiveIdle(t, interactive)
	if got := client.calls.Load(); got != 9 {
		t.Fatalf("provider calls = %d, want 9 including the user prompt", got)
	}
}

func TestInteractiveRepetitionGuardCanResumeStalledGoal(t *testing.T) {
	client := &repetitiveInteractiveClient{}
	store := &interruptGoalStore{}
	registry := goalToolRegistry()
	registry["repeat"] = &repetitiveInteractiveTool{}
	interactive := NewInteractive(InteractiveConfig{
		Agent:              core.NewAgent(client, "model", "system", registry),
		CurrentGoal:        store.current,
		PersistGoal:        store.persist,
		PersistGoalRuntime: store.persist,
	})
	interactive.runCtx = context.Background()

	interactive.runGoalCommand(context.Background(), "/goal finish the work", []string{"/goal", "finish", "the", "work"})
	waitInteractiveIdle(t, interactive)
	if got := store.current(); got == nil || got.Status != core.GoalStalled {
		t.Fatalf("goal after repetitive loop = %#v, want stalled", got)
	}

	interactive.runGoalCommand(context.Background(), "/goal resume", []string{"/goal", "resume"})
	waitInteractiveIdle(t, interactive)
	if got := client.calls.Load(); got < 9 {
		t.Fatalf("provider requests after resume = %d, want at least 9", got)
	}
	if got := store.current(); got == nil || (got.Status != core.GoalActive && got.Status != core.GoalStalled) {
		t.Fatalf("goal after resume = %#v, want an answered goal run", got)
	}
}

func TestInteractiveRepetitionGuardResolvesScheduledFollowUp(t *testing.T) {
	client := &repetitiveInteractiveClient{
		entered: make(chan struct{}),
		resume:  make(chan struct{}),
	}
	defer func() {
		select {
		case <-client.resume:
		default:
			close(client.resume)
		}
	}()
	agent := core.NewAgent(client, "model", "system", core.Registry{"repeat": &repetitiveInteractiveTool{}})
	interactive := NewInteractive(InteractiveConfig{Agent: agent})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	interactive.startTurn(ctx, "start")
	select {
	case <-client.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("eighth provider request did not start")
	}
	followUp := make(chan error, 1)
	go func() { followUp <- interactive.SubmitFollowUp(ctx, "scheduled task") }()

	// SubmitFollowUp blocks until teardown resolves it. Hold the eighth
	// provider call until the scheduled prompt is actually registered, not
	// merely until its goroutine has been launched.
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for {
		interactive.mu.Lock()
		queued := len(interactive.scheduled) == 1
		interactive.mu.Unlock()
		if queued {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("scheduled follow-up did not enter the queue")
		case <-poll.C:
		}
	}
	close(client.resume)

	select {
	case err := <-followUp:
		if !errors.Is(err, core.ErrRepetitiveLoop) {
			t.Fatalf("scheduled follow-up error = %v, want ErrRepetitiveLoop", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("scheduled follow-up remained blocked after repetitive loop stop")
	}
	waitInteractiveIdle(t, interactive)
	if got := client.calls.Load(); got != 8 {
		t.Fatalf("provider calls = %d, want 8", got)
	}
}

// SubmitFollowUp on an idle interactive without a Run context must start a
// turn instead of panicking in context.WithCancel(nil parent). This pins the
// fallback deterministically; the racy busy-to-idle interleaving behind the
// Windows CI panic needs no timing to cover the nil-parent path itself.
func TestSubmitFollowUpWithoutRunContextStartsTurn(t *testing.T) {
	client := &repetitiveInteractiveClient{requests: make(chan repetitiveInteractiveRequest, 16)}
	agent := core.NewAgent(client, "model", "system", core.Registry{"repeat": &repetitiveInteractiveTool{}})
	interactive := NewInteractive(InteractiveConfig{Agent: agent})
	if interactive.runCtx != nil {
		t.Fatalf("runCtx = %#v, want nil without Run", interactive.runCtx)
	}
	if err := interactive.SubmitFollowUp(context.Background(), "scheduled task"); err != nil {
		t.Fatalf("SubmitFollowUp = %v, want nil", err)
	}
	select {
	case request := <-client.requests:
		if request.call != 1 {
			t.Fatalf("provider request = %#v, want first call", request)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("provider request was not observed")
	}
	waitInteractiveIdle(t, interactive)
}
