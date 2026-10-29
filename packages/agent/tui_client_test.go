package agent

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func attachTestInfos() []session.Info {
	now := time.Now()
	return []session.Info{
		{ID: "abc123", Title: "fix parser", CWD: "/w", Attach: session.AttachStateDetached, UpdatedAt: now.Add(-time.Hour)},
		{ID: "abd456", Title: "fix lexer", CWD: "/w", Attach: session.AttachStateDetached, UpdatedAt: now},
		{ID: "xyz789", Title: "docs", CWD: "/w", Attach: session.AttachStateAttached, UpdatedAt: now.Add(time.Hour)},
		{ID: "qqq000", Title: "other", CWD: "/x", Attach: session.AttachStateDetached, UpdatedAt: now.Add(2 * time.Hour)},
	}
}

func TestDaemonSessionClientAttachReturnsPort(t *testing.T) {
	infos := twoSessions()
	sessions := startCLIDaemon(t, infos...)
	sessions[0].h.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: infos[0]}, nil).Maybe()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dialExistingDaemon(ctx)
	require.NoError(t, err)
	defer c.Disconnect()

	a := newDaemonSessionClient(c)
	p, err := a.Attach(ctx, session.ID("abc123"))
	require.NoError(t, err)
	snap, err := p.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, "abc123", snap.Info.ID)
	_, err = p.Subscribe(ctx, 0)
	require.NoError(t, err)

	// A bad id errors through the caller's Snapshot.
	bad, err := a.Attach(ctx, session.ID("nope"))
	require.NoError(t, err)
	_, err = bad.Snapshot(ctx)
	require.Error(t, err)
}

func TestResolveAttachTarget(t *testing.T) {
	startCLIDaemon(t, attachTestInfos()...)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dialExistingDaemon(ctx)
	require.NoError(t, err)
	defer c.Disconnect()
	list, err := c.List(ctx)
	require.NoError(t, err)

	for arg, want := range map[string]string{"abc123": "abc123", "abd": "abd456", "x": "xyz789", "fix p": "abc123", "doc": "xyz789"} {
		got, err := resolveAttachTarget(list, arg)
		require.NoError(t, err, arg)
		require.Equal(t, want, got, arg)
	}
	for _, arg := range []string{"ab", "fix", "zzz"} {
		_, err := resolveAttachTarget(list, arg)
		require.Error(t, err, arg)
	}

	got, err := latestDetachedInCWD(list, "/w")
	require.NoError(t, err)
	require.Equal(t, "abd456", got)
	_, err = latestDetachedInCWD(list, "/none")
	require.Error(t, err)
}

func TestAttachNoArgNoCandidatePrintsList(t *testing.T) {
	startCLIDaemon(t, twoSessions()...)
	t.Chdir(t.TempDir())
	var out, errb bytes.Buffer
	require.Equal(t, 1, runAttachCommand(nil, &out, &errb))
	require.Contains(t, errb.String(), "no detached session")
	require.Contains(t, out.String(), "abc123")
}

func TestAttachNoDaemon(t *testing.T) {
	t.Setenv("ZUT_DAEMON_SOCKET", filepath.Join(t.TempDir(), "none.sock"))
	var out, errb bytes.Buffer
	require.Equal(t, 1, runAttachCommand([]string{"abc"}, &out, &errb))
	require.Contains(t, errb.String(), daemonNotRunningMsg)
}
