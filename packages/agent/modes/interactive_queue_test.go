package modes

import (
	"context"
	"io"
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

func TestSlideBackSkipsHostEvents(t *testing.T) {
	for _, hostQueue := range []bool{false, true} {
		name := "agent queue"
		if hostQueue {
			name = "host queue"
		}
		t.Run(name, func(t *testing.T) {
			agent := core.NewAgent(nil, "test-model", "", nil)
			i := NewInteractive(InteractiveConfig{Agent: agent})
			i.busy = true
			messages := []core.QueuedMessage{
				{Text: "first report", HostEvent: true},
				{Text: "older draft"},
				{Text: "recover this draft"},
				{Text: "latest report", HostEvent: true},
			}
			if hostQueue {
				i.queued = messages
			} else {
				for _, message := range messages {
					agent.QueuePrompt(message)
				}
			}
			for _, want := range []string{"recover this draft", "older draft"} {
				i.handleKey(context.Background(), tui.Key{Kind: tui.KeyUp, Alt: true})
				if got := i.ed.Value(); got != want {
					t.Fatalf("editor = %q, want %q", got, want)
				}
			}
			i.handleKey(context.Background(), tui.Key{Kind: tui.KeyUp, Alt: true})
			if got := i.ed.Value(); got != "older draft" {
				t.Fatalf("host-only queue changed editor to %q", got)
			}
			pending := i.queued
			if !hostQueue {
				pending = agent.PendingQueuedMessages()
			}
			if len(pending) != 2 || pending[0].Text != "first report" || pending[1].Text != "latest report" || !pending[0].HostEvent || !pending[1].HostEvent {
				t.Fatalf("remaining reports = %#v, want both reports in original order", pending)
			}
		})
	}
}

func TestSlideBackParsesModifiedArrow(t *testing.T) {
	for name, sequence := range map[string]string{
		"alt up":       "\x1b[1;3A",
		"alt shift up": "\x1b[1;4A",
	} {
		t.Run(name, func(t *testing.T) {
			agent := core.NewAgent(nil, "test-model", "", nil)
			i := NewInteractive(InteractiveConfig{Agent: agent})
			i.busy = true
			i.ed.SetValue("recover this draft")
			i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
			agent.QueuePrompt(core.QueuedMessage{Text: "worker report", HostEvent: true})
			reader := tui.NewReader(func() (byte, error) {
				if sequence == "" {
					return 0, io.EOF
				}
				b := sequence[0]
				sequence = sequence[1:]
				return b, nil
			})
			key, err := reader.Read()
			if err != nil {
				t.Fatal(err)
			}
			i.handleKey(context.Background(), key)
			if got := i.ed.Value(); got != "recover this draft" {
				t.Fatalf("editor = %q, want user draft", got)
			}
			pending := agent.PendingQueuedMessages()
			if len(pending) != 1 || !pending[0].HostEvent {
				t.Fatalf("pending = %#v, want worker report", pending)
			}
		})
	}
}

func TestEscapeSkipsHostEvents(t *testing.T) {
	for _, hasUserMessage := range []bool{false, true} {
		for _, hostQueue := range []bool{false, true} {
			agent := core.NewAgent(nil, "test-model", "", nil)
			i := NewInteractive(InteractiveConfig{Agent: agent})
			i.busy = true
			i.cancelTurn = func() {}
			i.ed.SetValue("existing draft")
			messages := []core.QueuedMessage{{Text: "worker report", HostEvent: true}}
			want := "existing draft"
			if hasUserMessage {
				messages = append([]core.QueuedMessage{{Text: "recover this draft"}}, messages...)
				want = "recover this draft"
			}
			if hostQueue {
				i.queued = messages
			} else {
				for _, message := range messages {
					agent.QueuePrompt(message)
				}
			}
			i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc})
			if got := i.ed.Value(); got != want {
				t.Fatalf("user=%v hostQueue=%v: editor = %q, want %q", hasUserMessage, hostQueue, got, want)
			}
		}
	}
}

func TestSlidingQueueHintOnlyForUserMessages(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "vev")
	for _, hostQueue := range []bool{false, true} {
		for _, hasUserMessage := range []bool{false, true} {
			agent := core.NewAgent(nil, "test-model", "", nil)
			term := &alertTestTerminal{}
			i := NewInteractive(InteractiveConfig{Agent: agent, Terminal: term, Theme: tui.Dark})
			i.rend.Resize(80, 24)
			messages := []core.QueuedMessage{{Text: "worker report", HostEvent: true}}
			if hasUserMessage {
				messages = append(messages, core.QueuedMessage{Text: "user draft"})
			}
			if hostQueue {
				i.queued = messages
			} else {
				for _, message := range messages {
					agent.QueuePrompt(message)
				}
			}
			i.redraw()
			output := stripANSIBytes(term.String())
			if !strings.Contains(output, "worker report") {
				t.Fatal("host report missing from sliding queue")
			}
			if got := strings.Contains(output, "Press Alt+Shift+↑"); got != hasUserMessage {
				t.Fatalf("user=%v hostQueue=%v: recall hint visible=%v", hasUserMessage, hostQueue, got)
			}
		}
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
