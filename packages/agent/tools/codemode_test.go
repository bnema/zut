package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// A scripted provider drives the public Agent API, obtaining a real, scoped
// NestedInvoker without exposing a test-only context setter from core.
type codemodeScriptClient struct {
	args json.RawMessage
	sent bool
}

func (*codemodeScriptClient) Name() string { return "codemode-test" }
func (c *codemodeScriptClient) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	events := make(chan provider.Event, 1)
	msg := provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "done"}}}
	stop := provider.StopEnd
	if !c.sent {
		c.sent = true
		stop = provider.StopToolUse
		msg.Content = []provider.Content{provider.ToolCallBlock{ID: "cm", Name: "codemode", Arguments: c.args}}
	}
	events <- provider.EventDone{Stop: stop, Message: msg}
	close(events)
	return events, nil
}

func codemodePython(t *testing.T) string {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 absent")
	}
	return py
}
func runCodemodeAgent(t *testing.T, tool *CodemodeTool, code string, timeout int, guard func(provider.ToolCallBlock) (bool, string, json.RawMessage), extra ...core.Tool) core.ToolResult {
	t.Helper()
	args, err := json.Marshal(map[string]any{"code": code, "timeout": timeout})
	if err != nil {
		t.Fatal(err)
	}
	reg := core.NewRegistry(tool, &GlobTool{CWD: tool.CWD})
	for _, child := range extra {
		reg[child.Name()] = child
	}
	agent := core.NewAgent(&codemodeScriptClient{args: args}, "test", "", reg)
	agent.BeforeToolExecute = guard
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var result core.ToolResult
	found := false
	if err := agent.Prompt(ctx, "run", nil, func(ev core.AgentEvent) {
		if e, ok := ev.(core.EvToolResult); ok && e.ID == "cm" {
			result = e.Result
			found = true
		}
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("no codemode result")
	}
	return result
}
func codemodeOutput(t *testing.T, r core.ToolResult) string {
	t.Helper()
	if len(r.Content) == 0 {
		t.Fatal("empty result")
	}
	return r.Content[0].(provider.TextBlock).Text
}

func TestCodemodeRealPythonAPI(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "one.go"), []byte("package one"), 0600); err != nil {
		t.Fatal(err)
	}
	tool := &CodemodeTool{CWD: dir, Env: os.Environ(), Interpreter: codemodePython(t)}
	code := `import zut, os
assert "ZUT_CODEMODE_FDS" not in os.environ
if os.name != "nt":
    assert not os.get_inheritable(3) and not os.get_inheritable(4)
assert zut.describe("glob")["name"] == "glob"
assert any(t["name"] == "glob" for t in zut.search("glob"))
r = zut.tools.glob(pattern="*.go")
assert r.id == "cm/1" and not r.is_error
print("found", r.text)
zut.result(len(r.data))`
	result := runCodemodeAgent(t, tool, code, 10, nil)
	text := codemodeOutput(t, result)
	if result.IsError || !strings.Contains(text, "result: 1") || !strings.Contains(text, "nested calls: 1 (errors: 0, denied: 0)") {
		t.Fatal(text)
	}
	if len(result.Context.Discovers) == 0 {
		t.Fatal("missing nested context")
	}
}
func TestCodemodeRealPythonDenial(t *testing.T) {
	tool := &CodemodeTool{CWD: t.TempDir(), Env: os.Environ(), Interpreter: codemodePython(t)}
	result := runCodemodeAgent(t, tool, `try:
    zut.tools.glob(pattern="*")
except zut.ToolDenied as e:
    print("caught denied:", e)
try:
    zut.call("plan")
except zut.CodemodeError as e:
    print("caught", e.kind)`, 10, func(call provider.ToolCallBlock) (bool, string, json.RawMessage) {
		return call.Name != "glob", "confirmation refused", nil
	})
	text := codemodeOutput(t, result)
	if result.IsError || !strings.Contains(text, "caught denied: confirmation refused") || !strings.Contains(text, "caught not_allowed") || !strings.Contains(text, "denied: 1") {
		t.Fatal(text)
	}
}
func TestCodemodeRealPythonToolErrorAndTraceback(t *testing.T) {
	tool := &CodemodeTool{CWD: t.TempDir(), Env: os.Environ(), Interpreter: codemodePython(t)}
	// A malformed glob is a normal IsError result, not a bridge error.
	result := runCodemodeAgent(t, tool, `try:
    zut.call("glob", pattern="[")
except zut.ToolError as e:
    print("caught tool error", e.result.is_error)
r = zut.call("glob", check=False, pattern="[")
assert r.is_error
raise ValueError("user failure")`, 10, nil)
	text := codemodeOutput(t, result)
	if !result.IsError || !strings.Contains(text, "caught tool error True") || !strings.Contains(text, `File "<codemode>"`) || !strings.Contains(text, "ValueError: user failure") || strings.Contains(text, `File "<stdin>"`) {
		t.Fatal(text)
	}
}
func TestCodemodeRealPythonTimeout(t *testing.T) {
	tool := &CodemodeTool{CWD: t.TempDir(), Env: os.Environ(), Interpreter: codemodePython(t)}
	start := time.Now()
	result := runCodemodeAgent(t, tool, `while True: pass`, 1, nil)
	text := codemodeOutput(t, result)
	if !result.IsError || !strings.Contains(text, "[timed out after 1 second]") || time.Since(start) > 10*time.Second {
		t.Fatal(text)
	}
	// Execute joins bridgeDone before returning, including timeout paths.
	if !strings.Contains(text, "nested calls: 0") {
		t.Fatal(text)
	}
}
func TestCodemodeAdmission(t *testing.T) {
	sandbox := NewSandbox(t.TempDir())
	sandbox.Lock()
	tool := &CodemodeTool{CWD: sandbox.Root, Env: []string{}, Sandbox: sandbox, Interpreter: "must-not-spawn"}
	args := json.RawMessage(`{"code":"print(1)","timeout":1}`)
	if _, err := tool.Execute(context.Background(), args, nil); err == nil || !strings.Contains(err.Error(), "Python cannot be confined") {
		t.Fatalf("jailed: %v", err)
	}
	sandbox.Unlock()
	result, err := tool.Execute(context.Background(), args, nil)
	if err != nil || !result.IsError || !strings.Contains(codemodeOutput(t, result), "nested tool calls are unavailable") {
		t.Fatalf("missing invoker: %+v %v", result, err)
	}
	for _, raw := range []string{`{}`, `{"code":" ","timeout":1}`, `{"code":"pass"}`, `{"code":"pass","timeout":0}`} {
		result, err := tool.Execute(context.Background(), json.RawMessage(raw), nil)
		if err != nil || !result.IsError {
			t.Fatalf("validation %s: %+v %v", raw, result, err)
		}
	}
}

