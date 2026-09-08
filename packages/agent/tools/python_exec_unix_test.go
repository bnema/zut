//go:build !windows

package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
)

// writeFakeInterpreter creates an executable shell script acting as a fake
// Python interpreter. It ignores -u -B - arguments like a real interpreter
// would ignore unknown flags only if it is python; here the script simply
// never parses flags, which is sufficient to exercise stdin delivery,
// merged output, exit codes, and cancellation plumbing.
func writeFakeInterpreter(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakepy")
	content := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func execPythonWith(t *testing.T, exe, code string, timeout int64, progress func(string)) (string, bool, map[string]any) {
	t.Helper()
	tool := &PythonTool{
		CWD: t.TempDir(),
		resolveHook: func(context.Context, string, string) (resolvedPython, error) {
			return resolvedPython{Path: exe, Version: pythonVersion{Major: 3, Minor: 99, Micro: 0}}, nil
		},
	}
	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"code": code, "timeout": timeout}), progress)
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(provider.TextBlock).Text
	return text, res.IsError, res.Details.(map[string]any)
}

func TestPythonStdinDeliveredAndEOFObserved(t *testing.T) {
	exe := writeFakeInterpreter(t, "cat")
	text, isErr, _ := execPythonWith(t, exe, "hello stdin\nsecond line", 5, nil)
	if isErr {
		t.Fatalf("unexpected error:\n%s", text)
	}
	if !strings.Contains(text, "hello stdin") || !strings.Contains(text, "[exit 0]") {
		t.Fatalf("got:\n%s", text)
	}
}

func TestPythonMergedStderrAndNonzeroExit(t *testing.T) {
	exe := writeFakeInterpreter(t, "cat >/dev/null\necho boom >&2\nexit 3")
	text, isErr, details := execPythonWith(t, exe, "ignored", 5, nil)
	if !isErr {
		t.Fatalf("want IsError:\n%s", text)
	}
	if !strings.Contains(text, "boom") || !strings.Contains(text, "[exit 3]") {
		t.Fatalf("got:\n%s", text)
	}
	if details["exit_code"] != 3 {
		t.Fatalf("details = %#v", details)
	}
}

func TestPythonTimeoutBoundsReturn(t *testing.T) {
	exe := writeFakeInterpreter(t, "cat >/dev/null\nsleep 30")
	start := time.Now()
	text, isErr, _ := execPythonWith(t, exe, "x", 1, nil)
	elapsed := time.Since(start)
	if !isErr || !strings.Contains(text, "[timed out after 1 second]") {
		t.Fatalf("got:\n%s", text)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("timeout return took %s; cleanup did not bound the process", elapsed)
	}
}

func TestPythonDescendantCannotRetainPipe(t *testing.T) {
	// A background grandchild inherits the output pipe. Process-group kill
	// plus pipe closing must still bound the return, and the successful
	// interpreter exit must not be misreported as an execution failure.
	exe := writeFakeInterpreter(t, "(sleep 30 &)\ncat >/dev/null\necho quick")
	start := time.Now()
	// Generous timeout: the interpreter exits immediately, so WaitDelay
	// expiry (not the invocation deadline) bounds the return.
	text, isErr, _ := execPythonWith(t, exe, "x", 30, nil)
	elapsed := time.Since(start)
	if !strings.Contains(text, "quick") {
		t.Fatalf("got:\n%s", text)
	}
	if isErr {
		t.Fatalf("detached descendant must not turn exit 0 into an error:\n%s", text)
	}
	if !strings.Contains(text, "[exit 0]") {
		t.Fatalf("want successful interpreter exit, got:\n%s", text)
	}
	if elapsed > 25*time.Second {
		t.Fatalf("return took %s; descendant retained the pipe", elapsed)
	}
}

func TestPythonCancellationWhileSupplyingStdin(t *testing.T) {
	exe := writeFakeInterpreter(t, "while IFS= read -r line; do sleep 0.2; echo \"$line\"; done")
	tool := &PythonTool{
		CWD: t.TempDir(),
		resolveHook: func(context.Context, string, string) (resolvedPython, error) {
			return resolvedPython{Path: exe, Version: pythonVersion{Major: 3}}, nil
		},
	}
	big := strings.Repeat("line\n", 500)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res, err := tool.Execute(ctx, mustJSON(t, map[string]any{"code": big, "timeout": 30}), nil)
	elapsed := time.Since(start)
	if err != nil {
		// Cancellation may surface as a Go error or a cancelled result;
		// either is acceptable as long as the return is bounded.
		if elapsed > 15*time.Second {
			t.Fatalf("cancelled return took %s", elapsed)
		}
		return
	}
	text := res.Content[0].(provider.TextBlock).Text
	if !strings.Contains(text, "[cancelled]") && !res.IsError {
		t.Fatalf("want cancellation status, got:\n%s", text)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("cancelled return took %s", elapsed)
	}
}

// realPython3 returns a usable interpreter or skips the test.
func realPython3(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 on PATH; resolver coverage does not require installed Python")
	}
	ver, err := probePythonVersion(context.Background(), path)
	if err != nil || ver.Major != 3 {
		t.Skipf("python3 probe failed (%v); skipping runtime checks", err)
	}
	return path
}

func TestPythonRealRunsInSessionCWD(t *testing.T) {
	py := realPython3(t)
	dir := t.TempDir()
	tool := &PythonTool{CWD: dir, Interpreter: py}
	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"code":    "import os; print(os.getcwd())",
		"timeout": 15,
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(provider.TextBlock).Text
	if res.IsError || !strings.Contains(text, dir) {
		t.Fatalf("got:\n%s", text)
	}
	// -B plus stdin execution must not create bytecode caches.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() == "__pycache__" {
			t.Fatal("execution created __pycache__; -B / stdin execution must not write bytecode")
		}
	}
}

func TestPythonRealImportsAndMissingImport(t *testing.T) {
	py := realPython3(t)
	tool := &PythonTool{CWD: t.TempDir(), Interpreter: py}
	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"code":    "import json; print('imports ok')",
		"timeout": 15,
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(res.Content[0].(provider.TextBlock).Text, "imports ok") {
		t.Fatalf("installed import failed:\n%s", res.Content[0].(provider.TextBlock).Text)
	}
	res, err = tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"code":    "import no_such_module_xyz_zut",
		"timeout": 15,
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(provider.TextBlock).Text
	if !res.IsError || (!strings.Contains(text, "ModuleNotFoundError") && !strings.Contains(text, "ImportError")) {
		t.Fatalf("missing import must fail with an ordinary traceback, got:\n%s", text)
	}
}

func TestPythonRealNoStateBetweenCalls(t *testing.T) {
	py := realPython3(t)
	tool := &PythonTool{CWD: t.TempDir(), Interpreter: py}
	if _, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"code": "x_state_probe = 1", "timeout": 15}), nil); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"code": "print(x_state_probe)", "timeout": 15}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content[0].(provider.TextBlock).Text, "NameError") {
		t.Fatalf("successive calls must not share variables, got:\n%s", res.Content[0].(provider.TextBlock).Text)
	}
}

func TestPythonRealWrittenFilesPersist(t *testing.T) {
	py := realPython3(t)
	dir := t.TempDir()
	tool := &PythonTool{CWD: dir, Interpreter: py}
	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"code":    "open('created.txt','w').write('hi')",
		"timeout": 15,
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("got:\n%s", res.Content[0].(provider.TextBlock).Text)
	}
	b, err := os.ReadFile(filepath.Join(dir, "created.txt"))
	if err != nil || string(b) != "hi" {
		t.Fatalf("deliberately written file missing: %q %v", b, err)
	}
}
