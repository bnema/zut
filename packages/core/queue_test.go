package core

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
)

type queueFakeClient struct {
	calls int32
}

func (c *queueFakeClient) Name() string { return "queue-fake" }

func (c *queueFakeClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	call := atomic.AddInt32(&c.calls, 1)
	out := make(chan provider.Event, 4)
	go func() {
		defer close(out)
		out <- provider.EventStart{Provider: "queue-fake", Model: req.Model}
		switch call {
		case 1:
			out <- provider.EventToolStart{ID: "t1", Name: "echo"}
			out <- provider.EventToolEnd{ID: "t1"}
			out <- provider.EventDone{Stop: provider.StopToolUse, Message: provider.Message{
				Role: provider.RoleAssistant,
				Content: []provider.Content{
					provider.TextBlock{Text: "using tool"},
					provider.ToolCallBlock{ID: "t1", Name: "echo", Arguments: json.RawMessage(`{}`)},
				},
			}}
		case 2:
			out <- provider.EventTextDelta{Delta: "saw queued"}
			out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.TextBlock{Text: "saw queued"}},
			}}
		default:
			out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.TextBlock{Text: "extra"}},
			}}
		}
	}()
	return out, nil
}

// blockingTool waits until the test has queued a message, then
// returns. This pins the core behaviour: queued user text is delivered
// after the current tool batch finishes and before the next model call.
type blockingTool struct {
	started chan struct{}
	release chan struct{}
}

func (t *blockingTool) Name() string            { return "echo" }
func (t *blockingTool) Description() string     { return "echoes" }
func (t *blockingTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (t *blockingTool) Execute(ctx context.Context, args json.RawMessage, progress func(string)) (ToolResult, error) {
	close(t.started)
	select {
	case <-ctx.Done():
		return ToolResult{Content: []provider.Content{provider.TextBlock{Text: ctx.Err().Error()}}, IsError: true}, ctx.Err()
	case <-t.release:
	}
	return ToolResult{Content: []provider.Content{provider.TextBlock{Text: "tool ok"}}}, nil
}

func TestQueuedMessageInjectedAfterToolBatchBeforeNextModelCall(t *testing.T) {
	client := &queueFakeClient{}
	tool := &blockingTool{started: make(chan struct{}), release: make(chan struct{})}
	a := NewAgent(client, "fake-model", "system", Registry{"echo": tool})

	var (
		mu    sync.Mutex
		texts []string
	)
	sink := func(ev AgentEvent) {
		switch e := ev.(type) {
		case EvUserMessage:
			mu.Lock()
			texts = append(texts, "user:"+extractText(e.Message))
			mu.Unlock()
		case EvAssistantMessage:
			mu.Lock()
			texts = append(texts, "asst:"+extractText(e.Message))
			mu.Unlock()
		}
	}

	done := make(chan error, 1)
	go func() {
		done <- a.Prompt(context.Background(), "do X", nil, sink)
	}()

	<-tool.started
	if !a.QueueMessage("also do Y", nil) {
		t.Fatal("QueueMessage returned false")
	}
	close(tool.release)

	if err := <-done; err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if got := atomic.LoadInt32(&client.calls); got != 2 {
		t.Fatalf("Stream calls = %d; want 2", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if !queueTestContains(texts, "user:also do Y") {
		t.Fatalf("queued message was not emitted as user message; texts=%v", texts)
	}
	if !queueTestContains(texts, "asst:saw queued") {
		t.Fatalf("second assistant response missing; texts=%v", texts)
	}
}

func queueTestContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestQueuedImageMessageInjectedAtNextSafeBoundary(t *testing.T) {
	client := &queueFakeClient{}
	tool := &blockingTool{started: make(chan struct{}), release: make(chan struct{})}
	a := NewAgent(client, "fake-model", "system", Registry{"echo": tool})

	var queued provider.Message
	done := make(chan error, 1)
	go func() {
		done <- a.Prompt(context.Background(), "inspect later", nil, func(ev AgentEvent) {
			if user, ok := ev.(EvUserMessage); ok && len(user.Message.Content) > 0 {
				if _, isImage := user.Message.Content[0].(provider.ImageBlock); isImage {
					queued = user.Message
				}
			}
		})
	}()

	<-tool.started
	image := provider.ImageBlock{MimeType: "image/png", Data: []byte("png-bytes")}
	if !a.QueueMessage("", []provider.ImageBlock{image}) {
		t.Fatal("QueueMessage returned false for image-only prompt")
	}
	close(tool.release)

	if err := <-done; err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if len(queued.Content) != 1 {
		t.Fatalf("queued content = %#v, want one image", queued.Content)
	}
	got, ok := queued.Content[0].(provider.ImageBlock)
	if !ok || got.MimeType != image.MimeType || string(got.Data) != string(image.Data) {
		t.Fatalf("queued image = %#v, want %#v", queued.Content[0], image)
	}
}

func TestQueuedMessageKeepsAcceptedTimeAcrossDrain(t *testing.T) {
	a := NewAgent(nil, "fake", "", Registry{})
	before := time.Now()
	if !a.QueueMessage("queued", nil) {
		t.Fatal("QueueMessage returned false")
	}
	after := time.Now()

	queued := a.drainQueuedMessages()
	if len(queued) != 1 {
		t.Fatalf("drained %d messages, want 1", len(queued))
	}
	if queued[0].accepted.Before(before) || queued[0].accepted.After(after) {
		t.Fatalf("accepted time %s outside queue submission window %s..%s", queued[0].accepted, before, after)
	}
}

func TestQueueMessageSnapshotPopAndDrain(t *testing.T) {
	a := NewAgent(nil, "fake", "", Registry{})
	if a.QueueMessage("   ", nil) {
		t.Fatal("blank queue message accepted")
	}
	a.QueueMessage("one", nil)
	a.QueueMessage("two", nil)
	if got := a.PendingQueuedMessages(); len(got) != 2 || got[0].Text != "one" || got[1].Text != "two" {
		t.Fatalf("PendingQueuedMessages = %v; want [one two]", got)
	}
	if message, ok := a.PopQueuedMessage(); !ok || message.Text != "two" {
		t.Fatalf("PopQueuedMessage = %#v,%v; want two,true", message, ok)
	}
	if got := a.DrainQueuedMessages(); len(got) != 1 || got[0].Text != "one" {
		t.Fatalf("DrainQueuedMessages = %v; want [one]", got)
	}
	if got := a.QueuedMessageCount(); got != 0 {
		t.Fatalf("QueuedMessageCount = %d; want 0", got)
	}
}
