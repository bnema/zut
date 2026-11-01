package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/daemonclient"
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
	require.NotNil(t, cfg.AuthManager, "/login must work in the daemon TUI client")
	require.NotNil(t, cfg.SetKimiCLIFallbackDisabled, "/logout kimi must disable the Kimi CLI fallback")
	require.Nil(t, cfg.WebSearchToolAllowed, "attachment must not infer the session's limits")
	require.Nil(t, cfg.AutoSubagentsToolAllowed)
	require.Nil(t, cfg.AutoSubagentsStatusToolAllowed)
	require.Nil(t, cfg.AutoSubagentsStopToolAllowed)
	require.Nil(t, cfg.AutoSubagentsResumeToolAllowed)
	require.Nil(t, cfg.AutoSubagentsInterruptToolAllowed)
	require.False(t, cfg.WebSearchInvocationOverride)
}

func TestAttachDaemonStreamLogsToClientFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	startCLIDaemon(t, twoSessions()...)
	logger, closeLog := clientTimingLog()
	require.NotNil(t, logger)
	defer closeLog()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dialExistingDaemonWithOptions(ctx, daemonclient.Options{ClientKind: "tui", Logger: logger})
	require.NoError(t, err)
	defer c.Disconnect()
	_, err = c.Port("abc123", true).Subscribe(ctx, 0)
	require.NoError(t, err)
	require.NoError(t, c.Disconnect())
	data, err := os.ReadFile(clientLogPath(home))
	require.NoError(t, err)
	require.Contains(t, string(data), `msg="client stream ended"`)
	require.Contains(t, string(data), "session=abc123")
	require.Contains(t, string(data), "reason=canceled_or_disconnected")
}

