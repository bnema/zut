package session_test

import (
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/stretchr/testify/require"
)

func TestRefreshToolsWhenIdleRunsImmediatelyWhenIdle(t *testing.T) {
	refresh := sessionmocks.NewMockToolRefresher(t)
	refresh.EXPECT().RefreshTools().Return(nil).Once()
	h := session.NewToolRefreshHarness(t, session.Deps{ToolRefresher: refresh})
	require.NoError(t, h.C.RefreshToolsWhenIdle(h.Ctx))
	require.False(t, h.Pending(t)) // also a barrier: the posted refresh ran
}

func TestRefreshToolsWhenIdleDefersUntilTurnEnds(t *testing.T) {
	refresh := sessionmocks.NewMockToolRefresher(t)
	h := session.NewToolRefreshHarness(t, session.Deps{ToolRefresher: refresh})
	h.SetBusy(t, true)
	// Busy: recorded, not run (the mock fails on an unexpected call).
	require.NoError(t, h.C.RefreshToolsWhenIdle(h.Ctx))
	require.NoError(t, h.C.RefreshToolsWhenIdle(h.Ctx))
	require.True(t, h.Pending(t))
	// Coalesced into one refresh once the turn ends.
	refresh.EXPECT().RefreshTools().Return(nil).Once()
	h.SetBusy(t, false)
	require.False(t, h.Pending(t))
}
