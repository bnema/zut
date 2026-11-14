//go:build !windows

package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestCodemodeRealPythonTimeoutProcessGone(t *testing.T) {
	dir := t.TempDir()
	tool := &CodemodeTool{CWD: dir, Env: os.Environ(), Interpreter: codemodePython(t)}
	result := runCodemodeAgent(t, tool, `import os
with open("pid", "w") as f:
    f.write(str(os.getpid()))
while True: pass`, 1, nil)
	if !result.IsError {
		t.Fatal(codemodeOutput(t, result))
	}
	raw, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("process %d still exists: %v", pid, err)
	}
}

func TestCodemodeRealPythonBridgeFilesClosed(t *testing.T) {
	// Linux exposes the parent descriptor table, allowing an exact pipe-leak
	// check without relying on global goroutine counts or runtime scheduling.
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("requires /proc/self/fd")
	}
	pipes := func() map[string]bool {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, entry := range entries {
			target, _ := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
			if strings.HasPrefix(target, "pipe:") {
				out[fmt.Sprintf("%s:%s", entry.Name(), target)] = true
			}
		}
		return out
	}
	tool := &CodemodeTool{CWD: t.TempDir(), Env: os.Environ(), Interpreter: codemodePython(t)}
	before := pipes()
	for range 3 {
		result := runCodemodeAgent(t, tool, `zut.result("bridge joined")`, 10, nil)
		if result.IsError || !strings.Contains(codemodeOutput(t, result), `result: "bridge joined"`) {
			t.Fatal(codemodeOutput(t, result))
		}
	}
	for pipe := range pipes() {
		if !before[pipe] {
			t.Errorf("leaked parent pipe: %s", pipe)
		}
	}
}
