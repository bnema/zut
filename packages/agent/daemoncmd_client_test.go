package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/daemon"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type cliTestSession struct {
	h      *sessionmocks.MockHandle
	mu     sync.Mutex
	info   session.Info
	done   chan struct{}
	once   sync.Once
	closed chan string
}

func newCLITestSession(t *testing.T, info session.Info) *cliTestSession {
	s := &cliTestSession{h: sessionmocks.NewMockHandle(t), info: info, done: make(chan struct{}), closed: make(chan string, 1)}
	s.h.EXPECT().Info().RunAndReturn(func() session.Info { s.mu.Lock(); defer s.mu.Unlock(); return s.info }).Maybe()
	s.h.EXPECT().Done().Return(s.done).Maybe()
	s.h.EXPECT().Idle().Return(true).Maybe()
	s.h.EXPECT().SetAttach(mock.Anything, mock.Anything, mock.Anything).Maybe()
	s.h.EXPECT().Subscribe(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, _ uint64) (<-chan session.Event, error) {
		ch := make(chan session.Event)
		go func() {
			select {
			case <-ctx.Done():
			case <-s.done:
			}
			close(ch)
		}()
		return ch, nil
	}).Maybe()
	s.h.EXPECT().Close(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, reason string) error {
		s.once.Do(func() { close(s.done); s.closed <- reason })
		return nil
	}).Maybe()
	return s
}

