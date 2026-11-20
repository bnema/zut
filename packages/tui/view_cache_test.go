package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func TestViewCacheInvalidatesToolOutputExpansionAndWidth(t *testing.T) {
	messages := []provider.Message{
		{
			Role: provider.RoleAssistant,
			Content: []provider.Content{provider.ToolCallBlock{
				ID: "call-1", Name: "bash", Arguments: []byte(`{"command":"cat file"}`),
			}},
		},
		{
			Role: provider.RoleTool,
			Content: []provider.Content{provider.ToolResultBlock{
				CallID:  "call-1",
				Content: []provider.Content{provider.TextBlock{Text: cacheTestResult("old")}},
			}},
		},
	}
	view := &View{Theme: Dark, Messages: messages, MessagesRevision: 1}
	collapsed, _ := view.BuildWithAnchors(40)
	if len(collapsed) == 0 || !strings.Contains(strings.Join(collapsed, "\n"), "more lines") {
		t.Fatal("long tool result did not use the collapsed presentation")
	}

	view.ExpandAll = true
	expanded, _ := view.BuildWithAnchors(40)
	if len(expanded) <= len(collapsed) {
		t.Fatalf("expansion did not invalidate rows: collapsed=%d expanded=%d", len(collapsed), len(expanded))
	}

	wide, _ := view.BuildWithAnchors(120)
	// The wide render may have the same row count for this fixture, but it
	// must still be rebuilt for the new wrapping width.
	if strings.Join(wide, "\n") == strings.Join(expanded, "\n") {
		t.Fatal("resize reused rows from the old width")
	}

	view.ExpandAll = false
	view.Messages[1].Content = []provider.Content{provider.ToolResultBlock{
		CallID:  "call-1",
		Content: []provider.Content{provider.TextBlock{Text: cacheTestResult("new marker")}},
	}}
	view.Messages = append(view.Messages, provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "third message"}},
	})
	view.MessagesRevision = 2
	updated, _ := view.BuildWithAnchors(40)
	if !strings.Contains(strings.Join(updated, "\n"), "new marker") {
		t.Fatal("tool output revision reused stale cached rows")
	}
}

func TestViewRevisionCacheIsBoundToTailLimit(t *testing.T) {
	messages := make([]provider.Message, 20)
	for i := range messages {
		messages[i] = provider.Message{
			Role:    provider.RoleUser,
			Content: []provider.Content{provider.TextBlock{Text: fmt.Sprintf("message %d", i)}},
		}
	}
	view := &View{Theme: Dark, Messages: messages, MessagesRevision: 1, TailLimit: 5}
	view.Build(80)
	if view.messageCacheStart != len(messages)-view.TailLimit {
		t.Fatalf("message cache start=%d, want %d", view.messageCacheStart, len(messages)-view.TailLimit)
	}
	if len(view.messageCache) > view.TailLimit {
		t.Fatalf("message cache length=%d exceeds tail limit %d", len(view.messageCache), view.TailLimit)
	}

	view.Build(80)
	if len(view.messageCache) > view.TailLimit {
		t.Fatalf("message cache grew on a cache hit: length=%d", len(view.messageCache))
	}
}

func TestRenderLiveToolResultCollapseUsesRenderedLineCount(t *testing.T) {
	var text strings.Builder
	for i := 0; i < 13; i++ {
		text.WriteString(strings.Repeat("x", 17))
		text.WriteByte('\n')
	}
	view := &View{Theme: Dark}
	full := view.renderToolText(text.String(), 20, Dark.ToolOut, "", 1)
	collapsed := view.renderLiveToolResult(text.String(), 20, Dark.ToolOut, "", modeDefault)
	joined := strings.Join(collapsed, "\n")
	wantTotal := fmt.Sprintf("%d total", len(full))
	if !strings.Contains(joined, wantTotal) {
		t.Fatalf("collapse footer omitted rendered total %q: %s", wantTotal, joined)
	}
}

func cacheTestResult(prefix string) string {
	var b strings.Builder
	for n := 0; n < 32; n++ {
		b.WriteString(prefix)
		// Long enough to wrap at 40 columns so a resize changes rows.
		b.WriteString(" line with enough words to wrap at forty columns ")
		b.WriteString(string(rune('a' + n%26)))
		b.WriteByte('\n')
	}
	return b.String()
}
