package core

import (
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
)

func TestSessionPersistsAndReplaysToolTiming(t *testing.T) {
	session, err := NewSession(t.TempDir(), "/workspace", "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 8, 8, 12, 0, 0, 0, time.FixedZone("UTC-4", -4*60*60))
	timing := &provider.ToolTiming{
		StartedAt:   started,
		CompletedAt: started.Add(250 * time.Millisecond),
		Duration:    250 * time.Millisecond,
	}
	if err := session.AppendMessage(provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.ToolCallBlock{ID: "call-1", Name: "echo"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(provider.Message{
		Role: provider.RoleTool,
		Content: []provider.Content{provider.ToolResultBlock{
			CallID:  "call-1",
			Content: []provider.Content{provider.TextBlock{Text: "ok"}},
			Timing:  timing,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	path := session.Path
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	snapshot, err := ReadSessionSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	result, ok := snapshot.Messages[1].Content[0].(provider.ToolResultBlock)
	if !ok {
		t.Fatalf("replayed content = %T, want ToolResultBlock", snapshot.Messages[1].Content[0])
	}
	if result.Timing == nil {
		t.Fatal("replayed tool result lost timing")
	}
	if !result.Timing.StartedAt.Equal(timing.StartedAt) || !result.Timing.CompletedAt.Equal(timing.CompletedAt) || result.Timing.Duration != timing.Duration {
		t.Fatalf("replayed timing = %#v, want %#v", result.Timing, timing)
	}
}
