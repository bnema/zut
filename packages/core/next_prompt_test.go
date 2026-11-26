package core

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
)

// predictTestClient records every request and replies with scripted events,
// one script per call (the last script repeats).
type predictTestClient struct {
	mu       sync.Mutex
	requests []provider.Request
	scripts  [][]provider.Event
	err      error
}

func (c *predictTestClient) Name() string { return "predict-test" }

func (c *predictTestClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, req)
	if c.err != nil {
		return nil, c.err
	}
	script := c.scripts[min(len(c.requests)-1, len(c.scripts)-1)]
	out := make(chan provider.Event, len(script))
	for _, event := range script {
		out <- event
	}
	close(out)
	return out, nil
}

func assistantDone(text string) []provider.Event {
	return []provider.Event{
		provider.EventTextDelta{Delta: text},
		provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: text}}}},
	}
}

func newPredictAgent(t *testing.T, client provider.Client) *Agent {
	t.Helper()
	agent := NewAgent(client, "test-model", "system prompt", NewRegistry())
	agent.MaxTokens = 1234
	agent.Reasoning = "high"
	temp := float32(0.3)
	agent.Temperature = &temp
	agent.SetMessages([]provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "fix the bug"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "done, tests pass"}}},
	})
	return agent
}

func TestPredictNextPromptSharesTurnRequestPrefix(t *testing.T) {
	client := &predictTestClient{scripts: [][]provider.Event{assistantDone("ok"), assistantDone("commit it")}}
	agent := newPredictAgent(t, client)
	if err := agent.Prompt(context.Background(), "next", nil, func(AgentEvent) {}); err != nil {
		t.Fatal(err)
	}
	before := agent.Messages()

	got, err := agent.PredictNextPrompt(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "commit it" {
		t.Fatalf("prediction = %q", got)
	}
	if len(client.requests) != 2 {
		t.Fatalf("requests = %d", len(client.requests))
	}
	turn, side := client.requests[0], client.requests[1]
	if len(side.Messages) != len(before)+1 {
		t.Fatalf("side messages = %d, transcript = %d", len(side.Messages), len(before))
	}
	last := side.Messages[len(side.Messages)-1]
	if last.Role != provider.RoleUser || len(last.Content) != 1 {
		t.Fatalf("trailing message = %#v", last)
	}
	if block, ok := last.Content[0].(provider.TextBlock); !ok || !strings.Contains(block.Text, NextPromptNoPrediction) {
		t.Fatalf("trailing content = %#v", last.Content[0])
	}
	strip := func(r provider.Request) provider.Request {
		r.Messages, r.Lifecycle = nil, nil
		r.Context = provider.RequestContext{}
		return r
	}
	if !reflect.DeepEqual(strip(turn), strip(side)) {
		t.Fatalf("prefix fields differ:\nturn=%#v\nside=%#v", strip(turn), strip(side))
	}
	if side.Context.ThreadID != "" {
		t.Fatalf("side ThreadID = %q, want empty", side.Context.ThreadID)
	}
	if side.Context.CacheSessionID == "" || side.Context.CacheSessionID != turn.Context.CacheSessionID {
		t.Fatalf("side CacheSessionID = %q, turn = %q", side.Context.CacheSessionID, turn.Context.CacheSessionID)
	}
	if side.Context.TurnID == "" || side.Context.TurnID == turn.Context.TurnID {
		t.Fatalf("side TurnID = %q, turn TurnID = %q", side.Context.TurnID, turn.Context.TurnID)
	}
	if side.Lifecycle != nil {
		t.Fatal("side request must not carry a lifecycle sink")
	}
	if !reflect.DeepEqual(agent.Messages(), before) {
		t.Fatal("transcript changed after prediction")
	}
	// The real next turn must send the prediction's prefix unchanged.
	if err := agent.Prompt(context.Background(), "commit it", nil, func(AgentEvent) {}); err != nil {
		t.Fatal(err)
	}
	if len(client.requests) != 3 {
		t.Fatalf("requests = %d", len(client.requests))
	}
	next := client.requests[2]
	prefix := side.Messages[:len(side.Messages)-1]
	if len(next.Messages) <= len(prefix) || !reflect.DeepEqual(next.Messages[:len(prefix)], prefix) {
		t.Fatal("next turn does not extend the prediction's message prefix")
	}
	if !reflect.DeepEqual(strip(next), strip(side)) {
		t.Fatalf("next turn fields differ:\nnext=%#v\nside=%#v", strip(next), strip(side))
	}
}

func TestPredictNextPromptNormalizesReplies(t *testing.T) {
	cases := map[string]string{
		"NO_PREDICTION":              "",
		"  NO_PREDICTION\n":          "",
		"\"run the tests\"":          "run the tests",
		"`ship it`":                  "ship it",
		"looks good\n\n  now   push": "looks good now push",
		"\x1b[31mred\x1b[0m text\a":  "red text",
		strings.Repeat("a", 301):     "",
		"   ":                        "",
		"no_prediction.":             "",
		"NO_PREDICTION - unsure":     "",
	}
	for in, want := range cases {
		client := &predictTestClient{scripts: [][]provider.Event{assistantDone(in)}}
		got, err := newPredictAgent(t, client).PredictNextPrompt(context.Background())
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Fatalf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestPredictNextPromptFallsBackToFinalMessage(t *testing.T) {
	client := &predictTestClient{scripts: [][]provider.Event{{
		provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "yes"}}}},
	}}}
	got, err := newPredictAgent(t, client).PredictNextPrompt(context.Background())
	if err != nil || got != "yes" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestPredictNextPromptToolCallOnlyReplyIsEmpty(t *testing.T) {
	client := &predictTestClient{scripts: [][]provider.Event{{
		provider.EventDone{Stop: provider.StopToolUse, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{
			provider.ToolCallBlock{ID: "c1", Name: "bash", Arguments: []byte(`{}`)},
		}}},
	}}}
	agent := newPredictAgent(t, client)
	before := agent.Messages()
	got, err := agent.PredictNextPrompt(context.Background())
	if err != nil || got != "" {
		t.Fatalf("got %q, %v", got, err)
	}
	if !reflect.DeepEqual(agent.Messages(), before) {
		t.Fatal("transcript changed")
	}
}

