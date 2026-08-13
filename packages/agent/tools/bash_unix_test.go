//go:build !windows

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestBashTimeoutKillsDescendant(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	command := fmt.Sprintf("sleep 30 & child=$!; printf '%%s' \"$child\" > %s; wait", shellQuoteForTest(pidPath))
	tool := &BashTool{CWD: t.TempDir()}

	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"command": command,
		"timeout": 1,
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("timed-out command should be an error")
	}

	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	childPID, err := strconv.Atoi(string(data))
	if err != nil || childPID <= 0 {
		t.Fatalf("child pid = %q: %v", data, err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && bashProcessExists(childPID) {
		time.Sleep(10 * time.Millisecond)
	}
	if bashProcessExists(childPID) {
		t.Fatalf("bash descendant pid %d survived timeout cancellation", childPID)
	}
}

func bashProcessExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
