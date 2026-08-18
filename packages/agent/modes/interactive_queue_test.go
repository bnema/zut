package modes

import (
	"context"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

func TestQueuedMessageSummaryKeepsImageIndicatorWhenTextIsTruncated(t *testing.T) {
	message := core.QueuedMessage{
		Text:   strings.Repeat("x", 100),
		Images: []provider.ImageBlock{{MimeType: "image/png", Data: []byte("png-1")}},
	}

	summary := queuedMessageSummary(message, 24)

	if !strings.HasSuffix(summary, " [image]") {
		t.Fatalf("summary = %q, want visible image indicator", summary)
	}
	if len([]rune(summary)) > 24 {
		t.Fatalf("summary width = %d, want at most 24", len([]rune(summary)))
	}
}

func TestBusySubmitQueuesClipboardImagePrompt(t *testing.T) {
	agent := core.NewAgent(nil, "test-model", "", nil)
	i := NewInteractive(InteractiveConfig{Agent: agent})
	i.mu.Lock()
	i.busy = true
	i.mu.Unlock()
	i.ed.SetValue("inspect [clipboard image #1]")
	i.clipboardImages = []clipboardImageAttachment{testClipboardImage("[clipboard image #1]", "png-1")}

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})

	queued := agent.PendingQueuedMessages()
	if len(queued) != 1 || queued[0].Text != "inspect" {
		t.Fatalf("queued messages = %#v, want one inspect prompt", queued)
	}
	if len(queued[0].Images) != 1 || string(queued[0].Images[0].Data) != "png-1" {
		t.Fatalf("queued images = %#v, want png-1", queued[0].Images)
	}
	if !i.ed.IsEmpty() || len(i.clipboardImages) != 0 {
		t.Fatal("submitted image prompt remained in the editor")
	}
}

func TestSlideBackRestoresQueuedImagesToEditor(t *testing.T) {
	agent := core.NewAgent(nil, "test-model", "", nil)
	image := testClipboardImage("unused", "png-1").Image
	agent.QueueMessage("inspect", []provider.ImageBlock{image})
	i := NewInteractive(InteractiveConfig{Agent: agent})
	i.mu.Lock()
	i.busy = true
	i.mu.Unlock()

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyUp, Alt: true})

	if got := i.ed.Value(); got != "inspect [clipboard image #1]" {
		t.Fatalf("editor = %q, want restored image marker", got)
	}
	if len(i.clipboardImages) != 1 || string(i.clipboardImages[0].Image.Data) != "png-1" {
		t.Fatalf("clipboard images = %#v, want png-1", i.clipboardImages)
	}
}

func TestEscapeRestoresMostRecentQueuedMessageToEditor(t *testing.T) {
	agent := core.NewAgent(nil, "test-model", "", nil)
	agent.QueueMessage("older follow-up", nil)
	agent.QueueMessage("recover this draft", nil)

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
	if got := agent.PendingQueuedMessages(); len(got) != 1 || got[0].Text != "older follow-up" {
		t.Errorf("remaining queued messages = %v, want [older follow-up]", got)
	}
}

func TestEscapeRestoresHostQueuedMessageToEditor(t *testing.T) {
	i := NewInteractive(InteractiveConfig{})
	cancelled := 0
	i.mu.Lock()
	i.busy = true
	i.cancelTurn = func() { cancelled++ }
	i.queued = []core.QueuedMessage{{Text: "recover this draft"}}
	i.mu.Unlock()

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc})

	if cancelled != 1 {
		t.Fatalf("cancelled = %d, want 1", cancelled)
	}
	if got, want := i.ed.Value(), "recover this draft"; got != want {
		t.Errorf("editor = %q, want %q", got, want)
	}
}
