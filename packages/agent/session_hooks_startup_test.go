package agent

import (
	"context"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/bnema/zut/packages/agent/session"
)

// Extension chrome set before the controller binds is buffered and lands in
// the session snapshot once bound; cleared entries stay cleared.
func TestSessionHooksBufferStartupChrome(t *testing.T) {
	b := newSessionBindings()
	hooks := b.hooks()
	hooks.SetStatus("tasked-phases", "progress", "success", "2/4")
	hooks.SetWidget("tasked-phases", "plan", "above_input", "Plan", []string{"one"})
	hooks.SetWidget("tasked-phases", "cleared", "above_input", "old", []string{"old"})
	hooks.ClearWidget("tasked-phases", "cleared")
	hooks.SetStatus("tasked-phases", "removed", "info", "old")
	hooks.SetStatus("tasked-phases", "removed", "info", "")

	b.mu.Lock()
	if len(b.pending) != 6 {
		t.Fatalf("pending hook calls before bind = %d, want 6", len(b.pending))
	}
	b.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := session.NewController(ctx, session.Deps{})
	b.setHooks(c.ExtensionHooks())

	b.mu.Lock()
	pending := len(b.pending)
	b.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending hook calls after bind = %d, want flushed", pending)
	}
	var chrome session.ExtChrome
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap, err := c.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		chrome = snap.ExtChrome
		if len(chrome.Statuses["tasked-phases"]) == 1 && len(chrome.Widgets["tasked-phases"]) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("chrome after bind = statuses %v widgets %v, want one each", chrome.Statuses, chrome.Widgets)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := chrome.Statuses["tasked-phases"]["progress"].Text; got != "2/4" {
		t.Fatalf("status = %q", got)
	}
	if _, ok := chrome.Widgets["tasked-phases"]["plan"]; !ok {
		t.Fatalf("widgets = %v, want plan", chrome.Widgets)
	}
}

// A flood of startup alerts stays bounded before binding, and the latest
// alert reaches the controller once bound.
func TestSessionHooksBufferStartupAlertsBounded(t *testing.T) {
	b := newSessionBindings()
	hooks := b.hooks()
	hooks.Alert("question-ext", extproto.AlertRequest{Kind: extproto.AlertKindBell, Reason: "question_ready"})
	for n := 0; n < maxBufferedSessionHookCalls+8; n++ {
		hooks.Alert("question-ext", extproto.AlertRequest{Kind: extproto.AlertKindBell, Reason: "flood"})
	}
	b.mu.Lock()
	others, dropped := b.others, b.dropped
	b.mu.Unlock()
	if others != maxBufferedSessionHookCalls || dropped != 9 {
		t.Fatalf("buffered=%d dropped=%d, want bounded capacity %d and 9 dropped", others, dropped, maxBufferedSessionHookCalls)
	}

	events := boundHooks(t, b)
	for {
		ev := nextEvent(t, events)
		if ev.Kind != session.EvExtAlert {
			continue
		}
		if a := ev.Data.(session.ExtAlertEvent).Alert; a.Kind != extproto.AlertKindBell || a.Reason != "flood" {
			t.Fatalf("alert = %+v", a)
		}
		return
	}
}
