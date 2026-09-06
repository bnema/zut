package modes

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

type completionErrorClient struct {
	calls    atomic.Int32
	started  chan struct{}
	release  chan struct{}
	requests chan provider.Request
}

func (*completionErrorClient) Name() string { return "completion-error-test" }
func (c *completionErrorClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	if c.calls.Add(1) == 1 {
		close(c.started)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.release:
			return nil, errors.New("synthetic request failure")
		}
	}
	c.requests <- req
	out := make(chan provider.Event, 1)
	out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "done"}}}}
	close(out)
	return out, nil
}

func TestCompletionSurvivesParentError(t *testing.T) {
	for _, name := range []string{"provider", "compaction", "idle-pre-turn-compaction"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := &completionErrorClient{started: make(chan struct{}), release: make(chan struct{}), requests: make(chan provider.Request, 4)}
			ag := core.NewAgent(client, "test-model", "", nil)
			ag.SetMessages([]provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "existing task"}}}})
			threshold := 70
			i := NewInteractive(InteractiveConfig{Agent: ag, Provider: "anthropic", Model: "claude-sonnet-4-5", AutoCompactThreshold: &threshold})
			i.runCtx = ctx
			report := func() {
				i.TrackResidentSubagent("worker", "turn")
				i.ReportResidentSubagent(subagents.ResidentCompletion{ChildID: "worker", TurnID: "turn", Summary: "retained evidence"})
			}
			switch name {
			case "idle-pre-turn-compaction":
				i.lastCtxInput = 150000
				report()
			case "compaction":
				i.runCompact(ctx, compactContinuationRequest{origin: compactOriginManual})
			default:
				i.startTurn(ctx, "work")
			}
			select {
			case <-client.started:
			case <-time.After(2 * time.Second):
				t.Fatal("request did not start")
			}
			if name != "idle-pre-turn-compaction" {
				report()
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				i.mu.Lock()
				count := len(i.queued) + ag.QueuedMessageCount()
				i.mu.Unlock()
				if count != 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("completion did not enter queue")
				}
				time.Sleep(time.Millisecond)
			}
			i.submitOrQueue("stale user input", nil, true)
			close(client.release)
			waitInteractiveIdle(t, i)
			pending := ag.PendingQueuedMessages()
			if len(pending) != 1 || !pending[0].HostEvent || !strings.Contains(pending[0].Text, "retained evidence") {
				t.Fatalf("pending after error = %#v", pending)
			}
			if client.calls.Load() != 1 {
				t.Fatal("error automatically retried")
			}
			i.mu.Lock()
			i.lastCtxInput = 0 // the explicit retry no longer needs compaction
			i.mu.Unlock()
			i.startTurn(ctx, "retry now")
			select {
			case req := <-client.requests:
				if got := requestUserTextCount(req, pending[0].Text); got != 1 {
					t.Fatalf("completion copies = %d, want 1", got)
				}
				if requestUserTextCount(req, "stale user input") != 0 {
					t.Fatal("stale user input survived error")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("explicit retry did not start")
			}
			waitInteractiveIdle(t, i)
			if ag.QueuedMessageCount() != 0 {
				t.Fatal("completion remained queued after delivery")
			}
		})
	}
}

func TestExplicitCancellationDiscardsQueuedHostEvents(t *testing.T) {
	ag := core.NewAgent(nil, "test-model", "", nil)
	ag.QueuePrompt(core.QueuedMessage{Text: "agent event", HostEvent: true})
	i := &Interactive{agent: ag, queued: []core.QueuedMessage{{Text: "host event", HostEvent: true}}}
	i.mu.Lock()
	i.discardQueuedMessagesLocked(false)
	i.mu.Unlock()
	if ag.QueuedMessageCount() != 0 || len(i.queued) != 0 {
		t.Fatal("explicit cancellation retained queued events")
	}
}
