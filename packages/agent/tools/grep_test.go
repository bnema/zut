package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestGrepPrefersRGAndFallsBackToGrep(t *testing.T) {
	skipGrepScriptTestsOnWindows(t)
	root := t.TempDir()
	rg := writeGrepScript(t, "#!/bin/sh\nprintf 'rg selected\\n'\n")
	grep := writeGrepScript(t, "#!/bin/sh\nprintf 'grep selected\\n'\n")

	t.Run("rg", func(t *testing.T) {
		tool := &GrepTool{CWD: root, LookPath: func(name string) (string, error) {
			switch name {
			case "rg":
				return rg, nil
			case "grep":
				return grep, nil
			default:
				return "", os.ErrNotExist
			}
		}}
		result := executeGrepTest(t, tool, root)
		if got := grepText(result); got != "rg selected\n" {
			t.Fatalf("output = %q, want rg output", got)
		}
		if grepDetails(result)["engine"] != "rg" {
			t.Fatalf("engine = %v, want rg", grepDetails(result)["engine"])
		}
	})

	t.Run("grep fallback", func(t *testing.T) {
		tool := &GrepTool{CWD: root, LookPath: func(name string) (string, error) {
			if name == "rg" {
				return "", os.ErrNotExist
			}
			return grep, nil
		}}
		result := executeGrepTest(t, tool, root)
		if got := grepText(result); got != "grep selected\n" {
			t.Fatalf("output = %q, want grep output", got)
		}
		if grepDetails(result)["engine"] != "grep" {
			t.Fatalf("engine = %v, want grep", grepDetails(result)["engine"])
		}
	})
}

func TestGrepChecksSandboxReadScopeBeforeRunning(t *testing.T) {
	skipGrepScriptTestsOnWindows(t)
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	sandbox := NewSandbox(root)
	sandbox.Lock()
	called := false
	tool := &GrepTool{
		CWD:     root,
		Sandbox: sandbox,
		LookPath: func(string) (string, error) {
			called = true
			return "", errors.New("lookup should not run")
		},
	}
	_, err := tool.Execute(context.Background(), grepArgsJSON("needle", outside), nil)
	if err == nil || !strings.Contains(err.Error(), "jailed") {
		t.Fatalf("error = %v, want sandbox denial", err)
	}
	if called {
		t.Fatal("executable lookup ran before the sandbox check")
	}
}

func TestGrepNoMatchIsSuccessful(t *testing.T) {
	skipGrepScriptTestsOnWindows(t)
	root := t.TempDir()
	noMatch := writeGrepScript(t, "#!/bin/sh\nexit 1\n")
	tool := &GrepTool{CWD: root, LookPath: lookupScripts(map[string]string{"rg": noMatch})}
	result := executeGrepTest(t, tool, root)
	if result.IsError {
		t.Fatal("no-match result is an error")
	}
	if got := grepDetails(result)["exit_code"]; got != 0 {
		t.Fatalf("reported exit code = %v, want 0", got)
	}
	if got := grepDetails(result)["process_exit_code"]; got != 1 {
		t.Fatalf("process exit code = %v, want 1", got)
	}
	if got := grepText(result); got != "No matches found.\n" {
		t.Fatalf("output = %q, want no-match message", got)
	}
}

func TestGrepBoundsOutput(t *testing.T) {
	skipGrepScriptTestsOnWindows(t)
	root := t.TempDir()
	script := writeGrepScript(t, "#!/bin/sh\ni=0\nwhile [ $i -lt 4000 ]; do\n  printf 'matching line %04d with enough padding to hit the cap\\n' $i\n  i=$((i + 1))\ndone\n")
	tool := &GrepTool{CWD: root, LookPath: lookupScripts(map[string]string{"rg": script})}
	result := executeGrepTest(t, tool, root)
	text := grepText(result)
	if !grepDetails(result)["bytes_truncated"].(bool) {
		t.Fatal("bytes_truncated = false, want true")
	}
	if !strings.Contains(text, "truncated at") {
		t.Fatalf("bounded output has no truncation marker: %q", text[len(text)-100:])
	}
	if len(text) > maxGrepOutputBytes {
		t.Fatalf("bounded output length = %d, cap exceeded", len(text))
	}
}

func TestGrepExactlyMaxOutputLinesIsNotTruncated(t *testing.T) {
	input := strings.Repeat("matching line\n", maxGrepOutputLines)
	output := &grepOutput{}
	if _, err := output.Write([]byte(input)); err != nil {
		t.Fatal(err)
	}
	got, truncated := output.text()
	if truncated {
		t.Fatal("exactly maxGrepOutputLines was marked truncated")
	}
	if got != input {
		t.Fatalf("output was altered: got %d bytes, want %d", len(got), len(input))
	}
}

func TestGrepCancellationPropagates(t *testing.T) {
	skipGrepScriptTestsOnWindows(t)
	root := t.TempDir()
	started := filepath.Join(root, "started")
	script := writeGrepScript(t, "#!/bin/sh\nprintf started > started\nsleep 10\n")
	tool := &GrepTool{CWD: root, LookPath: lookupScripts(map[string]string{"rg": script})}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	resultCh := make(chan error, 1)
	go func() {
		_, err := tool.Execute(ctx, grepArgsJSON("needle", root), nil)
		resultCh <- err
	}()

	startupDeadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(startupDeadline) {
			t.Fatal("grep fixture did not start before the test deadline")
		}
		time.Sleep(5 * time.Millisecond)
	}

	select {
	case err := <-resultCh:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("grep did not stop after context cancellation")
	}
}

