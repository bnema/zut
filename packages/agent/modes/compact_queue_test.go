package modes

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
)

type compactQueueClient struct {
	compactionStarted chan struct{}
	releaseCompaction chan struct{}
	followUpRequest   chan provider.Request
	onFollowUp        func()

	mu    sync.Mutex
	calls int
}

func (c *compactQueueClient) Name() string { return "compact-queue-test" }

func (c *compactQueueClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.mu.Unlock()

	out := make(chan provider.Event, 2)
	go func() {
		defer close(out)
		if call == 1 {
			close(c.compactionStarted)
			select {
			case <-ctx.Done():
				out <- provider.EventDone{Stop: provider.StopAborted, Err: ctx.Err()}
			case <-c.releaseCompaction:
				out <- provider.EventTextDelta{Delta: "summary"}
				out <- provider.EventDone{Stop: provider.StopEnd}
			}
			return
		}

		if c.onFollowUp != nil {
			c.onFollowUp()
		}
		c.followUpRequest <- req
		out <- provider.EventDone{
			Stop: provider.StopEnd,
			Message: provider.Message{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.TextBlock{Text: "done"}},
			},
		}
	}()
	return out, nil
}

func receiveRequest(t *testing.T, requests <-chan provider.Request) provider.Request {
	t.Helper()
	select {
	case req := <-requests:
		return req
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for provider request")
		return provider.Request{}
	}
}

func requestUserTextCount(req provider.Request, want string) int {
	count := 0
	for _, message := range req.Messages {
		if message.Role != provider.RoleUser {
			continue
		}
		for _, content := range message.Content {
			if text, ok := content.(provider.TextBlock); ok && text.Text == want {
				count++
			}
		}
	}
	return count
}

func TestEstimateCompactedContextInput(t *testing.T) {
	if got := estimateCompactedContextInput(nil); got != 0 {
		t.Fatalf("empty transcript estimate = %d, want 0", got)
	}
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "1234"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{
			provider.TextBlock{Text: "5678"},
			provider.ToolCallBlock{ID: "call-1", Name: "read", Arguments: json.RawMessage(`{}`)},
		}},
		{Role: provider.RoleTool, Content: []provider.Content{
			provider.ToolResultBlock{CallID: "call-1", Content: []provider.Content{
				provider.TextBlock{Text: "01234567"},
				provider.ImageBlock{MimeType: "image/png", Data: []byte("ignored-bytes")},
			}},
		}},
	}
	// 4 + 4 + len("read")+2 + 8 = 22 chars → 5 tokens; image bytes excluded.
	if got, want := estimateCompactedContextInput(msgs), 5; got != want {
		t.Fatalf("estimate = %d, want %d", got, want)
	}
}