func TestDaemonClientRetriesExpiredSnapshotAttach(t *testing.T) {
	sessions := startCLIDaemon(t, twoSessions()[0])
	h := sessions[0].h
	// Replace the harness's default Subscribe behavior after its daemon watcher
	// has subscribed. All client subscriptions below are explicitly controlled.
	h.EXPECT().Subscribe(mock.Anything, mock.Anything).Unset()
	dropped := make(chan session.Event, 1)
	h.EXPECT().Subscribe(mock.Anything, uint64(0)).Return((<-chan session.Event)(dropped), nil).Once()
	h.EXPECT().Subscribe(mock.Anything, uint64(0)).Return(nil, session.ErrSeqTooOld).Once()
	h.EXPECT().Subscribe(mock.Anything, uint64(10)).Return(nil, session.ErrSeqTooOld).Once()
	live := make(chan session.Event, 1)
	live <- session.Event{Seq: 21, Session: "abc123", Kind: session.EvTitle, Data: session.TitleEvent{Title: "live"}}
	h.EXPECT().Subscribe(mock.Anything, uint64(20)).Return((<-chan session.Event)(live), nil).Once()
	snapshots := 0
	h.EXPECT().Snapshot(mock.Anything).RunAndReturn(func(context.Context) (session.Snapshot, error) {
		snapshots++
		seq := uint64(10)
		if snapshots >= 3 {
			seq = 20
		}
		return session.Snapshot{Seq: seq, Info: twoSessions()[0]}, nil
	}).Times(3)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dialExistingDaemon(ctx)
	require.NoError(t, err)
	defer c.Disconnect()
	events, err := c.Port("abc123", true).Subscribe(ctx, 0)
	require.NoError(t, err)
	dropped <- session.Event{Seq: 1, Session: "abc123", Kind: session.EvAttach, Data: session.AttachEvent{State: session.AttachStateDetached, Reason: "slow_consumer"}}
	for _, want := range []session.Event{{Seq: 10, Kind: session.EvMessagesReplaced}, {Seq: 20, Kind: session.EvMessagesReplaced}, {Seq: 21, Kind: session.EvTitle}} {
		select {
		case ev, ok := <-events:
			require.True(t, ok, "stream ended during snapshot retry")
			require.Equal(t, want.Seq, ev.Seq)
			require.Equal(t, want.Kind, ev.Kind)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestDaemonClientBurstRecovery(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprint("expired=", expired), func(t *testing.T) {
			h := startCLIDaemon(t, twoSessions()[0])[0].h
			h.EXPECT().Subscribe(mock.Anything, mock.Anything).Unset()
			const count = daemonclient.SubscriberBuffer*3 + 10
			burst := make(chan session.Event, count)
			for seq := uint64(1); seq <= count; seq++ {
				burst <- session.Event{Seq: seq, Session: "abc123", Kind: session.EvTitle}
			}
			h.EXPECT().Subscribe(mock.Anything, uint64(0)).Return((<-chan session.Event)(burst), nil).Once()
			live := make(chan session.Event, count)
			if expired {
				h.EXPECT().Subscribe(mock.Anything, uint64(daemonclient.SubscriberBuffer)).Return(nil, session.ErrSeqTooOld).Once()
				h.EXPECT().Subscribe(mock.Anything, uint64(count)).Return((<-chan session.Event)(live), nil).Once()
			} else {
				for seq := uint64(daemonclient.SubscriberBuffer + 1); seq <= count; seq++ {
					live <- session.Event{Seq: seq, Session: "abc123", Kind: session.EvTitle}
				}
				h.EXPECT().Subscribe(mock.Anything, uint64(daemonclient.SubscriberBuffer)).Return((<-chan session.Event)(live), nil).Once()
			}
			h.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Seq: count, Info: twoSessions()[0]}, nil).Maybe()
			// A standard-library pipe observes the overflow log without polling or
			// racing the consumer against the daemon's event writer.
			reader, writer := io.Pipe()
			lines := make(chan string, 16)
			scanned := make(chan struct{})
			go func() {
				defer close(scanned)
				scanner := bufio.NewScanner(reader)
				for scanner.Scan() {
					lines <- scanner.Text()
				}
			}()
			defer func() { writer.Close(); <-scanned; reader.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			c, err := dialExistingDaemonWithOptions(ctx, daemonclient.Options{Logger: slog.New(slog.NewTextHandler(writer, nil))})
			require.NoError(t, err)
			defer c.Disconnect()
			events, err := c.Port("abc123", true).Subscribe(ctx, 0)
			require.NoError(t, err)
			select {
			case line := <-lines:
				require.Contains(t, line, "reason=local_buffer_full")
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			receive := func() session.Event {
				t.Helper()
				select {
				case ev, ok := <-events:
					require.True(t, ok)
					return ev
				case <-ctx.Done():
					t.Fatal(ctx.Err())
					return session.Event{}
				}
			}
			limit := count
			if expired {
				limit = daemonclient.SubscriberBuffer
			}
			for seq := 1; seq <= limit; seq++ {
				ev := receive()
				require.Equal(t, uint64(seq), ev.Seq)
				require.Equal(t, session.EvTitle, ev.Kind)
			}
			if expired {
				ev := receive()
				require.Equal(t, uint64(count), ev.Seq)
				require.Equal(t, session.EvMessagesReplaced, ev.Kind)
			}
			live <- session.Event{Seq: count + 1, Session: "abc123", Kind: session.EvTitle}
			require.Equal(t, uint64(count+1), receive().Seq)
			require.NoError(t, c.Disconnect())
			select {
			case line := <-lines:
				require.Contains(t, line, `msg="client stream ended"`)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}

func TestDaemonClientRepeatedSlowDropsRecover(t *testing.T) {
	h := startCLIDaemon(t, twoSessions()[0])[0].h
	h.EXPECT().Subscribe(mock.Anything, mock.Anything).Unset()
	subscribed := make(chan chan session.Event, 1)
	h.EXPECT().Subscribe(mock.Anything, uint64(0)).RunAndReturn(func(context.Context, uint64) (<-chan session.Event, error) {
		ch := make(chan session.Event, 1)
		subscribed <- ch
		return ch, nil
	}).Times(7)
	h.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: twoSessions()[0]}, nil).Times(6)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dialExistingDaemon(ctx)
	require.NoError(t, err)
	defer c.Disconnect()
	events, err := c.Port("abc123", true).Subscribe(ctx, 0)
	require.NoError(t, err)
	for i := 0; i < 6; i++ {
		select {
		case ch := <-subscribed:
			ch <- session.Event{Seq: 1, Session: "abc123", Kind: session.EvAttach, Data: session.AttachEvent{State: session.AttachStateDetached, Reason: "slow_consumer"}}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	select {
	case ch := <-subscribed:
		ch <- session.Event{Seq: 1, Session: "abc123", Kind: session.EvTitle}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case ev, ok := <-events:
		require.True(t, ok)
		require.Equal(t, session.EvTitle, ev.Kind)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