func TestPredictNextPromptPropagatesErrors(t *testing.T) {
	boom := errors.New("boom")
	client := &predictTestClient{scripts: [][]provider.Event{{provider.EventDone{Stop: provider.StopError, Err: boom}}}}
	if _, err := newPredictAgent(t, client).PredictNextPrompt(context.Background()); !errors.Is(err, boom) || !strings.Contains(err.Error(), "next prompt prediction") {
		t.Fatalf("stream err = %v", err)
	}
	client = &predictTestClient{err: boom}
	if _, err := newPredictAgent(t, client).PredictNextPrompt(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("request err = %v", err)
	}
}

func TestPredictNextPromptEmptyTranscriptAndNilClient(t *testing.T) {
	client := &predictTestClient{scripts: [][]provider.Event{assistantDone("x")}}
	agent := NewAgent(client, "m", "s", NewRegistry())
	if got, err := agent.PredictNextPrompt(context.Background()); err != nil || got != "" {
		t.Fatalf("got %q, %v", got, err)
	}
	if len(client.requests) != 0 {
		t.Fatal("empty transcript must not send a request")
	}
	agent.Client = nil
	if _, err := agent.PredictNextPrompt(context.Background()); err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestPredictNextPromptHonorsCancellation(t *testing.T) {
	client := &blockingTitleClient{started: make(chan struct{})}
	agent := newPredictAgent(t, client)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := agent.PredictNextPrompt(ctx)
		errc <- err
	}()
	<-client.started
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prediction did not stop after cancel")
	}
}

type noSidePredictionClient struct{ predictTestClient }

func (*noSidePredictionClient) SupportsSidePrediction() bool { return false }

func TestPredictNextPromptSkipsUnsupportedProviders(t *testing.T) {
	client := &noSidePredictionClient{predictTestClient{scripts: [][]provider.Event{assistantDone("x")}}}
	got, err := newPredictAgent(t, client).PredictNextPrompt(context.Background())
	if err != nil || got != "" {
		t.Fatalf("got %q, %v", got, err)
	}
	if len(client.requests) != 0 {
		t.Fatal("unsupported provider must not receive a request")
	}
}

// endlessTextClient streams text deltas until its context is cancelled.
type endlessTextClient struct{ cancelled chan struct{} }

func (c *endlessTextClient) Name() string { return "endless" }

func (c *endlessTextClient) Stream(ctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
	out := make(chan provider.Event)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				close(c.cancelled)
				return
			case out <- provider.EventTextDelta{Delta: "word "}:
			}
		}
	}()
	return out, nil
}

func TestPredictNextPromptStopsAtByteCap(t *testing.T) {
	client := &endlessTextClient{cancelled: make(chan struct{})}
	got, err := newPredictAgent(t, client).PredictNextPrompt(context.Background())
	if err != nil || got != "" {
		t.Fatalf("got %q, %v", got, err)
	}
	select {
	case <-client.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("stream context was not cancelled after the cap")
	}
}
