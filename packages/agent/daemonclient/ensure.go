package daemonclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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
// 50→400 ms backoff for up to 5 s; failure reports the log tail.
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
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	ctx, cancel := context.WithTimeout(ctx, spawnDeadline)
	defer cancel()
	delay := pollMin
	for {
		c, err = Dial(ctx, path, opts.Options)
		if err == nil || !absent(err) && ctx.Err() == nil {
			return c, err
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, spawnError(fmt.Errorf("zut daemon did not start within %s: %w", spawnDeadline, err), logPath)
		case <-exited:
			t.Stop()
			if c, err = Dial(context.WithoutCancel(ctx), path, opts.Options); err == nil {
				// The child exited because another daemon won the start race.
				return c, nil
			}
			return nil, spawnError(fmt.Errorf("zut daemon exited during startup: %s", cmd.ProcessState), logPath)
		case <-t.C:
		}
		delay = min(delay*2, pollMax)
	}
}

// absent reports dial errors meaning nothing listens on the socket.
func absent(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
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
