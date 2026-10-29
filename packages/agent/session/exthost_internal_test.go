package session

import (
	"context"
	"github.com/bnema/zut/packages/agent/extensions"
	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/stretchr/testify/require"
	"strconv"
	"sync/atomic"
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
	return c.call(ctx, func() { c.ext.mu.Lock(); c.ext.port = p; c.ext.mu.Unlock() })
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

// ExtDomainQueueLen reads the accepted-but-unrun domain queue on the actor,
// after the actor has drained every hook admitted before the call.
func ExtDomainQueueLen(ctx context.Context, c *Controller) (int, error) {
	var n int
	err := c.call(ctx, func() { n = len(c.ext.domainJobs) })
	return n, err
}

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
}

func TestExtensionNotesCapped(t *testing.T) {
	c, ctx := newTestController(t)
	h := c.ExtensionHooks()
	for i := 0; i < 150; i++ {
		h.Notify("a", "info", strconv.Itoa(i))
		h.Display("b", strconv.Itoa(i))
	}
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Len(t, snap.Notes, maxExtNotes)
	require.Equal(t, "[a] 50", snap.Notes[0].Text)
	require.Equal(t, "[b] 149", snap.Notes[len(snap.Notes)-1].Text)
}

func TestExtensionOverloadDuringDrainReportedOnNextWake(t *testing.T) {
	c, ctx := newTestController(t)
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	h := extHooks{c}
	h.admit("x", func() {
		for i := 0; i < 1025; i++ {
			h.ClearWidget("x", "none")
		}
	})
	for {
		select {
		case ev := <-events:
			if ev.Kind == EvNote {
				require.Equal(t, Note{Level: "error", Text: "extension x overloaded: hook dropped"}, ev.Data.(NoteEvent).Note)
				return
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestExtensionOverloadRateLimited(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	var now atomic.Int64
	now.Store(time.Unix(1000, 0).UnixNano())
	c := NewController(ctx, Deps{Clock: func() time.Time { return time.Unix(0, now.Load()) }})
	t.Cleanup(func() { cancel(); <-c.Done() })
	h := c.ExtensionHooks()
	flood := func() {
		release := make(chan struct{})
		blocked := make(chan struct{})
		require.NoError(t, c.post(ctx, func() { close(blocked); <-release }))
		<-blocked
		for i := 0; i < 1025; i++ {
			h.ClearWidget("x", "none")
		}
		close(release)
	}
	overloads := func() int {
		snap, err := c.Snapshot(ctx)
		require.NoError(t, err)
		return len(snap.Notes)
	}
	flood()
	require.Equal(t, 1, overloads())
	now.Add(int64(999 * time.Millisecond))
	flood()
	require.Equal(t, 1, overloads())
	now.Add(int64(time.Millisecond))
	flood()
	require.Equal(t, 2, overloads())
}
