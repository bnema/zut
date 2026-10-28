package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/bnema/zut/packages/agent/mcp/oauth"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// ---- helpers ----

func startManager(t *testing.T, servers map[string]ServerConfig, cwd string) (*Manager, []error, error) {
	t.Helper()
	m := NewManager(Config{Servers: servers}, cwd, ClientInfo{Name: "zut-test", Version: "0"}, nil)
	t.Cleanup(func() { _ = m.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	failures, reqErr := m.Start(ctx)
	return m, failures, reqErr
}

func startFake(t *testing.T, extraEnv map[string]string, tweak func(*ServerConfig)) (*Manager, string) {
	t.Helper()
	cfg, events := fakeStdioConfig(t, extraEnv)
	if tweak != nil {
		tweak(&cfg)
	}
	m, failures, reqErr := startManager(t, map[string]ServerConfig{"fake": cfg}, "")
	if len(failures) != 0 || reqErr != nil {
		t.Fatalf("start failed: %v / %v", failures, reqErr)
	}
	return m, events
}

func findTool(t *testing.T, m *Manager, name string) core.Tool {
	t.Helper()
	for _, tool := range m.Tools() {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found; have %v", name, toolNames(m))
	return nil
}

func toolNames(m *Manager) []string {
	var names []string
	for _, tool := range m.Tools() {
		names = append(names, tool.Name())
	}
	return names
}

func hasTool(m *Manager, name string) bool {
	for _, n := range toolNames(m) {
		if n == name {
			return true
		}
	}
	return false
}

func execTool(t *testing.T, tool core.Tool, args string) core.ToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := tool.Execute(ctx, json.RawMessage(args), nil)
	if err != nil {
		t.Fatalf("%s: Execute returned error: %v", tool.Name(), err)
	}
	return res
}

func resultText(res core.ToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tb, ok := c.(provider.TextBlock); ok {
			parts = append(parts, tb.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// ---- stdio end to end ----

func TestManagerStdioToolsAndDescriptions(t *testing.T) {
	m, _ := startFake(t, nil, nil)

	if m.Empty() {
		t.Fatal("manager should not be empty")
	}
	st := m.Status()
	if len(st) != 1 || !st[0].Connected || st[0].Tools != fakeToolCount || !st[0].Resources || st[0].Transport != "stdio" {
		t.Fatalf("status = %+v", st)
	}
	if st[0].Info.ProtocolVersion != "2025-06-18" || !st[0].Info.ToolsChanged || st[0].Info.Name != "fake" {
		t.Errorf("server info = %+v", st[0].Info)
	}

	tools := m.Tools()
	// All server tools across two pages + 2 resource helpers.
	if len(tools) != fakeToolCount+2 {
		t.Fatalf("got %d tools: %v", len(tools), toolNames(m))
	}
	seen := map[string]bool{}
	for _, tool := range tools {
		if seen[tool.Name()] {
			t.Errorf("duplicate tool name %s", tool.Name())
		}
		seen[tool.Name()] = true
		if !IsTool(tool) {
			t.Errorf("IsTool(%s) = false", tool.Name())
		}
		var schema map[string]any
		if err := json.Unmarshal(tool.Schema(), &schema); err != nil || schema["type"] != "object" {
			t.Errorf("%s schema invalid: %s", tool.Name(), tool.Schema())
		}
	}
	// A tool from page two proves pagination was followed.
	for _, want := range []string{"mcp__fake__echo", "mcp__fake__structured", "mcp__fake__getenv", ListResourcesToolName, ReadResourceToolName} {
		if !seen[want] {
			t.Errorf("missing tool %s", want)
		}
	}

	echo := findTool(t, m, "mcp__fake__echo")
	if got, want := echo.Description(), "[MCP fake/echo, read-only] Echo text back"; got != want {
		t.Errorf("echo description = %q, want %q", got, want)
	}
	if !IsReadOnlyTool(echo) {
		t.Error("echo carries readOnlyHint and should be read-only")
	}
	img := findTool(t, m, "mcp__fake__image")
	if strings.Contains(img.Description(), "read-only") || IsReadOnlyTool(img) {
		t.Errorf("image tool must not be read-only: %q", img.Description())
	}
	// Top-level anyOf from the server schema is removed.
	if strings.Contains(string(echo.Schema()), "anyOf") {
		t.Errorf("schema still has anyOf: %s", echo.Schema())
	}
	if !IsReadOnlyTool(findTool(t, m, ListResourcesToolName)) || !IsReadOnlyTool(findTool(t, m, ReadResourceToolName)) {
		t.Error("resource helpers are read-only")
	}
	if IsTool(nil) || IsReadOnlyTool(nil) {
		t.Error("nil is not an MCP tool")
	}
	if IsTool(foreignTool{}) || IsReadOnlyTool(foreignTool{}) {
		t.Error("foreign tools are not MCP tools")
	}
	if !IsMCPToolName("mcp__fake__echo") || !IsMCPToolName(ListResourcesToolName) || IsMCPToolName("bash") {
		t.Error("IsMCPToolName wrong")
	}

	text := FormatStatus(m.Status())
	if !strings.Contains(text, "fake (stdio): connected, 18 tools, resources") {
		t.Errorf("FormatStatus = %q", text)
	}
	if FormatStatus(nil) != "no MCP servers configured" {
		t.Error("empty FormatStatus")
	}
	add := m.SystemPromptAddendum()
	for _, want := range []string{"[mcp_servers]", "fake: 18 tools", "mcp__fake__<tool>", "Be nice"} {
		if !strings.Contains(add, want) {
			t.Errorf("addendum missing %q:\n%s", want, add)
		}
	}
}

type foreignTool struct{}

func (foreignTool) Name() string            { return "foreign" }
func (foreignTool) Description() string     { return "" }
func (foreignTool) Schema() json.RawMessage { return json.RawMessage(`{}`) }
func (foreignTool) Execute(context.Context, json.RawMessage, func(string)) (core.ToolResult, error) {
	return core.ToolResult{}, nil
}

func TestManagerStdioExecuteConversion(t *testing.T) {
	m, _ := startFake(t, map[string]string{"FAKE_JUNK": "1"}, nil)

	t.Run("text", func(t *testing.T) {
		res := execTool(t, findTool(t, m, "mcp__fake__echo"), `{"text":"hi"}`)
		if res.IsError || resultText(res) != "echo: hi" {
			t.Fatalf("got %+v", res)
		}
		d, ok := res.Details.(ToolDetails)
		if !ok || d.Server != "fake" || d.Tool != "echo" {
			t.Errorf("details = %#v", res.Details)
		}
	})
	t.Run("image", func(t *testing.T) {
		res := execTool(t, findTool(t, m, "mcp__fake__image"), `{}`)
		if res.IsError || len(res.Content) != 2 {
			t.Fatalf("got %+v", res)
		}
		if tb, ok := res.Content[0].(provider.TextBlock); !ok || tb.Text != "an image" {
			t.Errorf("content[0] = %#v", res.Content[0])
		}
		ib, ok := res.Content[1].(provider.ImageBlock)
		if !ok || ib.MimeType != "image/png" || string(ib.Data) != string(fakeImageBytes) {
			t.Errorf("content[1] = %#v", res.Content[1])
		}
	})
	t.Run("embedded resource", func(t *testing.T) {
		res := execTool(t, findTool(t, m, "mcp__fake__resource"), `{}`)
		if got, want := resultText(res), "[resource mem://emb]\nembedded body"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("isError", func(t *testing.T) {
		res := execTool(t, findTool(t, m, "mcp__fake__err"), `{}`)
		if !res.IsError || resultText(res) != "tool failed" {
			t.Errorf("got %+v", res)
		}
	})
	t.Run("structuredContent fallback", func(t *testing.T) {
		res := execTool(t, findTool(t, m, "mcp__fake__structured"), `{}`)
		if res.IsError || resultText(res) != `{"answer":42}` {
			t.Errorf("got %+v (%q)", res, resultText(res))
		}
	})
	t.Run("invalid arguments", func(t *testing.T) {
		res := execTool(t, findTool(t, m, "mcp__fake__echo"), `[1]`)
		if !res.IsError || !strings.Contains(resultText(res), "JSON object") {
			t.Errorf("got %q", resultText(res))
		}
	})
	t.Run("empty arguments", func(t *testing.T) {
		res := execTool(t, findTool(t, m, "mcp__fake__echo"), ``)
		if res.IsError || resultText(res) != "echo: " {
			t.Errorf("got %q", resultText(res))
		}
	})
	t.Run("output truncated at 50KiB", func(t *testing.T) {
		res := execTool(t, findTool(t, m, "mcp__fake__big"), `{}`)
		text := resultText(res)
		marker := "\n... [truncated at 51200 bytes]"
		if !strings.Contains(text, marker) {
			t.Fatalf("no truncation marker; len=%d", len(text))
		}
		if len(text) > 50*1024+len(marker)+len("\n") {
			t.Errorf("text too long: %d", len(text))
		}
		if strings.Contains(text, "tail") {
			t.Error("content after the budget must be dropped")
		}
		if strings.Count(text, "a") < 50*1024 {
			t.Error("budgeted text was cut too early")
		}
	})
}

func TestManagerStdioServerToClientRequests(t *testing.T) {
	cwd := t.TempDir()
	cfg, _ := fakeStdioConfig(t, nil)
	m, failures, reqErr := startManager(t, map[string]ServerConfig{"fake": cfg}, cwd)
	if len(failures) != 0 || reqErr != nil {
		t.Fatal(failures, reqErr)
	}
	res := execTool(t, findTool(t, m, "mcp__fake__ask_roots"), `{}`)
	text := resultText(res)
	if res.IsError || !strings.Contains(text, "ping={}") {
		t.Fatalf("server->client ping not answered: %q", text)
	}
	if !strings.Contains(text, `"roots":[{`) || !strings.Contains(text, "file://") || !strings.Contains(text, filepath.Base(cwd)) {
		t.Errorf("roots/list answer wrong: %q", text)
	}
	// The working directory is applied to the child.
	cwdRes := execTool(t, findTool(t, m, "mcp__fake__cwd"), `{}`)
	got, _ := filepath.EvalSymlinks(resultText(cwdRes))
	want, _ := filepath.EvalSymlinks(cwd)
	if got != want {
		t.Errorf("child cwd = %q, want %q", got, want)
	}
}

func TestManagerStdioEnvIsolation(t *testing.T) {
	t.Setenv("SECRET_API_KEY", "host-secret")
	t.Setenv("MCP_T_PASS", "passed-through")
	m, _ := startFake(t, map[string]string{"MCP_T_EXPLICIT": "from-cfg"}, func(c *ServerConfig) {
		c.EnvVars = []string{"MCP_T_PASS"}
	})
	getenv := findTool(t, m, "mcp__fake__getenv")
	for name, want := range map[string]string{
		"SECRET_API_KEY": "[]",
		"MCP_T_PASS":     "[passed-through]",
		"MCP_T_EXPLICIT": "[from-cfg]",
	} {
		args, _ := json.Marshal(map[string]string{"name": name})
		if got := resultText(execTool(t, getenv, string(args))); got != want {
			t.Errorf("getenv %s = %s, want %s", name, got, want)
		}
	}
}

func TestManagerToolFiltering(t *testing.T) {
	t.Run("enabledTools", func(t *testing.T) {
		m, _ := startFake(t, nil, func(c *ServerConfig) { c.EnabledTools = []string{"echo", "structured"} })
		names := toolNames(m)
		want := map[string]bool{"mcp__fake__echo": true, "mcp__fake__structured": true, ListResourcesToolName: true, ReadResourceToolName: true}
		if len(names) != len(want) {
			t.Fatalf("names = %v", names)
		}
		for _, n := range names {
			if !want[n] {
				t.Errorf("unexpected tool %s", n)
			}
		}
		if got := len(m.ServerTools("fake")); got != 2 {
			t.Errorf("ServerTools = %d", got)
		}
	})
	t.Run("disabledTools", func(t *testing.T) {
		m, _ := startFake(t, nil, func(c *ServerConfig) { c.DisabledTools = []string{"echo", "slow", "exit"} })
		for _, n := range []string{"mcp__fake__echo", "mcp__fake__slow", "mcp__fake__exit"} {
			if hasTool(m, n) {
				t.Errorf("%s should be filtered", n)
			}
		}
		if !hasTool(m, "mcp__fake__image") || len(m.ServerTools("fake")) != fakeToolCount-3 {
			t.Errorf("remaining tools wrong: %v", toolNames(m))
		}
	})
	t.Run("both", func(t *testing.T) {
		m, _ := startFake(t, nil, func(c *ServerConfig) {
			c.EnabledTools = []string{"echo", "image"}
			c.DisabledTools = []string{"image"}
		})
		if got := m.ServerTools("fake"); len(got) != 1 || got[0].Name != "echo" {
			t.Errorf("ServerTools = %+v", got)
		}
	})
}

func TestManagerToolTimeout(t *testing.T) {
	m, events := startFake(t, nil, func(c *ServerConfig) { c.ToolTimeoutSec = 0.3 })
	start := time.Now()
	res := execTool(t, findTool(t, m, "mcp__fake__slow"), `{}`)
	if !res.IsError {
		t.Fatalf("expected error result, got %q", resultText(res))
	}
	text := resultText(res)
	if !strings.Contains(text, "toolTimeoutSec") || !strings.Contains(text, "timed out") || !strings.Contains(text, "fake/slow") {
		t.Errorf("message = %q", text)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("timeout took far too long")
	}
	// The client tells the server to stop working on the request.
	waitEvent(t, events, "cancelled:")
	// The connection stays usable afterwards.
	if got := resultText(execTool(t, findTool(t, m, "mcp__fake__echo"), `{"text":"after"}`)); got != "echo: after" {
		t.Errorf("after timeout: %q", got)
	}
}

func TestManagerToolCancellation(t *testing.T) {
	m, events := startFake(t, nil, nil)
	tool := findTool(t, m, "mcp__fake__slow")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type out struct {
		res core.ToolResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := tool.Execute(ctx, json.RawMessage(`{}`), nil)
		done <- out{res, err}
	}()
	waitEvent(t, events, "slow-started")
	cancel()
	select {
	case o := <-done:
		if o.err != nil || !o.res.IsError || !strings.Contains(resultText(o.res), "cancelled") {
			t.Errorf("got %+v err=%v", o.res, o.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not return after cancellation")
	}
	waitEvent(t, events, "cancelled:")
}

func TestManagerReconnectAfterServerDies(t *testing.T) {
	m, events := startFake(t, nil, nil)
	pidTool := findTool(t, m, "mcp__fake__pid")
	pid1, err := strconv.Atoi(resultText(execTool(t, pidTool, `{}`)))
	if err != nil {
		t.Fatal(err)
	}
	res := execTool(t, findTool(t, m, "mcp__fake__exit"), `{}`)
	if !res.IsError {
		t.Fatalf("killing call should report an error, got %q", resultText(res))
	}
	if canCheckProcess {
		eventually(t, "first process to exit", func() bool { return !processAlive(pid1) })
	}
	// The next call transparently reconnects with a fresh process.
	pid2, err := strconv.Atoi(resultText(execTool(t, pidTool, `{}`)))
	if err != nil {
		t.Fatalf("reconnect failed: %v", err)
	}
	if pid2 == pid1 {
		t.Errorf("expected a new server process, pid stayed %d", pid1)
	}
	if n := countEvents(events, "start"); n != 2 {
		t.Errorf("server started %d times, want 2", n)
	}
	if st := m.Status(); !st[0].Connected || st[0].Err != "" {
		t.Errorf("status after reconnect = %+v", st[0])
	}
	if !hasTool(m, "mcp__fake__echo") {
		t.Error("tools lost after reconnect")
	}
}

func TestManagerCloseIdempotentAndProcessExits(t *testing.T) {
	m, events := startFake(t, nil, nil)
	pid, err := strconv.Atoi(resultText(execTool(t, findTool(t, m, "mcp__fake__pid"), `{}`)))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if canCheckProcess {
		eventually(t, "server process to exit", func() bool { return !processAlive(pid) })
	}
	waitEvent(t, events, "stdin-closed")
	if st := m.Status(); st[0].Connected {
		t.Error("still connected after Close")
	}
	// Using a closed manager reports an error result, never hangs or reconnects.
	res := execTool(t, findTool(t, m, "mcp__fake__echo"), `{"text":"x"}`)
	if !res.IsError || !strings.Contains(resultText(res), "closed") {
		t.Errorf("after close: %q", resultText(res))
	}
	if n := countEvents(events, "start"); n != 1 {
		t.Errorf("closed manager restarted the server (%d starts)", n)
	}
	var nilManager *Manager
	if err := nilManager.Close(); err != nil {
		t.Error(err)
	}
}

func TestManagerRequiredAndOptionalFailures(t *testing.T) {
	good, _ := fakeStdioConfig(t, nil)
	missing := ServerConfig{Command: filepath.Join(t.TempDir(), "no-such-binary")}

	t.Run("optional", func(t *testing.T) {
		m, failures, reqErr := startManager(t, map[string]ServerConfig{"good": good, "bad": missing}, "")
		if reqErr != nil {
			t.Fatalf("optional failure must not be fatal: %v", reqErr)
		}
		if len(failures) != 1 || !strings.Contains(failures[0].Error(), `"bad"`) {
			t.Fatalf("failures = %v", failures)
		}
		if !hasTool(m, "mcp__good__echo") {
			t.Error("healthy server should still expose tools")
		}
		for _, st := range m.Status() {
			switch st.Name {
			case "bad":
				if st.Connected || st.Err == "" || st.Tools != 0 {
					t.Errorf("bad status = %+v", st)
				}
			case "good":
				if !st.Connected {
					t.Errorf("good status = %+v", st)
				}
			}
		}
		if text := FormatStatus(m.Status()); !strings.Contains(text, "bad (stdio): disconnected") || !strings.Contains(text, "error:") {
			t.Errorf("FormatStatus = %q", text)
		}
	})
	t.Run("required", func(t *testing.T) {
		req := missing
		req.Required = true
		_, failures, reqErr := startManager(t, map[string]ServerConfig{"good": good, "bad": req}, "")
		if reqErr == nil || !strings.Contains(reqErr.Error(), `"bad"`) {
			t.Fatalf("requiredErr = %v", reqErr)
		}
		if len(failures) != 1 {
			t.Errorf("failures = %v", failures)
		}
	})
	t.Run("empty manager", func(t *testing.T) {
		m := NewManager(Config{}, "", ClientInfo{}, nil)
		if !m.Empty() || m.Tools() != nil || m.SystemPromptAddendum() != "" {
			t.Error("empty manager should expose nothing")
		}
		if f, r := m.Start(context.Background()); f != nil || r != nil {
			t.Error("empty Start")
		}
	})
}

func TestManagerStartupTimeoutMessage(t *testing.T) {
	_, failures, reqErr := startManager(t, map[string]ServerConfig{"slow": func() ServerConfig {
		cfg, _ := fakeStdioConfig(t, map[string]string{"FAKE_INIT_DELAY_MS": "5000"})
		cfg.StartupTimeoutSec = 0.3
		cfg.Required = true
		return cfg
	}()}, "")
	if len(failures) != 1 || reqErr == nil {
		t.Fatalf("failures=%v reqErr=%v", failures, reqErr)
	}
	msg := failures[0].Error()
	if !strings.Contains(msg, "startupTimeoutSec") || !strings.Contains(msg, "timed out") {
		t.Errorf("message = %q", msg)
	}
}

func TestManagerInitializeFailureIncludesStderr(t *testing.T) {
	cfg, _ := fakeStdioConfig(t, map[string]string{"FAKE_INIT_ERR": "1"})
	_, failures, _ := startManager(t, map[string]ServerConfig{"s": cfg}, "")
	if len(failures) != 1 {
		t.Fatalf("failures = %v", failures)
	}
	msg := failures[0].Error()
	for _, want := range []string{"init refused", "server stderr:", "fatal: missing credentials"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q: %q", want, msg)
		}
	}
	var rpcErr *RPCError
	if !errors.As(failures[0], &rpcErr) || rpcErr.Code != -32000 {
		t.Errorf("RPCError not unwrappable: %v", failures[0])
	}
}

func TestManagerRejectsUnsupportedProtocolAndBadPagination(t *testing.T) {
	cfg, _ := fakeStdioConfig(t, map[string]string{"FAKE_PROTO": "1999-01-01"})
	_, failures, _ := startManager(t, map[string]ServerConfig{"s": cfg}, "")
	if len(failures) != 1 || !strings.Contains(failures[0].Error(), "unsupported protocol version") {
		t.Errorf("failures = %v", failures)
	}
	cfg, _ = fakeStdioConfig(t, map[string]string{"FAKE_BAD_CURSOR": "1"})
	_, failures, _ = startManager(t, map[string]ServerConfig{"s": cfg}, "")
	if len(failures) != 1 || !strings.Contains(failures[0].Error(), "repeated pagination cursor") {
		t.Errorf("failures = %v", failures)
	}
}

func TestManagerToolsListChanged(t *testing.T) {
	m, _ := startFake(t, nil, nil)
	if hasTool(m, "mcp__fake__extra") {
		t.Fatal("extra tool should not exist yet")
	}
	res := execTool(t, findTool(t, m, "mcp__fake__add_tool"), `{}`)
	if resultText(res) != "added" {
		t.Fatalf("add_tool = %q", resultText(res))
	}
	eventually(t, "re-listed tools to include extra", func() bool {
		for _, tool := range m.ServerTools("fake") {
			if tool.Name == "extra" {
				return true
			}
		}
		return false
	})
	if !hasTool(m, "mcp__fake__extra") {
		t.Error("Tools() should surface the new tool")
	}
}

func TestManagerResourceTools(t *testing.T) {
	m, _ := startFake(t, nil, nil)
	list := findTool(t, m, ListResourcesToolName)
	text := resultText(execTool(t, list, `{}`))
	for _, want := range []string{"## fake", "- mem://a", "A", "(text/plain)", ": first", "- template mem://item/{id}", "Item"} {
		if !strings.Contains(text, want) {
			t.Errorf("list output missing %q:\n%s", want, text)
		}
	}
	if got := resultText(execTool(t, list, `{"server":"fake"}`)); !strings.Contains(got, "mem://a") {
		t.Errorf("filtered list = %q", got)
	}
	if res := execTool(t, list, `{bad`); !res.IsError {
		t.Error("bad args should be an error result")
	}
	if got := resultText(execTool(t, list, `{"server":"nope"}`)); !strings.Contains(got, "unavailable") {
		t.Errorf("unknown server list = %q", got)
	}

	read := findTool(t, m, ReadResourceToolName)
	res := execTool(t, read, `{"server":"fake","uri":"mem://a"}`)
	if res.IsError || resultText(res) != "[resource mem://a]\ncontent of mem://a" {
		t.Errorf("read = %+v %q", res, resultText(res))
	}
	if d, _ := res.Details.(ToolDetails); d.Server != "fake" {
		t.Errorf("details = %#v", res.Details)
	}
	for name, args := range map[string]string{
		"missing uri":    `{"server":"fake"}`,
		"missing server": `{"uri":"mem://a"}`,
		"unknown server": `{"server":"nope","uri":"mem://a"}`,
		"invalid json":   `{`,
	} {
		if res := execTool(t, read, args); !res.IsError {
			t.Errorf("%s: want error result, got %q", name, resultText(res))
		}
	}
}

func TestManagerResourceToolsOnlyWhenServerHasResources(t *testing.T) {
	m, _ := startFake(t, map[string]string{"FAKE_NO_RESOURCES": "1"}, nil)
	if hasTool(m, ListResourcesToolName) || hasTool(m, ReadResourceToolName) {
		t.Errorf("resource tools should be absent: %v", toolNames(m))
	}
	if len(m.Tools()) != fakeToolCount {
		t.Errorf("tools = %v", toolNames(m))
	}
	if strings.Contains(FormatStatus(m.Status()), "resources") {
		t.Error("status should not mention resources")
	}
}

func TestManagerMultipleServersNameCollision(t *testing.T) {
	a, _ := fakeStdioConfig(t, nil)
	b, _ := fakeStdioConfig(t, nil)
	m, failures, _ := startManager(t, map[string]ServerConfig{"a.b": a, "a_b": b}, "")
	if len(failures) != 0 {
		t.Fatal(failures)
	}
	seen := map[string]bool{}
	for _, tool := range m.Tools() {
		if seen[tool.Name()] {
			t.Fatalf("duplicate tool name %s", tool.Name())
		}
		seen[tool.Name()] = true
		if len(tool.Name()) > 64 {
			t.Errorf("name too long: %s", tool.Name())
		}
	}
	// Execute routes to the right server regardless of the hashed name.
	for _, tool := range m.Tools() {
		if strings.HasPrefix(tool.Name(), "mcp__a_b__echo") {
			if got := resultText(execTool(t, tool, `{"text":"z"}`)); got != "echo: z" {
				t.Errorf("%s -> %q", tool.Name(), got)
			}
		}
	}
	if got := m.Servers(); len(got) != 2 || got[0] != "a.b" {
		t.Errorf("Servers = %v", got)
	}
}

func TestManagerStartAfterClose(t *testing.T) {
	cfg, events := fakeStdioConfig(t, nil)
	m := NewManager(Config{Servers: map[string]ServerConfig{"s": cfg}}, "", ClientInfo{}, nil)
	_ = m.Close()
	failures, _ := m.Start(context.Background())
	if len(failures) != 1 || !strings.Contains(failures[0].Error(), "closed") {
		t.Errorf("failures = %v", failures)
	}
	if _, err := os.Stat(events); err == nil {
		t.Error("a closed manager must not spawn the server")
	}
}

func TestManagerHungHTTPDoesNotPreventHealthyServer(t *testing.T) {
	hung := newHTTPFake(false)
	hung.hangInit = true
	bad := httpServerConfig(hung.start(t))
	bad.StartupTimeoutSec = 0.05
	healthy := newHTTPFake(false)
	good := httpServerConfig(healthy.start(t))
	m := NewManager(Config{Servers: map[string]ServerConfig{"hung": bad, "healthy": good}}, "", ClientInfo{}, nil)
	defer m.Close()
	done := make(chan []error, 1)
	go func() { failures, _ := m.Start(context.Background()); done <- failures }()
	select {
	case failures := <-done:
		if len(failures) != 1 || !strings.Contains(failures[0].Error(), "hung") {
			t.Fatalf("failures = %v", failures)
		}
		if len(m.ServerTools("healthy")) == 0 {
			t.Fatal("healthy server did not load")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("startup blocked")
	}
}

func TestManagerHungStdioIsKilledAndHealthyLoads(t *testing.T) {
	bad, _ := fakeStdioConfig(t, map[string]string{"FAKE_HANG_INIT": "1"})
	bad.StartupTimeoutSec = 0.2
	good, _ := fakeStdioConfig(t, nil)
	reaped := make(chan int, 4)
	hook := func(pid int) { reaped <- pid }
	afterReapHook.Store(&hook)
	defer afterReapHook.Store(nil)
	m := NewManager(Config{Servers: map[string]ServerConfig{"hung": bad, "healthy": good}}, "", ClientInfo{}, nil)
	defer m.Close()
	done := make(chan []error, 1)
	go func() { failures, _ := m.Start(context.Background()); done <- failures }()
	select {
	case failures := <-done:
		if len(failures) != 1 || !strings.Contains(failures[0].Error(), "hung") {
			t.Fatalf("failures = %v", failures)
		}
		if len(m.ServerTools("healthy")) == 0 {
			t.Fatal("healthy server did not load")
		}
	case <-time.After(bad.StartupTimeout() + 2*time.Second):
		t.Fatal("startup exceeded cleanup slack")
	}
	{
		select {
		case <-reaped:
		case <-time.After(time.Second):
			t.Fatal("hung child was not reaped")
		}
	}
}

func TestManagerStartupRefreshAndLockFailures(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(strconv.FormatBool(held), func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}))
			defer token.Close()
			defer func() {
				if release != nil {
					close(release)
				}
			}()
			badFake := newHTTPFake(false)
			bad := httpServerConfig(badFake.start(t))
			bad.StartupTimeoutSec = 0.1
			goodFake := newHTTPFake(false)
			good := httpServerConfig(goodFake.start(t))
			good.Headers = map[string]string{"Authorization": "Bearer explicit"}
			store := oauth.Store{Path: filepath.Join(t.TempDir(), "auth.json")}
			if err := store.Set(context.Background(), bad.URL, &oauth.Entry{AccessToken: "old", RefreshToken: "refresh", Expiry: time.Now().Add(-time.Hour), TokenEndpoint: token.URL}); err != nil {
				t.Fatal(err)
			}
			if held {
				req, _ := http.NewRequest("GET", bad.URL, nil)
				holder := make(chan struct{})
				go func() {
					resp, _ := oauth.HTTPClient(nil, store, bad.URL, "holder").Do(req)
					if resp != nil {
						resp.Body.Close()
					}
					close(holder)
				}()
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("holder did not acquire lock")
				}
				defer func() { close(release); <-holder; release = nil }()
			}
			m := NewManager(Config{Servers: map[string]ServerConfig{"bad": bad, "healthy": good}}, "", ClientInfo{}, nil)
			m.SetAuthPath(store.Path)
			defer m.Close()
			done := make(chan []error, 1)
			go func() { failures, _ := m.Start(context.Background()); done <- failures }()
			select {
			case failures := <-done:
				if len(failures) != 1 || !strings.Contains(failures[0].Error(), "bad") {
					t.Fatalf("failures = %v", failures)
				}
				if held && !strings.Contains(failures[0].Error(), "locked by another zut process") {
					t.Fatalf("missing lock cause: %v", failures)
				}
				if len(m.ServerTools("healthy")) == 0 {
					t.Fatal("healthy server did not load")
				}
			case <-time.After(bad.StartupTimeout() + 2*time.Second):
				t.Fatal("startup exceeded bound")
			}
		})
	}
}

func TestReconnectWaitHonorsContext(t *testing.T) {
	m := NewManager(Config{Servers: map[string]ServerConfig{"busy": {Command: "unused"}}}, "", ClientInfo{}, nil)
	s := m.servers["busy"]
	s.connectMu.Lock()
	defer s.connectMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := m.connect(ctx, s)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("connect = %v", err)
	}
}

func TestNormalCloseAllowsEOFCleanup(t *testing.T) {
	for _, managerClose := range []bool{false, true} {
		t.Run(strconv.FormatBool(managerClose), func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}))
			defer srv.Close()
			defer close(release)
			cfg, events := fakeStdioConfig(t, map[string]string{"FAKE_EOF_CLEANUP_URL": srv.URL})
			m, failures, err := startManager(t, map[string]ServerConfig{"cleanup": cfg}, "")
			if err != nil || len(failures) != 0 {
				t.Fatalf("startup: %v %v", failures, err)
			}
			c := m.servers["cleanup"].live()
			done := make(chan error, 1)
			go func() {
				if managerClose {
					done <- m.Close()
				} else {
					done <- c.Close()
				}
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("cleanup did not begin")
			}
			// Hold cleanup beyond the failed-startup EOF grace, using a deadline
			// rather than sleep-based synchronization. Normal close must still wait.
			timer := time.NewTimer(200 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
			case err := <-done:
				t.Fatalf("close interrupted cleanup: %v", err)
			}
			release <- struct{}{}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("close blocked")
			}
			data, err := os.ReadFile(events)
			if err != nil || !strings.Contains(string(data), "cleanup-complete") {
				t.Fatalf("cleanup incomplete: %s (%v)", data, err)
			}
		})
	}
}
