package modes

import (
	"context"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/tui"
)

func TestEscapeRestoresMostRecentQueuedMessageToEditor(t *testing.T) {
	agent := core.NewAgent(nil, "test-model", "", nil)
	agent.QueueMessage("older follow-up")
	agent.QueueMessage("recover this draft")

	i := NewInteractive(InteractiveConfig{Agent: agent})
	i.ed.SetValue("existing draft")
	cancelled := 0
	i.mu.Lock()
	i.busy = true
	i.cancelTurn = func() { cancelled++ }
	i.mu.Unlock()

	if done := i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}); done {
		t.Fatal("Escape exited")
	}
	if cancelled != 1 {
		t.Fatalf("cancelled = %d, want 1", cancelled)
	}
	if got, want := i.ed.Value(), "recover this draft"; got != want {
		t.Errorf("editor = %q, want %q", got, want)
	}
	if got := agent.PendingQueuedMessages(); len(got) != 1 || got[0] != "older follow-up" {
		t.Errorf("remaining queued messages = %v, want [older follow-up]", got)
	}
}

func TestEscapeRestoresHostQueuedMessageToEditor(t *testing.T) {
	i := NewInteractive(InteractiveConfig{})
	cancelled := 0
	i.mu.Lock()
	i.busy = true
	i.cancelTurn = func() { cancelled++ }
	i.queued = []string{"recover this draft"}
	i.mu.Unlock()

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc})

	if cancelled != 1 {
		t.Fatalf("cancelled = %d, want 1", cancelled)
	}
	if got, want := i.ed.Value(), "recover this draft"; got != want {
		t.Errorf("editor = %q, want %q", got, want)
	}
}
