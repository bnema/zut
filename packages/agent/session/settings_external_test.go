package session_test

import (
	"context"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/stretchr/testify/require"
	"path/filepath"
	"testing"
	"time"
)

func TestSettingsCWDAndTitlePersistence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	base := t.TempDir()
	target := filepath.Join(base, "child")
	store := sessionmocks.NewMockSessionStore(t)
	store.EXPECT().CurrentSessionPath().Return("").Maybe()
	store.EXPECT().ChangeCWD(target).Return(nil).Once()
	store.EXPECT().PersistTitle("Useful title").Return(nil).Once()
	refresh := sessionmocks.NewMockToolRefresher(t)
	refresh.EXPECT().RefreshTools().Return(nil).Once()
	c := session.NewController(ctx, session.Deps{CWD: base, SessionStore: store, ToolRefresher: refresh})
	defer func() { cancel(); <-c.Done() }()
	require.NoError(t, c.ChangeCWD(ctx, "child"))
	require.Equal(t, target, c.Info().CWD)
	require.NoError(t, c.SetTitle(ctx, "Useful title"))
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, "Useful title", snap.Info.Title)
}
