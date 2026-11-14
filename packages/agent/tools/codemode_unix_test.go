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

func TestCodemodeRealPythonBashData(t *testing.T) {
	dir := t.TempDir()
	tool := &CodemodeTool{CWD: dir, Env: os.Environ(), Interpreter: codemodePython(t)}
	result := runCodemodeAgent(t, tool, `r = zut.tools["bash"](command="printf captured; exit 3", timeout=5, check=False)
assert not r and r.is_error
assert r.data == {"exit_code": 3, "output": "captured", "timed_out": False}
assert "captured" in str(r)
zut.result(r.data)`, 10, nil, &BashTool{CWD: dir, Env: os.Environ()})
	if result.IsError {
		t.Fatal(codemodeOutput(t, result))
	}
}

type codemodeMCPTestTool struct{ codemodeEchoDataTool }

func (*codemodeMCPTestTool) Name() string { return "mcp__srv__list-things" }

func TestCodemodeRealPythonMCPNamespace(t *testing.T) {
	tool := &CodemodeTool{CWD: t.TempDir(), Env: os.Environ(), Interpreter: codemodePython(t)}
	result := runCodemodeAgent(t, tool, `assert not hasattr(zut.tools, "__wrapped__")
assert not hasattr(zut.tools, "__length_hint__")
assert dir(zut.mcp) == ["srv"]
assert dir(zut.mcp.srv) == ["list_things"]
r = zut.mcp.srv.list_things(check=True, value="forwarded")
assert r.data == {"value": "forwarded"}
for obj, name, suggestion in [(zut.mcp, "sr", "srv"), (zut.mcp.srv, "list_thing", "list_things")]:
    try:
        getattr(obj, name)
    except AttributeError as e:
        assert suggestion in str(e) and "did you mean" in str(e)
    else:
        raise AssertionError("expected AttributeError")
assert not hasattr(zut.mcp, "__wrapped__")
zut.result("namespace ok")`, 10, nil, &codemodeMCPTestTool{})
	if result.IsError || !strings.Contains(codemodeOutput(t, result), "namespace ok") {
		t.Fatal(codemodeOutput(t, result))
	}
}

func TestCodemodeRealPythonHelp(t *testing.T) {
	tool := &CodemodeTool{CWD: t.TempDir(), Env: os.Environ(), Interpreter: codemodePython(t)}
	result := runCodemodeAgent(t, tool, `import ast, contextlib, io
signature = zut.describe("glob")["signature"]
ast.parse(signature)
assert "pattern: str" in signature
out = io.StringIO()
with contextlib.redirect_stdout(out):
    zut.help("glob")
assert signature in out.getvalue()
method = zut.mcp.srv.list_things
assert method.__name__ == "mcp__srv__list-things"
with contextlib.redirect_stdout(out):
    zut.help(method)
assert "def mcp__srv__list_things(" in method.__doc__
assert "Return the supplied JSON unchanged" in method.__doc__
assert method.__doc__ in out.getvalue()
zut.result("help ok")`, 10, nil, &codemodeMCPTestTool{})
	if result.IsError || !strings.Contains(codemodeOutput(t, result), "help ok") {
		t.Fatal(codemodeOutput(t, result))
	}
}

type codemodeMCPCollisionTestTool struct{ codemodeEchoDataTool }

func (*codemodeMCPCollisionTestTool) Name() string   { return "mcp___MCP__server___MCP__server" }
func (*codemodeMCPCollisionTestTool) Server() string { return "_MCP__server" }
func TestCodemodeRealPythonMCPStateCollision(t *testing.T) {
	tool := &CodemodeTool{CWD: t.TempDir(), Env: os.Environ(), Interpreter: codemodePython(t)}
	result := runCodemodeAgent(t, tool, `assert dir(zut.mcp) == ["_MCP__server"]
server = zut.mcp._MCP__server
assert dir(server) == ["_MCP__server"]
assert server._MCP__server(value=42).data == {"value": 42}
zut.result("collision ok")`, 10, nil, &codemodeMCPCollisionTestTool{})
	if result.IsError || !strings.Contains(codemodeOutput(t, result), "collision ok") {
		t.Fatal(codemodeOutput(t, result))
	}
}
