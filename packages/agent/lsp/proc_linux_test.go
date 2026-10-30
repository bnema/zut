//go:build linux

package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCloseKillsServerChildren(t *testing.T) {
	for _, cooperative := range []bool{true, false} {
		t.Run(strconv.FormatBool(cooperative), func(t *testing.T) {
			root := t.TempDir()
			pidFile := filepath.Join(root, "child.pid")
			// The child inherits the server's process group. Neither process needs
			// stdout for synchronization; the pid file is written after spawning.
			script := "sleep 60 &\necho $! > \"$1\"\n"
			if cooperative {
				script += "cat >/dev/null\nexit 0\n"
			} else {
				script += "wait\n"
			}
			client, err := NewClient(ServerConfig{ID: "children", Command: "/bin/sh", Args: []string{"-c", script, "server", pidFile}}, root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			var pid int
			pollProcess(t, func() bool {
				raw, err := os.ReadFile(pidFile)
				if err != nil {
					return false
				}
				pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
				return err == nil && pid > 0
			})
			// Ensure a regression failure does not leave the helper running.
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			if err := syscall.Kill(pid, 0); err != nil {
				t.Fatalf("child not running before Close: %v", err)
			}
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
			pollProcess(t, func() bool {
				raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
				if os.IsNotExist(err) {
					return true
				}
				if err != nil {
					return false
				}
				state, _, ok := parseProcStat(string(raw))
				// An orphan zombie may await the host's reaper, but is no longer running.
				return ok && (state == 'Z' || state == 'X')
			})
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func pollProcess(t *testing.T, ready func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ready() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for process state")
		case <-ticker.C:
		}
	}
}
