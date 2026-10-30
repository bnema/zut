package agent

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
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
	for arg, want := range map[string]error{"ab": errSessionAmbiguous, "fix": errSessionAmbiguous, "zzz": errSessionNotFound} {
		_, err := resolveAttachTarget(list, arg)
		require.ErrorIs(t, err, want, arg)
	}
	_, err = resolveAttachTarget(list, "ab")
	require.EqualError(t, err, `session prefix "ab" is ambiguous: abc123, abd456`)
	_, err = resolveAttachTarget(list, "fix")
	require.EqualError(t, err, `session title prefix "fix" is ambiguous: abc123, abd456`)
	_, err = resolveAttachTarget(list, "zzz")
	require.EqualError(t, err, `no open session matches "zzz"`)

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

func TestDaemonSessionClientResumeStoredSession(t *testing.T) {
	home := sessionAssemblyEnv(t)
	t.Setenv("OPENAI_API_KEY", "test-resume-key")
	require.NoError(t, SaveConfig(Config{Provider: "openai", Model: "gpt-5"}))
	cwd := t.TempDir()
	stored, err := core.NewSession(home, cwd, "openai", "gpt-5", "test")
	require.NoError(t, err)
	require.NoError(t, stored.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "persisted history"}}}))
	path := stored.Path
	require.NoError(t, stored.Close())
	td := startFactoryDaemon(t, home, daemonTestSocket(t))
	c := dialTestDaemon(t, td.socket)
	ctx := factoryTestCtx(t)
	client := newDaemonSessionClient(c)
	info, err := client.Create(ctx, session.CreateSpec{CWD: cwd, ResumePath: path})
	require.NoError(t, err)
	require.Equal(t, path, info.SessionPath)
	port, err := client.Attach(ctx, session.ID(info.ID))
	require.NoError(t, err)
	snap, err := port.Snapshot(ctx)
	require.NoError(t, err)
	require.Len(t, snap.Messages, 1)
}

func TestDaemonShortIDResumeLookup(t *testing.T) {
	home := sessionAssemblyEnv(t)
	t.Setenv("OPENAI_API_KEY", "test-resume-key")
	require.NoError(t, SaveConfig(Config{Provider: "openai", Model: "gpt-5"}))
	cwd := t.TempDir()
	stored, err := core.NewSession(home, cwd, "openai", "gpt-5", "test")
	require.NoError(t, err)
	require.NoError(t, stored.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "saved"}}}))
	path, uuid := stored.Path, stored.ID
	require.NoError(t, stored.Close())
	td := startFactoryDaemon(t, home, daemonTestSocket(t))
	c := dialTestDaemon(t, td.socket)
	ctx := factoryTestCtx(t)
	info, err := c.Create(ctx, session.CreateSpec{CWD: cwd, Env: []string{"OPENAI_API_KEY=test-resume-key"}, ResumePath: path})
	require.NoError(t, err)
	_, err = c.Port(session.ID(info.ID), true).Subscribe(ctx, 0)
	require.NoError(t, err)
	require.NoError(t, c.Close(ctx, session.ID(info.ID), "user_exit"))
	list, err := c.List(ctx)
	require.NoError(t, err)
	for _, live := range list {
		require.NotEqual(t, info.ID, live.ID)
	}
	// This is the exact command emitted by the TUI's exit notice.
	command := "Resume this session: zut --resume " + info.ID
	args, err := ParseArgs(strings.Fields(strings.TrimPrefix(command, "Resume this session: zut ")))
	require.NoError(t, err)
	require.Equal(t, info.ID, args.ResumeSessionID)
	spec, err := buildCreateSpec(ctx, args, []string{"--resume", info.ID}, cwd, []string{"OPENAI_API_KEY=test-resume-key"}, defaultSessionLocator())
	require.NoError(t, err)
	require.Equal(t, path, spec.ResumePath)
	reopened, err := c.Create(ctx, *spec)
	require.NoError(t, err)
	require.NotEqual(t, info.ID, reopened.ID)
	require.Equal(t, path, reopened.SessionPath)
	found, err := core.FindManagedSessionByID(ctx, home, uuid)
	require.NoError(t, err)
	require.Equal(t, path, found)
	args, err = ParseArgs([]string{"--resume", "aaaaaaaaaaaa"})
	require.NoError(t, err)
	_, err = buildCreateSpec(ctx, args, nil, cwd, nil, defaultSessionLocator())
	require.ErrorContains(t, err, "not found")
}

func TestTUIClientConfigProjectsLaunchToolLimits(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("ZUT_THEME", "dark")
	for _, tt := range []struct {
		name          string
		args          []string
		web, override bool
	}{
		{"defaults", nil, true, false},
		{"no tools", []string{"--no-tools"}, false, false},
		{"read only", []string{"--tools", "read"}, false, false},
		{"explicit web", []string{"--tools", "web_search"}, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tuiClientConfig(&bytes.Buffer{}, &session.CreateSpec{Args: tt.args})
			require.NotNil(t, cfg.WebSearchToolAllowed)
			require.Equal(t, tt.web, *cfg.WebSearchToolAllowed)
			require.Equal(t, tt.override, cfg.WebSearchInvocationOverride)
			args, err := ParseArgs(tt.args)
			require.NoError(t, err)
			require.Equal(t, autoSubagentsToolAllowed(args), *cfg.AutoSubagentsToolAllowed)
			require.Equal(t, autoSubagentsStatusToolAllowed(args), *cfg.AutoSubagentsStatusToolAllowed)
			require.Equal(t, autoSubagentsStopToolAllowed(args), *cfg.AutoSubagentsStopToolAllowed)
			require.Equal(t, autoSubagentsResumeToolAllowed(args), *cfg.AutoSubagentsResumeToolAllowed)
			require.Equal(t, autoSubagentsInterruptToolAllowed(args), *cfg.AutoSubagentsInterruptToolAllowed)
		})
	}
	cfg := tuiClientConfig(&bytes.Buffer{}, nil)
	require.Nil(t, cfg.WebSearchToolAllowed, "attachment must not infer the session's limits")
	require.Nil(t, cfg.AutoSubagentsToolAllowed)
	require.Nil(t, cfg.AutoSubagentsStatusToolAllowed)
	require.Nil(t, cfg.AutoSubagentsStopToolAllowed)
	require.Nil(t, cfg.AutoSubagentsResumeToolAllowed)
	require.Nil(t, cfg.AutoSubagentsInterruptToolAllowed)
	require.False(t, cfg.WebSearchInvocationOverride)
}
