package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/mcp"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/stretchr/testify/require"
)

// stdioMCPFakeEnv makes the test binary act as a minimal stdio MCP server
// (see TestMain) with one "pid" tool returning its process id.
const stdioMCPFakeEnv = "ZUT_AGENT_TEST_STDIO_MCP"

func runStdioMCPFake() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	out := json.NewEncoder(os.Stdout)
	for sc.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil || len(msg.ID) == 0 {
			continue
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fake", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "pid", "description": "Process id", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": strconv.Itoa(os.Getpid())}}}
		default:
			result = map[string]any{}
		}
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	}
}

// writeStdioFakeConfig points the global mcp.json at the fake server.
func writeStdioFakeConfig(t *testing.T, home string) {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	writeMCPConfig(t, home, map[string]mcp.ServerConfig{"fake": {
		Command: exe,
		Args:    []string{"-test.run=^$"},
		Env:     map[string]string{stdioMCPFakeEnv: "1", "GORACE": "atexit_sleep_ms=0"},
	}})
}

func callPID(t *testing.T, m *mcp.Manager) int {
	t.Helper()
	for _, tool := range m.Tools() {
		if tool.Name() != "mcp__fake__pid" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		res, err := tool.Execute(ctx, json.RawMessage(`{}`), nil)
		require.NoError(t, err)
		pid, err := strconv.Atoi(toolResultText(res))
		require.NoError(t, err, toolResultText(res))
		return pid
	}
	t.Fatal("mcp__fake__pid missing")
	return 0
}

func toolResultText(res core.ToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(provider.TextBlock); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

func startPooledSessionMCP(t *testing.T, cwd string, pool *mcp.Pool) *mcp.Manager {
	t.Helper()
	args := Args{CWD: cwd, mcpPool: pool}
	m, stop, err := startSessionMCP(context.Background(), args, os.Environ())
	require.NoError(t, err)
	require.NotNil(t, m)
	t.Cleanup(stop)
	return m
}

func TestStartSessionMCPUsesPool(t *testing.T) {
	home := sessionAssemblyEnv(t)
	writeStdioFakeConfig(t, home)
	cwd := t.TempDir()
	pool := mcp.NewPool(time.Minute, nil)
	t.Cleanup(func() { _ = pool.Close() })
	a := startPooledSessionMCP(t, cwd, pool)
	b := startPooledSessionMCP(t, cwd, pool)
	require.Equal(t, callPID(t, a), callPID(t, b), "sessions on one pool must share the process")

	c := startPooledSessionMCP(t, cwd, nil)
	d := startPooledSessionMCP(t, cwd, nil)
	require.NotEqual(t, callPID(t, c), callPID(t, d), "without a pool each session owns its process")
}

func TestDaemonShutdownClosesPoolAfterSessions(t *testing.T) {
	home := sessionAssemblyEnv(t)
	writeStdioFakeConfig(t, home)
	td := startFactoryDaemon(t, home, daemonTestSocket(t))
	pool := td.server.factory.mcpPool
	require.NotNil(t, pool)
	// A session-like manager on the daemon pool holds the shared process.
	m := startPooledSessionMCP(t, t.TempDir(), pool)
	pid := callPID(t, m)
	td.stop()
	// After daemon shutdown the pool is closed: the process is gone and the
	// pool rejects new work.
	if testProcessCheckable {
		require.Eventually(t, func() bool { return !testProcessAlive(pid) }, 10*time.Second, 10*time.Millisecond, "shared process outlived the daemon")
	}
	// A non-required failure is reported, not fatal.
	m2, stop, err := startSessionMCP(context.Background(), Args{CWD: t.TempDir(), mcpPool: pool}, os.Environ())
	require.NoError(t, err)
	t.Cleanup(stop)
	require.Empty(t, m2.ServerTools("fake"))
	require.Contains(t, mcp.FormatStatus(m2.Status()), "pool closed")
}
