package modes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/tui"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func applyLiveRefresh(t *testing.T, i *Interactive) {
	t.Helper()
	select {
	case fn := <-i.loopCalls:
		fn()
	case <-time.After(5 * time.Second):
		t.Fatal("missing refresh application")
	}
}

func TestLiveRefreshSerialAndStopsOnClose(t *testing.T) {
	client := newMockSessionClient(t)
	entered, release := make(chan struct{}), make(chan struct{})
	client.EXPECT().List(mock.Anything).RunAndReturn(func(ctx context.Context) ([]session.Info, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return liveInfos(), nil
	}).Once()
	client.EXPECT().List(mock.Anything).Return([]session.Info{{ID: "s2"}, {ID: "s1"}}, nil).Once()
	i := NewInteractive(InteractiveConfig{Terminal: &alertTestTerminal{}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	i.sessionDialog.active = true
	i.sessionDialog.liveCancel = cancel
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() { defer close(done); i.refreshLiveSessions(ctx, client, "s1", 0, ticks) }()
	waitSignal(t, entered, "list entered")
	// A tick cannot be consumed while List is in flight (no second call).
	select {
	case ticks <- time.Now():
		t.Fatal("poller accepted tick during List")
	default:
	}
	close(release)
	applyLiveRefresh(t, i)
	i.sessionDialog.liveCursor = 1
	i.sessionDialog.liveFocus = true
	select {
	case ticks <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("poller did not wait for tick")
	}
	applyLiveRefresh(t, i)
	require.Equal(t, "s2", i.sessionDialog.live[i.sessionDialog.liveCursor].ID)
	i.mu.Lock()
	i.sessionDialog.Close()
	i.mu.Unlock()
	waitSignal(t, done, "refresh stopped on close")
	select {
	case ticks <- time.Now():
		t.Fatal("refresh still consuming ticks")
	default:
	}
}

func TestLiveRefreshErrorDedupResetsOnSuccess(t *testing.T) {
	client := newMockSessionClient(t)
	failure := errors.New("offline")
	client.EXPECT().List(mock.Anything).Return(nil, failure).Twice()
	client.EXPECT().List(mock.Anything).Return(liveInfos(), nil).Once()
	client.EXPECT().List(mock.Anything).Return(nil, failure).Once()
	i := NewInteractive(InteractiveConfig{Terminal: &alertTestTerminal{}})
	i.sessionDialog.active = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() { defer close(done); i.refreshLiveSessions(ctx, client, "s1", 0, ticks) }()
	for step := 0; step < 4; step++ {
		if step > 0 {
			select {
			case ticks <- time.Now():
			case <-time.After(5 * time.Second):
				t.Fatal("missing tick receiver")
			}
		}
		applyLiveRefresh(t, i)
		if step == 0 || step == 3 {
			require.Equal(t, "sessions: list live: offline", i.statusErr)
		} else {
			require.Equal(t, "unrelated status", i.statusErr)
		}
		i.statusErr = "unrelated status"
	}
	cancel()
	waitSignal(t, done, "refresh stopped on context cancellation")
}

func TestLiveRefreshSurvivesScopeTabs(t *testing.T) {
	client := newMockSessionClient(t)
	client.EXPECT().List(mock.Anything).Return(liveInfos(), nil).Once()
	client.EXPECT().List(mock.Anything).Return([]session.Info{{ID: "s1", Title: "refreshed"}}, nil).Once()
	i := NewInteractive(InteractiveConfig{Terminal: &alertTestTerminal{}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root, cwd := t.TempDir(), t.TempDir()
	i.sessionDialog.Open(ctx, root, cwd)
	pollCtx, stop := context.WithCancel(ctx)
	i.sessionDialog.liveCancel = stop
	ticks := make(chan time.Time)
	done := make(chan struct{})
	generation := i.sessionDialog.liveGeneration
	go func() { defer close(done); i.refreshLiveSessions(pollCtx, client, "s1", generation, ticks) }()
	applyLiveRefresh(t, i)
	for n := 0; n < 2; n++ {
		act := i.sessionDialog.HandleKey(tui.Key{Kind: tui.KeyTab})
		require.True(t, act.ToggleScope)
		i.sessionDialog.Open(ctx, root, cwd, !i.sessionDialog.allScope)
		require.Contains(t, strings.Join(i.sessionDialog.renderLive(tui.Theme{}, 160), "\n"), "mine")
		require.NoError(t, pollCtx.Err())
	}
	select {
	case ticks <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("poller stopped after scope change")
	}
	applyLiveRefresh(t, i)
	require.Contains(t, strings.Join(i.sessionDialog.renderLive(tui.Theme{}, 160), "\n"), "refreshed")
	i.sessionDialog.Close()
	waitSignal(t, done, "refresh stopped after real close")
}