func TestGrepInternalTimeoutStopsSearch(t *testing.T) {
	skipGrepScriptTestsOnWindows(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hold"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	script := writeGrepScript(t, "#!/bin/sh\nprintf 'started\\n'\nexec tail -f hold\n")
	tool := &GrepTool{CWD: root, LookPath: lookupScripts(map[string]string{"rg": script})}

	resultCh := make(chan error, 1)
	go func() {
		_, err := tool.executeWithTimeout(context.Background(), grepArgsJSON("needle", root), nil, 100*time.Millisecond)
		resultCh <- err
	}()

	err := waitForGrepResult(t, resultCh)
	var timeoutErr *grepTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("error = %v, want internal grep timeout", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("internal timeout error = %v, want distinct caller deadline error", err)
	}
	if !strings.Contains(err.Error(), "try narrowing the path or glob") {
		t.Fatalf("error = %v, want path/glob narrowing guidance", err)
	}
}

func TestGrepCallerDeadlineTakesPrecedenceOverInternalTimeout(t *testing.T) {
	skipGrepScriptTestsOnWindows(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hold"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	script := writeGrepScript(t, "#!/bin/sh\nprintf 'started\\n'\nexec tail -f hold\n")
	tool := &GrepTool{CWD: root, LookPath: lookupScripts(map[string]string{"rg": script})}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	resultCh := make(chan error, 1)
	go func() {
		_, err := tool.executeWithTimeout(ctx, grepArgsJSON("needle", root), nil, time.Second)
		resultCh <- err
	}()

	err := waitForGrepResult(t, resultCh)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want caller deadline exceeded", err)
	}
	var timeoutErr *grepTimeoutError
	if errors.As(err, &timeoutErr) {
		t.Fatalf("error = %v, want caller deadline rather than internal timeout", err)
	}
}

func waitForGrepResult(t *testing.T, resultCh <-chan error) error {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case err := <-resultCh:
		return err
	case <-timer.C:
		t.Fatal("grep fixture did not stop before the test deadline")
		return nil
	}
}

func TestGrepDoesNotRequireBashPermission(t *testing.T) {
	skipGrepScriptTestsOnWindows(t)
	root := t.TempDir()
	script := writeGrepScript(t, "#!/bin/sh\nprintf 'read-only search\\n'\n")
	sandbox := NewSandbox(root)
	sandbox.Lock()
	permissions := &PermissionSet{}
	permissions.FS.Read = []string{root}
	permissions.Bash.Mode = "none"
	sandbox.SetPermissions(permissions)
	tool := &GrepTool{CWD: root, Sandbox: sandbox, LookPath: lookupScripts(map[string]string{"rg": script})}
	result := executeGrepTest(t, tool, root)
	if got := grepText(result); got != "read-only search\n" {
		t.Fatalf("output = %q", got)
	}
}

func TestGrepSchemaValidation(t *testing.T) {
	tool := &GrepTool{LookPath: func(string) (string, error) {
		t.Fatal("schema-invalid input reached executable lookup")
		return "", nil
	}}
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"not object", `[]`},
		{"missing pattern", `{"path":"."}`},
		{"missing path", `{"pattern":"needle"}`},
		{"empty pattern", `{"pattern":"","path":"."}`},
		{"wrong type", `{"pattern":true,"path":"."}`},
		{"unknown field", `{"pattern":"needle","path":".","shell":"echo"}`},
		{"nul", "{\"pattern\":\"needle\\u0000\",\"path\":\".\"}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tool.Execute(context.Background(), []byte(tc.raw), nil); err == nil {
				t.Fatal("invalid arguments were accepted")
			}
		})
	}

	var schema map[string]any
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v", schema["type"])
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("schema additionalProperties = %v", schema["additionalProperties"])
	}
}

func executeGrepTest(t *testing.T, tool *GrepTool, root string) core.ToolResult {
	t.Helper()
	result, err := tool.Execute(context.Background(), grepArgsJSON("needle", root), nil)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func grepDetails(result core.ToolResult) map[string]any {
	return result.Details.(map[string]any)
}

func grepText(result core.ToolResult) string {
	if len(result.Content) == 0 {
		return ""
	}
	block, ok := result.Content[0].(provider.TextBlock)
	if !ok {
		return ""
	}
	return block.Text
}

func grepArgsJSON(pattern, path string) []byte {
	return []byte(`{"pattern":` + strconvQuote(pattern) + `,"path":` + strconvQuote(path) + `}`)
}

func strconvQuote(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}

func lookupScripts(paths map[string]string) func(string) (string, error) {
	return func(name string) (string, error) {
		if path, ok := paths[name]; ok {
			return path, nil
		}
		return "", os.ErrNotExist
	}
}

func writeGrepScript(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-grep")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func skipGrepScriptTestsOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX executable fixture")
	}
}
