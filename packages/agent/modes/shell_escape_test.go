package modes

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// newPortShellInteractive attaches an idle daemon session for shell escapes.
func newPortShellInteractive(t *testing.T) (*Interactive, *sessionmocks.MockPort) {
	t.Helper()
	port := sessionmocks.NewMockPort(t)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "shell", Run: session.RunStateIdle}, Seq: 1}, nil).Once()
	port.EXPECT().Subscribe(mock.Anything, uint64(0x1)).Return(make(chan session.Event), nil).Once()
	i := NewInteractive(InteractiveConfig{Port: port, CWD: t.TempDir()})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, i.attachPort(ctx))
	return i, port
}

func TestShellEscapeAddsOutputToAgentContext(t *testing.T) {
	i, port := newPortShellInteractive(t)
	sent := make(chan string, 1)
	release := make(chan struct{})
	done := make(chan struct{})
	port.EXPECT().AppendShellContext(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, text string) error {
		sent <- text
		<-release
		return nil
	}).Once()
	cmd := "printf zut-shell-context"
	if runtime.GOOS == "windows" {
		cmd = "echo zut-shell-context"
	}

	i.startShellEscape(context.Background(), cmd)

	select {
	case text := <-sent:
		require.Contains(t, text, "zut-shell-context")
	case <-time.After(5 * time.Second):
		t.Fatal("shell output was not sent to the daemon session")
	}
	i.mu.Lock()
	require.True(t, i.shellRunning)
	i.pendingIdleWork = append(i.pendingIdleWork, func() { close(done) })
	i.mu.Unlock()
	close(release)
	waitSignal(t, done, "shell completion")
	i.mu.Lock()
	defer i.mu.Unlock()
	require.False(t, i.shellRunning)
	require.Equal(t, "shell command finished", i.statusOK)

}

func TestShellEscapeReportsDaemonContextFailure(t *testing.T) {
	i, port := newPortShellInteractive(t)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	port.EXPECT().AppendShellContext(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, string) error {
		close(entered)
		<-release
		return errors.New(`unsupported port method "append_shell_context"; run "zut daemon restart"`)
	}).Once()
	i.startShellEscape(context.Background(), "echo hi")
	waitSignal(t, entered, "AppendShellContext")
	i.mu.Lock()
	require.True(t, i.shellRunning)
	i.pendingIdleWork = append(i.pendingIdleWork, func() { close(done) })
	i.mu.Unlock()
	close(release)
	waitSignal(t, done, "shell completion")
	i.mu.Lock()
	defer i.mu.Unlock()
	require.False(t, i.shellRunning)
	require.Contains(t, i.statusErr, "zut daemon restart")

}

func TestAppendPortShellContextKeepsTailWithinLimit(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	var got string
	port.EXPECT().AppendShellContext(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, text string) error {
		got = text
		return nil
	}).Once()
	out := strings.Repeat("x", session.MaxShellContextBytes) + "TAIL"
	require.NoError(t, appendPortShellContext(port, out))
	require.LessOrEqual(t, len(got), session.MaxShellContextBytes)
	require.True(t, strings.HasPrefix(got, "[shell output truncated]"))
	require.True(t, strings.HasSuffix(got, "TAIL"))
}

func TestShellEscapeCommand(t *testing.T) {
	cases := []struct {
		in      string
		wantCmd string
		wantOK  bool
	}{
		{"!ls -la", "ls -la", true},
		{"  !pwd", "pwd", true},
		{"!  go test ./...  ", "go test ./...", true},
		{"!", "", false},
		{"!   ", "", false},
		{"ls -la", "", false},
		{"/help", "", false},
		{"hello !world", "", false},
	}
	for _, c := range cases {
		cmd, ok := shellEscapeCommand(c.in)
		if ok != c.wantOK || cmd != c.wantCmd {
			t.Errorf("shellEscapeCommand(%q) = (%q,%v); want (%q,%v)",
				c.in, cmd, ok, c.wantCmd, c.wantOK)
		}
	}
}
