package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/stretchr/testify/require"
)

func TestConfirmDetachedPersists(t *testing.T) {
	home := t.TempDir()
	store, err := NewFileDeferredStore(home, "test")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := NewController(ctx, Deps{DeferredStore: store})
	d := c.confirmer().ConfirmToolCall(core.ToolCallConfirmation{Name: "bash", Summary: "echo hi"})
	require.False(t, d.Allow)
	require.Equal(t, detachedActionReason, d.Reason)
	reopened, err := NewFileDeferredStore(home, "test")
	require.NoError(t, err)
	require.Len(t, reopened.List(), 1)
	st, err := os.Stat(filepath.Join(home, "daemon", "sessions", "test", "deferred.jsonl"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), st.Mode().Perm())
	require.NoError(t, c.ResolveDeferred(ctx, reopened.List()[0].ID, "dismiss"))
	require.Empty(t, store.List())
}
func TestConfirmPendingDecisions(t *testing.T) {
	for _, mode := range []string{"allow", "deny", "detach", "abort"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := NewController(ctx, Deps{})
			require.NoError(t, c.call(ctx, func() { c.attach = AttachStateAttached }))
			events, err := c.Subscribe(ctx, 0)
			require.NoError(t, err)
			result := make(chan core.ConfirmDecision, 1)
			go func() { result <- c.confirmer().ConfirmToolCall(core.ToolCallConfirmation{Name: "bash"}) }()
			ev := <-events
			require.Equal(t, EvPending, ev.Kind)
			id := ev.Data.(PendingEvent).Pending.ID
			switch mode {
			case "allow", "deny":
				require.NoError(t, c.AnswerConfirmation(ctx, id, core.ConfirmDecision{Allow: mode == "allow", RememberAll: mode == "allow"}))
			case "detach":
				require.NoError(t, c.call(ctx, func() { c.attach = AttachStateDetached; c.denyAllPending("detached") }))
			case "abort":
				require.NoError(t, c.call(ctx, func() { c.denyAllPending("aborted") }))
			}
			d := <-result
			require.Equal(t, mode == "allow", d.Allow)
			require.Equal(t, mode == "allow", d.RememberAll)
			snap, err := c.Snapshot(ctx)
			require.NoError(t, err)
			require.Empty(t, snap.Pending)
			if mode == "detach" {
				require.Len(t, snap.Deferred, 1)
			} else {
				require.Empty(t, snap.Deferred)
			}
		})
	}
}
func TestConfirmAwaitPendingDetached(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := NewController(ctx, Deps{})
	_, err := c.awaitPending(ctx, PendingRequest{Kind: "rescue"})
	require.ErrorIs(t, err, ErrNotAttached)
}
