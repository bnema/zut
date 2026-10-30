package modes

import (
	"encoding/json"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

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
