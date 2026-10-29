package session

import (
	"github.com/bnema/zut/packages/agent/extensions"
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
