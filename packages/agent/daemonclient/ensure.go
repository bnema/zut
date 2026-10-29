package daemonclient

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/internal/detach"
)

// Spawn polling bounds (D4).
const (
	spawnDeadline = 5 * time.Second
	pollMin       = 50 * time.Millisecond
	pollMax       = 400 * time.Millisecond
	logTailBytes  = 4 << 10
)

// EnsureOptions configures EnsureDaemon.
type EnsureOptions struct {
	Options
	// SocketPath overrides SocketPath(Getenv).
	SocketPath string
	// Getenv resolves ZUT_DAEMON_SOCKET, XDG_RUNTIME_DIR, LOCALAPPDATA and
	// ZUT_HOME (default os.Getenv).
	Getenv func(string) string
	// Env is the spawned daemon's environment (default os.Environ()).
	Env []string
	// Executable is the zut binary to spawn (default os.Executable()).
	Executable string
	// Args replaces the default ["daemon", "run"] child arguments.
	Args []string
	// Home overrides ZutHome(Getenv) for $ZUT_HOME/logs/daemon.log.
	Home string
}

// LogPath returns the daemon log path under home.
func LogPath(home string) string { return filepath.Join(home, "logs", "daemon.log") }

// EnsureDaemon dials the daemon, spawning `<exe> daemon run` detached when no
// daemon listens (socket missing or connection refused). The child's
// stdout/stderr go to $ZUT_HOME/logs/daemon.log. Dialing is retried with
// 50→400 ms backoff for up to 5 s, even if the child exits early (another
// daemon may win the start race). On success the child is released; on
// timeout it is killed and its exit status and the log tail are reported.
func EnsureDaemon(ctx context.Context, opts EnsureOptions) (*Client, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	path := opts.SocketPath
	if path == "" {
		path = SocketPath(getenv)
	}
	c, err := Dial(ctx, path, opts.Options)
	if err == nil || !absent(err) {
		return c, err
	}
	home := opts.Home
	if home == "" {
		home = ZutHome(getenv)
	}
	logPath := LogPath(home)
	cmd, err := spawn(opts, logPath)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, spawnDeadline)
	defer cancel()
	delay := pollMin
	for {
		// The child may exit early because another daemon won the start
		// race; keep polling until the deadline either way.
		c, err = Dial(ctx, path, opts.Options)
		if err == nil {
			_ = cmd.Process.Release()
			return c, nil
		}
		if !absent(err) && ctx.Err() == nil {
			_ = cmd.Process.Release()
			return nil, err
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, spawnError(fmt.Errorf("zut daemon did not start within %s (%s): %w", spawnDeadline, reap(cmd), err), logPath)
		case <-t.C:
		}
		delay = min(delay*2, pollMax)
	}
}

// reap stops a child that never became reachable and reports its status.
// Wait runs only here, inside the startup window, after Kill guarantees exit.
func reap(cmd *exec.Cmd) string {
	_ = cmd.Process.Kill()
	if err := cmd.Wait(); err != nil {
		return "child " + err.Error()
	}
	return "child " + cmd.ProcessState.String()
}

func spawn(opts EnsureOptions, logPath string) (*exec.Cmd, error) {
	exe := opts.Executable
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return nil, fmt.Errorf("locate zut executable: %w", err)
		}
	}
	args := opts.Args
	if args == nil {
		args = []string{"daemon", "run"}
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, fmt.Errorf("create daemon log dir: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open daemon log: %w", err)
	}
	defer logFile.Close()
	cmd := exec.Command(exe, args...)
	cmd.Env = opts.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, logFile, logFile
	detach.Configure(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn zut daemon: %w", err)
	}
	return cmd, nil
}

// spawnError appends the last lines of the daemon log to err.
func spawnError(err error, logPath string) error {
	tail := logTail(logPath)
	if tail == "" {
		return fmt.Errorf("%w (log %s is empty)", err, logPath)
	}
	return fmt.Errorf("%w; tail of %s:\n%s", err, logPath, tail)
}

func logTail(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > logTailBytes {
		_, _ = f.Seek(-logTailBytes, io.SeekEnd)
	}
	b, _ := io.ReadAll(io.LimitReader(f, logTailBytes))
	if i := bytes.IndexByte(b, '\n'); i >= 0 && len(b) == logTailBytes {
		b = b[i+1:]
	}
	return strings.TrimRight(string(b), "\n")
}
