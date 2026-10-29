package session

import (
	"context"
	"github.com/bnema/zut/packages/agent/extensions"
	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/stretchr/testify/require"
	"testing"
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
