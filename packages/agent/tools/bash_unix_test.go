//go:build !windows

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
)

func TestBashTimeoutKillsDescendant(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	command := fmt.Sprintf("sleep 30 & child=$!; printf '%%s' \"$child\" > %s; wait", shellQuoteForTest(pidPath))
	tool := &BashTool{Env: []string{}, CWD: t.TempDir()}

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

func TestBashData(t *testing.T) {
	tool := &BashTool{Env: os.Environ(), CWD: t.TempDir()}
	for _, tc := range []struct {
		name, command, output string
		exit                  int
		timedOut              bool
	}{
		{"success", "printf hello", "hello", 0, false},
		{"failure", "printf error >&2; exit 7", "error", 7, false},
		{"timeout", "printf before; sleep 30", "before", -1, true},
		{"line truncation", "i=0; while [ $i -lt 2005 ]; do i=$((i+1)); printf 'x\\n'; done", strings.Repeat("x\n", maxBashLines-1) + "x", 0, false},
		{"byte truncation", "printf '%060000d' 0", strings.Repeat("0", maxBashBytes), 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": tc.command, "timeout": 1}), nil)
			if err != nil {
				t.Fatal(err)
			}
			data := res.Data.(map[string]any)
			if data["exit_code"] != tc.exit || data["output"] != tc.output || data["timed_out"] != tc.timedOut {
				t.Fatalf("unexpected data: exit=%v output length=%d timed_out=%v", data["exit_code"], len(data["output"].(string)), data["timed_out"])
			}
			text := res.Content[0].(provider.TextBlock).Text
			if !strings.Contains(text, tc.output) {
				t.Fatal("data output differs from text")
			}
			encoded, err := json.Marshal(res)
			if err != nil || strings.Contains(string(encoded), `"timed_out"`) {
				t.Fatalf("data serialized: %s %v", encoded, err)
			}
			if path := res.Details.(map[string]any)["full_output_path"].(string); path != "" {
				t.Cleanup(func() { _ = os.Remove(path) })
			}
		})
	}
}
