package session_test

import (
	"context"
	"errors"
	"github.com/bnema/zut/packages/agent/extproto"
	. "github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestConfirmGateRememberAllAndEmission(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ext := sessionmocks.NewMockExtensionHost(t)
	ext.EXPECT().EmitEvent(extproto.EventFromHost{Event: "tool_confirmation_requested", ToolName: "bash", ToolPreview: "hi"}).Once()
	c, confirmer := ConfirmTestSetup(ctx, Deps{Extensions: ext}, true)
	gate := core.NewConfirmGate(confirmer)
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	done := make(chan bool, 1)
	go func() { ok, _, _ := gate.Check("bash", "hi"); done <- ok }()
	p := (<-events).Data.(PendingEvent).Pending
	require.NoError(t, c.AnswerConfirmation(ctx, p.ID, core.ConfirmDecision{Allow: true, RememberAll: true}))
	require.True(t, <-done)
	ok, _, _ := gate.Check("other", "next")
	require.True(t, ok)
}
func TestConfirmPersistenceFailureNote(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := sessionmocks.NewMockDeferredStore(t)
	store.EXPECT().List().Return(nil)
	store.EXPECT().Append(mock.Anything).Return(errors.New("disk unavailable"))
	c, confirmer := ConfirmTestSetup(ctx, Deps{DeferredStore: store}, false)
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	require.False(t, confirmer.(core.ToolCallConfirmer).ConfirmToolCall(core.ToolCallConfirmation{Name: "bash"}).Allow)
	require.Equal(t, EvNote, (<-events).Kind)
	require.Equal(t, EvDeferred, (<-events).Kind)
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Len(t, snap.Deferred, 1)
}

func TestConfirmSendBackRemoveRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := sessionmocks.NewMockDeferredStore(t)
	store.EXPECT().List().Return([]DeferredAction{{ID: "retry", Tool: "bash", Summary: "hi"}}).Once()
	store.EXPECT().Remove("retry").Return(errors.New("disk unavailable")).Once()
	store.EXPECT().Remove("retry").Return(nil).Once()
	c, _ := ConfirmTestSetup(ctx, Deps{DeferredStore: store}, false)
	ConfirmTestDispatch(t, c)
	require.Error(t, c.ResolveDeferred(ctx, "retry", "send_to_agent"))
	require.Equal(t, 1, c.Info().Deferred)
	require.NoError(t, c.ResolveDeferred(ctx, "retry", "send_to_agent"))
	require.Zero(t, c.Info().Deferred)
}
