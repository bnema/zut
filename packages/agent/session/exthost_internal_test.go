package session

import (
	"context"
	"github.com/bnema/zut/packages/agent/extensions"
	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

// Attachment is seeded on the actor: lifecycle is owned by another lane and
// its baseline SetAttach is intentionally a stub.
func TestExtensionAttachedSlash(t *testing.T) {
	c, ctx := newTestController(t)
	require.NoError(t, c.call(ctx, func() { c.attach = AttachStateAttached; c.updateInfo() }))
	h := c.ExtensionHooks()
	require.True(t, h.(extensions.InteractiveToolHostHooks).SupportsInteractiveTools())
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	h.SubmitSlash(" /compact ")
	select {
	case ev := <-events:
		require.Equal(t, EvExtInsert, ev.Kind)
		require.Equal(t, ExtInsertEvent{Text: "/compact", Submit: true}, ev.Data)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestExtensionHooksFromActor(t *testing.T) {
	c, ctx := newTestController(t)
	h := c.ExtensionHooks()
	require.NoError(t, c.call(ctx, func() {
		h.Notify("a", "info", "one")
		h.Insert("two")
		h.SetWidget("a", "w", "", "", []string{"three"})
		h.OpenPanel("a", extproto.PanelSpec{ID: "p", Lines: []string{"four"}})
		h.Submit("prompt")
		h.SubmitSlash("/unknown")
	}))
	_, err := c.Snapshot(ctx)
	require.NoError(t, err)
	completed := make(chan struct{})
	go func() { _ = c.Close(ctx, "test"); close(completed) }()
	select {
	case <-completed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

// SetExtensionTestPort injects the existing generated Port mock without an
// import cycle between internal tests and sessionmocks.
func SetExtensionTestPort(c *Controller, ctx context.Context, p Port) error {
	return c.call(ctx, func() { c.ext.port = p })
}
func TestExtensionPanelEventIsolation(t *testing.T) {
	c, ctx := newTestController(t)
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	h := c.ExtensionHooks()
	for _, op := range []string{"open", "update", "close"} {
		switch op {
		case "open":
			h.OpenPanel("a", extproto.PanelSpec{ID: "p", Lines: []string{"original"}})
		case "update":
			h.UpdatePanel("a", "p", "", []string{"original"}, "")
		case "close":
			h.ClosePanel("a", "p")
		}
		select {
		case ev := <-events:
			payload := ev.Data.(ExtPanelEvent)
			require.Equal(t, op, payload.Op)
			payload.Panel.Spec.Lines[0] = "mutated"
			snap, err := c.Snapshot(ctx)
			require.NoError(t, err)
			if op != "close" {
				require.Equal(t, "original", snap.ExtChrome.Panels[0].Spec.Lines[0])
			} else {
				require.Empty(t, snap.ExtChrome.Panels)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

// ExtShutdown and ExtDispatcherDone expose lifecycle hooks to external tests
// that need the generated Port mock.
func ExtShutdown(c *Controller)                       { c.extShutdown() }
func ExtDispatcherDone(c *Controller) <-chan struct{} { return c.ext.dispatcherDone }

func TestExtensionMailboxOverflowReportsOnce(t *testing.T) {
	c, ctx := newTestController(t)
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	release := make(chan struct{})
	blocked := make(chan struct{})
	require.NoError(t, c.post(ctx, func() { close(blocked); <-release }))
	<-blocked
	h := c.ExtensionHooks()
	for i := 0; i < 1100; i++ {
		h.Notify("flood", "info", "n") // must never block while the actor is busy
	}
	c.ext.mu.Lock()
	require.Len(t, c.ext.mailbox, 1024)
	c.ext.mu.Unlock()
	close(release)
	var notes, overloads int
	for notes+overloads < 1025 {
		select {
		case ev := <-events:
			if ev.Kind != EvNote {
				continue
			}
			if ev.Data.(NoteEvent).Note.Text == "extension flood overloaded: hook dropped" {
				require.Equal(t, "error", ev.Data.(NoteEvent).Note.Level)
				overloads++
			} else {
				notes++
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	require.Equal(t, 1024, notes)
	require.Equal(t, 1, overloads)
}

func TestExtensionShutdownDiscardsPendingAndClosesAdmission(t *testing.T) {
	c, ctx := newTestController(t)
	release := make(chan struct{})
	blocked := make(chan struct{})
	require.NoError(t, c.post(ctx, func() { close(blocked); <-release }))
	<-blocked
	h := c.ExtensionHooks()
	h.Notify("a", "info", "pending")
	c.extShutdown()
	h.Notify("a", "info", "after")
	h.SetStatus("a", "s", "info", "after")
	c.ext.mu.Lock()
	require.Empty(t, c.ext.mailbox)
	c.ext.mu.Unlock()
	close(release)
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Empty(t, snap.Notes)
	require.Empty(t, snap.ExtChrome.Statuses)
	select {
	case <-c.ext.dispatcherDone:
	case <-ctx.Done():
		t.Fatal("dispatcher still running")
	}
}

func TestExtensionUnknownSlashThenClearNotes(t *testing.T) {
	c, ctx := newTestController(t)
	h := c.ExtensionHooks()
	h.SubmitSlash("/unknown")
	h.ClearNotes("extensions")
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Empty(t, snap.Notes)
	time.Sleep(20 * time.Millisecond)
	snap, err = c.Snapshot(ctx)
	require.NoError(t, err)
	require.Empty(t, snap.Notes)
}
