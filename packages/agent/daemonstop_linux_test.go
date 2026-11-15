//go:build linux

package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/internal/flock"
	"github.com/stretchr/testify/require"
)

func TestDaemonStopSignalHelper(t *testing.T) {
	if os.Getenv("ZUT_STOP_HELPER") != "1" {
		return
	}
	home := os.Getenv("ZUT_STOP_HOME")
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()
	var l *flock.Lock
	if os.Getenv("ZUT_STOP_NO_LOCK") != "1" {
		var err error
		l, err = flock.TryLock(filepath.Join(home, "daemon", "daemon.lock"))
		require.NoError(t, err)
	}
	require.NoError(t, os.WriteFile(filepath.Join(home, "daemon", "daemon.pid"), []byte(strconv.Itoa(os.Getpid())), 0600))
	fmt.Println("ready")
	<-ctx.Done()
	if l != nil {
		require.NoError(t, l.Close())
	}
}

func TestStopIncompatibleDaemon(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, "daemon"), 0700))
	t.Setenv("ZUT_STOP_HELPER", "1")
	t.Setenv("ZUT_STOP_HOME", home)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDaemonStopSignalHelper$")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	ready := make([]byte, len("ready\n"))
	_, err = io.ReadFull(stdout, ready)
	require.NoError(t, err)
	require.Equal(t, "ready\n", string(ready))
	var out bytes.Buffer
	stopped, err := stopIncompatibleDaemon(ctx, home, &out)
	require.NoError(t, err)
	require.True(t, stopped)
	require.Contains(t, out.String(), fmt.Sprintf("stopped daemon pid %d with SIGTERM", cmd.Process.Pid))
	require.NoError(t, cmd.Wait())
}

func TestStopIncompatibleDaemonStalePID(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, "daemon"), 0700))
	// Signaling this PID would kill the test: a free lock must prevent it.
	require.NoError(t, os.WriteFile(filepath.Join(home, "daemon", "daemon.pid"), []byte(strconv.Itoa(os.Getpid())), 0600))
	var out bytes.Buffer
	stopped, err := stopIncompatibleDaemon(context.Background(), home, &out)
	require.NoError(t, err)
	require.False(t, stopped)
	require.Empty(t, out.String())
}

func TestStopIncompatibleDaemonWrongLockOwner(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, "daemon"), 0700))
	l, err := flock.TryLock(filepath.Join(home, "daemon", "daemon.lock"))
	require.NoError(t, err)
	defer l.Close()
	t.Setenv("ZUT_STOP_HELPER", "1")
	t.Setenv("ZUT_STOP_HOME", home)
	t.Setenv("ZUT_STOP_NO_LOCK", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDaemonStopSignalHelper$")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	ready := make([]byte, len("ready\n"))
	_, err = io.ReadFull(stdout, ready)
	require.NoError(t, err)
	var out bytes.Buffer
	stopped, err := stopIncompatibleDaemon(ctx, home, &out)
	require.False(t, stopped)
	require.EqualError(t, err, unverifiedDaemonPID(cmd.Process.Pid).Error())
	require.NoError(t, cmd.Process.Signal(syscall.Signal(0)), "unverified process must still be alive")
	require.Empty(t, out.String())
}

func TestDaemonSignalFailureExited(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, "daemon"), 0700))
	for _, exitErr := range []error{os.ErrProcessDone, syscall.ESRCH} {
		stopped, err := daemonSignalFailure(home, exitErr)
		require.True(t, stopped)
		require.NoError(t, err)
		l, err := flock.TryLock(filepath.Join(home, "daemon", "daemon.lock"))
		require.NoError(t, err)
		stopped, err = daemonSignalFailure(home, exitErr)
		require.False(t, stopped)
		require.ErrorIs(t, err, exitErr)
		require.NoError(t, l.Close())
	}
}

func TestSignalDaemonPIDAlreadyExited(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	require.NoError(t, cmd.Run())
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, "daemon"), 0700))
	_, err := signalDaemonPID(cmd.Process.Pid, filepath.Join(home, "daemon", "daemon.lock"))
	require.Error(t, err)
	stopped, err := daemonSignalFailure(home, err)
	require.True(t, stopped)
	require.NoError(t, err)
}
