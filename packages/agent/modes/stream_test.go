package modes

import (
	"bytes"
	"context"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

type streamTestClient struct {
	events []provider.Event
	err    error
}

func (c *streamTestClient) Name() string { return "stream-test" }

func (c *streamTestClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	if c.err != nil {
		return nil, c.err
	}
	out := make(chan provider.Event, len(c.events))
	go func() {
		defer close(out)
		for _, ev := range c.events {
			select {
			case <-ctx.Done():
				return
			case out <- ev:
			}
		}
	}()
	return out, nil
}

func TestRunStreamWritesDeltasLive(t *testing.T) {
	client := &streamTestClient{events: []provider.Event{
		provider.EventTextDelta{Delta: "Hel"},
		provider.EventTextDelta{Delta: "lo"},
		provider.EventDone{
			Stop: provider.StopEnd,
			Message: provider.Message{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.TextBlock{Text: "Hello"}},
			},
		},
	}}
	ag := core.NewAgent(client, "test-model", "", nil)
	var out, diag bytes.Buffer
	if err := RunStreamWithDiag(context.Background(), ag, "hi", nil, &out, &diag); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "Hello\n" {
		t.Fatalf("stdout = %q, want Hello\\n", got)
	}
}

func TestRunStreamFallbackWithoutDeltas(t *testing.T) {
	client := &streamTestClient{events: []provider.Event{
		provider.EventDone{
			Stop: provider.StopEnd,
			Message: provider.Message{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.TextBlock{Text: "done"}},
			},
		},
	}}
	ag := core.NewAgent(client, "test-model", "", nil)
	var out bytes.Buffer
	if err := RunStreamWithDiag(context.Background(), ag, "hi", nil, &out, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "done\n" {
		t.Fatalf("stdout = %q, want done\\n", got)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
