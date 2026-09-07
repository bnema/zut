package modes

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// streamContinuationClient emits interim text with StopContinue on the
// first call and the final answer with StopEnd on the second call.
type streamContinuationClient struct {
	calls int32
}

func (c *streamContinuationClient) Name() string { return "stream-continuation" }

func (c *streamContinuationClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	call := atomic.AddInt32(&c.calls, 1)
	out := make(chan provider.Event, 8)
	go func() {
		defer close(out)
		text, stop := "final", provider.StopEnd
		if call == 1 {
			text, stop = "part one", provider.StopContinue
		}
		for _, chunk := range strings.Split(text, " ") {
			select {
			case <-ctx.Done():
				return
			case out <- provider.EventTextDelta{Delta: chunk + " "}:
			}
		}
		select {
		case <-ctx.Done():
			return
		case out <- provider.EventDone{Stop: stop, Message: provider.Message{
			Role:    provider.RoleAssistant,
			Content: []provider.Content{provider.TextBlock{Text: text}},
		}}:
		}
	}()
	return out, nil
}

func TestRunStreamContinuation(t *testing.T) {
	client := &streamContinuationClient{}
	ag := core.NewAgent(client, "test-model", "", nil)
	var out, diag bytes.Buffer
	if err := RunStreamWithDiag(context.Background(), ag, "hi", nil, &out, &diag); err != nil {
		t.Fatal(err)
	}
	// Two inferences happened: execution did not return after the
	// interim continuation.
	if got := atomic.LoadInt32(&client.calls); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}
	got := out.String()
	if !strings.Contains(got, "final") {
		t.Fatalf("stdout = %q, want it to include the final answer", got)
	}
	if !strings.Contains(got, "part one") {
		t.Fatalf("stdout = %q, want it to include the interim text", got)
	}
}