type codemodeBlockingTool struct {
	started, cancelled chan struct{}
	ready              net.Conn
}

func (*codemodeBlockingTool) Name() string            { return "blocking" }
func (*codemodeBlockingTool) Description() string     { return "blocks until cancelled" }
func (*codemodeBlockingTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (b *codemodeBlockingTool) Execute(ctx context.Context, _ json.RawMessage, _ func(string)) (core.ToolResult, error) {
	close(b.started)
	if _, err := b.ready.Write([]byte("!")); err != nil {
		return core.ToolResult{}, err
	}
	<-ctx.Done()
	close(b.cancelled)
	return core.ToolResult{}, ctx.Err()
}

func TestCodemodeRealPythonExitCancelsNested(t *testing.T) {
	dir := t.TempDir()
	tool := &CodemodeTool{CWD: dir, Env: os.Environ(), Interpreter: codemodePython(t)}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// Accept and pass one readiness byte through a blocking socket handshake;
	// no filesystem polling or sleep is needed to order child exit after Invoke.
	accepted := make(chan net.Conn, 1)
	go func() { conn, _ := listener.Accept(); accepted <- conn }()
	child := &codemodeBlockingTool{started: make(chan struct{}), cancelled: make(chan struct{})}
	guard := func(call provider.ToolCallBlock) (bool, string, json.RawMessage) {
		if call.Name == "blocking" {
			child.ready = <-accepted
		}
		return true, "", nil
	}
	code := fmt.Sprintf(`import threading, os, socket
ready = socket.create_connection(("127.0.0.1", %d))
threading.Thread(target=lambda: zut.call("blocking"), daemon=True).start()
assert ready.recv(1) == b"!"
os._exit(0)`, listener.Addr().(*net.TCPAddr).Port)
	result := runCodemodeAgent(t, tool, code, 60, guard, child)
	if child.ready != nil {
		defer child.ready.Close()
	}
	if result.IsError {
		t.Fatal(codemodeOutput(t, result))
	}
	select {
	case <-child.cancelled:
	default:
		t.Fatal("nested invocation was not cancelled before Execute returned")
	}
}

func TestCodemodeRealPythonOversizedRequest(t *testing.T) {
	tool := &CodemodeTool{CWD: t.TempDir(), Env: os.Environ(), Interpreter: codemodePython(t)}
	// The agent helper has a 20s deadline, shorter than the requested 60s
	// timeout. This must finish locally, not be rescued by the tool timeout.
	result := runCodemodeAgent(t, tool, `try:
    zut.result("x" * (9 * 1024 * 1024))
except zut.CodemodeError as e:
    print("oversized:", e.kind)
zut.result("still connected")`, 60, nil)
	text := codemodeOutput(t, result)
	if result.IsError || !strings.Contains(text, "oversized: limit") || !strings.Contains(text, `result: "still connected"`) {
		t.Fatal(text)
	}
}

func TestCodemodeRealPythonNameArgument(t *testing.T) {
	tool := &CodemodeTool{CWD: t.TempDir(), Env: os.Environ(), Interpreter: codemodePython(t)}
	calls := 0
	guard := func(call provider.ToolCallBlock) (bool, string, json.RawMessage) {
		if call.Name == "glob" {
			calls++
			var args map[string]any
			if err := json.Unmarshal(call.Arguments, &args); err != nil {
				t.Error(err)
			}
			if args["name"] != "forwarded" {
				t.Errorf("name not forwarded: %s", call.Arguments)
			}
		}
		return true, "", nil
	}
	result := runCodemodeAgent(t, tool, `zut.call("glob", check=False, name="forwarded", pattern="*")
zut.tools.glob(check=False, name="forwarded", pattern="*")`, 10, guard)
	if result.IsError || calls != 2 {
		t.Fatal(codemodeOutput(t, result), calls)
	}
}

func TestCodemodeRealPythonSystemExit(t *testing.T) {
	for _, tc := range []struct {
		code    string
		isError bool
		exit    int
	}{
		{"import sys; sys.exit(0)", false, 0},
		{"import sys; sys.exit(3)", true, 3},
		{"raise KeyboardInterrupt()", true, 130},
	} {
		t.Run(tc.code, func(t *testing.T) {
			tool := &CodemodeTool{CWD: t.TempDir(), Env: os.Environ(), Interpreter: codemodePython(t)}
			result := runCodemodeAgent(t, tool, tc.code, 10, nil)
			if result.IsError != tc.isError {
				t.Fatal(codemodeOutput(t, result))
			}
			// KeyboardInterrupt is represented differently across platforms.
			if !strings.Contains(tc.code, "KeyboardInterrupt") && result.Details.(map[string]any)["exit_code"] != tc.exit {
				t.Fatal(result.Details)
			}
		})
	}
}

func TestCodemodeRealPythonImportedTraceback(t *testing.T) {
	dir := t.TempDir()
	module := `def fail():
    try:
        raise ValueError("original")
    except ValueError as error:
        raise RuntimeError("imported failure") from error
`
	if err := os.WriteFile(filepath.Join(dir, "broken_module.py"), []byte(module), 0600); err != nil {
		t.Fatal(err)
	}
	tool := &CodemodeTool{CWD: dir, Env: os.Environ(), Interpreter: codemodePython(t)}
	result := runCodemodeAgent(t, tool, `import broken_module; broken_module.fail()`, 10, nil)
	text := codemodeOutput(t, result)
	for _, want := range []string{"broken_module.py", `File "<codemode>"`, "ValueError: original", "direct cause", "RuntimeError: imported failure"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
	if !result.IsError || strings.Contains(text, `File "<stdin>"`) {
		t.Fatal(text)
	}
}

type codemodeEchoDataTool struct{}

func (*codemodeEchoDataTool) Name() string            { return "echo_data" }
func (*codemodeEchoDataTool) Description() string     { return "Return the supplied JSON unchanged" }
func (*codemodeEchoDataTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (*codemodeEchoDataTool) Execute(_ context.Context, args json.RawMessage, _ func(string)) (core.ToolResult, error) {
	return core.ToolResult{Data: append(json.RawMessage(nil), args...)}, nil
}

func TestCodemodeRealPythonConcurrentRPC(t *testing.T) {
	tool := &CodemodeTool{CWD: t.TempDir(), Env: os.Environ(), Interpreter: codemodePython(t)}
	result := runCodemodeAgent(t, tool, `import threading
barrier = threading.Barrier(8)
failures = []
def worker(index):
    try:
        barrier.wait()
        for sequence in range(20):
            value = {"index": index, "sequence": sequence, "integer": 9007199254740993}
            r = zut.call("echo_data", **value)
            assert r.data == value, (r.data, value)
    except BaseException as error:
        failures.append(str(error))
threads = [threading.Thread(target=worker, args=(i,)) for i in range(8)]
for thread in threads:
    thread.start()
for thread in threads:
    thread.join()
assert not failures, failures
zut.result("all thread results matched")`, 10, nil, &codemodeEchoDataTool{})
	text := codemodeOutput(t, result)
	if result.IsError || !strings.Contains(text, `result: "all thread results matched"`) || !strings.Contains(text, "nested calls: 160 (errors: 0, denied: 0)") {
		t.Fatal(text)
	}
}

func TestCodemodeRealPythonErgonomics(t *testing.T) {
	tool := &CodemodeTool{CWD: t.TempDir(), Env: os.Environ(), Interpreter: codemodePython(t)}
	result := runCodemodeAgent(t, tool, `names = dir(zut.tools)
assert names == sorted(names) and "glob" in names and "codemode" not in names
s = zut.search("", limit=1)
assert isinstance(s, list) and isinstance(s, zut.SearchResults)
assert len(s) == 1 and s.total == 2 and s.truncated
s = zut.search("missing")
assert s == [] and s.total == 0 and not s.truncated
r = zut.tools["echo_data"](check=True, value=1)
assert r and str(r) == r.text
assert repr(r) == "ToolResult(id='cm/1', is_error=False, text='')"
r = zut.ToolResult(dict(id="long", text="x" * 100, data=None, is_error=True))
assert not r and str(r) == "x" * 100
assert repr(r) == "ToolResult(id='long', is_error=True, text='" + "x" * 80 + "…')"
r = zut.tools.glob(check=False, pattern="[")
assert r.is_error and not r
try:
    zut.tools["glob"](check=True, pattern="[")
except zut.ToolError as e:
    assert e.kind == "tool_error" and e.result.is_error
else:
    raise AssertionError("expected ToolError")
zut.result("ergonomics ok")`, 10, nil, &codemodeEchoDataTool{})
	if result.IsError || !strings.Contains(codemodeOutput(t, result), "ergonomics ok") {
		t.Fatal(codemodeOutput(t, result))
	}
}
