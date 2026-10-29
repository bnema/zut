//go:build linux

package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func stubSystemd(t *testing.T) (unit string, calls *[][]string) {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	oldRun, oldExe := systemctlRun, systemdExecutable
	t.Cleanup(func() { systemctlRun, systemdExecutable = oldRun, oldExe })
	var got [][]string
	systemctlRun = func(args ...string) error {
		got = append(got, args)
		return nil
	}
	systemdExecutable = func() (string, error) { return "/opt/zut/bin/zut", nil }
	return filepath.Join(cfg, "systemd", "user", "zut-daemon.service"), &got
}

func TestDaemonInstallWritesUnit(t *testing.T) {
	unit, calls := stubSystemd(t)
	var out bytes.Buffer
	require.NoError(t, daemonInstall(&out, nil))

	b, err := os.ReadFile(unit)
	require.NoError(t, err)
	require.Equal(t, `[Unit]
Description=zut daemon

[Service]
ExecStart=/opt/zut/bin/zut daemon run
Restart=on-failure

[Install]
WantedBy=default.target
`, string(b))
	fi, err := os.Stat(unit)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), fi.Mode().Perm())
	di, err := os.Stat(filepath.Dir(unit))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), di.Mode().Perm())
	require.Contains(t, out.String(), "systemctl --user daemon-reload && systemctl --user enable --now zut-daemon")
	require.Empty(t, *calls)
}

func TestDaemonInstallNowRunsSystemctl(t *testing.T) {
	_, calls := stubSystemd(t)
	var out bytes.Buffer
	require.NoError(t, daemonInstall(&out, []string{"--now"}))
	require.Equal(t, [][]string{{"--user", "daemon-reload"}, {"--user", "enable", "--now", "zut-daemon"}}, *calls)
	require.NotContains(t, out.String(), "systemctl")
}

func TestDaemonUninstallRemovesUnit(t *testing.T) {
	unit, calls := stubSystemd(t)
	require.NoError(t, daemonInstall(&bytes.Buffer{}, nil))
	var out bytes.Buffer
	require.NoError(t, daemonUninstall(&out, nil))
	require.NoFileExists(t, unit)
	require.Contains(t, out.String(), "systemctl --user disable --now zut-daemon && systemctl --user daemon-reload")
	require.Empty(t, *calls)

	// Missing unit is fine; --now runs systemctl.
	require.NoError(t, daemonUninstall(&bytes.Buffer{}, []string{"--now"}))
	require.Equal(t, [][]string{{"--user", "disable", "--now", "zut-daemon"}, {"--user", "daemon-reload"}}, *calls)
}

func TestDaemonLogsPrintsFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	path := filepath.Join(home, "logs", "daemon.log")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("line one\nline two\n"), 0o600))

	var out bytes.Buffer
	require.NoError(t, daemonLogsCommand(&out, nil))
	require.Equal(t, "line one\nline two\n", out.String())
}

type syncBuffer struct {
	mu  chan struct{}
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu <- struct{}{}
	defer func() { <-b.mu }()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu <- struct{}{}
	defer func() { <-b.mu }()
	return b.buf.String()
}

func TestDaemonLogsFollow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	require.NoError(t, os.WriteFile(path, []byte("first\n"), 0o600))
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuffer{mu: make(chan struct{}, 1)}
	done := make(chan error, 1)
	go func() { done <- daemonLogs(ctx, out, path, true) }()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString("second\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.Eventually(t, func() bool { return strings.Contains(out.String(), "second\n") }, 5*time.Second, 20*time.Millisecond)
	cancel()
	require.NoError(t, <-done)
	require.Equal(t, "first\nsecond\n", out.String())
}