// startCLIDaemon runs a real in-process daemon whose sessions are created
// from infos (in order), and points ZUT_DAEMON_SOCKET at it.
func startCLIDaemon(t *testing.T, infos ...session.Info) []*cliTestSession {
	dir, err := os.MkdirTemp("", "zd")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "d.sock")
	t.Setenv("ZUT_DAEMON_SOCKET", path)

	sessions := make([]*cliTestSession, len(infos))
	f := sessionmocks.NewMockFactory(t)
	for i, in := range infos {
		sessions[i] = newCLITestSession(t, in)
		f.EXPECT().Create(mock.Anything, mock.Anything).Return(sessions[i].h, nil).Once()
	}
	d := daemon.New(f)
	ready := make(chan struct{})
	runErr := make(chan error, 1)
	go func() {
		runErr <- d.Run(context.Background(), daemon.Options{SocketPath: path, Home: t.TempDir(), Version: "v", GCInterval: time.Hour, Ready: ready})
	}()
	t.Cleanup(func() {
		d.Stop()
		select {
		case <-runErr:
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	select {
	case <-ready:
	case err := <-runErr:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("daemon not ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dialExistingDaemon(ctx)
	require.NoError(t, err)
	defer c.Disconnect()
	for range infos {
		_, err := c.Create(ctx, session.CreateSpec{CWD: "/w", Env: []string{}, Args: []string{}})
		require.NoError(t, err)
	}
	return sessions
}

func twoSessions() []session.Info {
	now := time.Now()
	return []session.Info{
		{ID: "abc123", Title: "first", CWD: "/w", RepoRoot: "/w", Model: "m1", Attach: session.AttachStateDetached, Run: session.RunStateIdle, CreatedAt: now.Add(-2 * time.Hour),
			Residents: []session.ResidentSummary{{ID: "r1", Profile: "worker", State: "running", Task: "do it"}}},
		{ID: "abd456", Title: "second", CWD: "/x", RepoRoot: "/x", Model: "m2", Attach: session.AttachStateDetached, Run: session.RunStateIdle, CreatedAt: now.Add(-time.Minute),
			Activity: session.Activity{Kind: "tool", Tool: "bash"}},
	}
}

func TestLsText(t *testing.T) {
	startCLIDaemon(t, twoSessions()...)
	var out, errb bytes.Buffer
	require.Equal(t, 0, runLsCommand(nil, &out, &errb), errb.String())
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	require.Len(t, lines, 3)
	require.Equal(t, []string{"ID", "ATTACH", "STATE", "ACTIVITY", "CWD", "TITLE", "MODEL", "AGE"}, strings.Fields(lines[0]))
	require.Equal(t, "ID      ATTACH    STATE  ACTIVITY   CWD  TITLE   MODEL  AGE", lines[0])
	require.Equal(t, []string{"abc123", "detached", string(session.RunStateIdle), "-", "/w", "first", "m1", "2h"}, strings.Fields(lines[1]))
	require.Equal(t, []string{"abd456", "detached", string(session.RunStateIdle), "tool:bash", "/x", "second", "m2", "1m"}, strings.Fields(lines[2]))
}

func TestLsJSON(t *testing.T) {
	startCLIDaemon(t, twoSessions()...)
	var out, errb bytes.Buffer
	require.Equal(t, 0, runLsCommand([]string{"--json"}, &out, &errb), errb.String())
	var got []session.Info
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	require.Len(t, got, 2)
	require.Equal(t, "abc123", got[0].ID)
	require.Equal(t, "second", got[1].Title)
}

func TestTreeText(t *testing.T) {
	startCLIDaemon(t, twoSessions()...)
	var out, errb bytes.Buffer
	require.Equal(t, 0, runTreeCommand(nil, &out, &errb), errb.String())
	idle := string(session.RunStateIdle)
	require.Equal(t, "/w\n  abc123  detached  "+idle+"  first\n    r1  worker  running  do it\n/x\n  abd456  detached  "+idle+"  second\n", out.String())

	out.Reset()
	require.Equal(t, 0, runTreeCommand([]string{"--json"}, &out, &errb), errb.String())
	var tree session.Tree
	require.NoError(t, json.Unmarshal(out.Bytes(), &tree))
	require.Len(t, tree.Workspaces, 2)
}

func TestKillByPrefix(t *testing.T) {
	ss := startCLIDaemon(t, twoSessions()...)
	var out, errb bytes.Buffer
	require.Equal(t, 0, runKillCommand([]string{"abd"}, &out, &errb), errb.String())
	require.Equal(t, "killed abd456\n", out.String())
	select {
	case reason := <-ss[1].closed:
		require.Equal(t, "killed", reason)
	case <-time.After(5 * time.Second):
		t.Fatal("session not killed")
	}
	select {
	case <-ss[0].closed:
		t.Fatal("wrong session killed")
	default:
	}
}

func TestKillAmbiguousPrefix(t *testing.T) {
	ss := startCLIDaemon(t, twoSessions()...)
	var out, errb bytes.Buffer
	require.Equal(t, 1, runKillCommand([]string{"ab"}, &out, &errb))
	require.Contains(t, errb.String(), `session prefix "ab" is ambiguous: abc123, abd456`)
	require.Empty(t, out.String())
	for _, s := range ss {
		select {
		case <-s.closed:
			t.Fatal("session killed on ambiguous prefix")
		default:
		}
	}
}

func TestLsTreeKillNoDaemon(t *testing.T) {
	dir, err := os.MkdirTemp("", "zd")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("ZUT_DAEMON_SOCKET", filepath.Join(dir, "none.sock"))
	for name, run := range map[string]func() (int, string){
		"ls":   func() (int, string) { var o, e bytes.Buffer; return runLsCommand(nil, &o, &e), e.String() },
		"tree": func() (int, string) { var o, e bytes.Buffer; return runTreeCommand(nil, &o, &e), e.String() },
		"kill": func() (int, string) { var o, e bytes.Buffer; return runKillCommand([]string{"x"}, &o, &e), e.String() },
	} {
		code, msg := run()
		require.Equal(t, 1, code, name)
		require.Equal(t, "zut daemon is not running\n", msg, name)
	}
	_, err = os.Stat(filepath.Join(dir, "none.sock"))
	require.True(t, os.IsNotExist(err), "no daemon must be spawned")
}
