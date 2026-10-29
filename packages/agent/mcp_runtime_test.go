package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/mcp"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

// fakeMCPHTTPServer answers the minimal streamable-HTTP MCP surface with one
// read-only tool ("lookup") and one mutating tool ("deploy").
func fakeMCPHTTPServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake", "version": "1"},
				"instructions":    "Use lookup before deploy.",
			}
		case "tools/list":
			result = map[string]any{"tools": []any{
				map[string]any{"name": "lookup", "description": "Look something up.", "inputSchema": map[string]any{"type": "object"}, "annotations": map[string]any{"readOnlyHint": true}},
				map[string]any{"name": "deploy", "description": "Deploy.", "inputSchema": map[string]any{"type": "object"}},
			}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "called " + string(req.Params)}}}
		default:
			result = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func setupMCPHome(t *testing.T, url string) (project string) {
	t.Helper()
	root := t.TempDir()
	zutHome := filepath.Join(root, "zut-home")
	home := filepath.Join(root, "home")
	project = filepath.Join(root, "project")
	t.Setenv("ZUT_HOME", zutHome)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, dir := range []string{zutHome, home, project} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := mcp.Config{Servers: map[string]mcp.ServerConfig{"fake": {Type: "http", URL: url}}}
	if err := mcp.WriteConfigFile(mcp.GlobalConfigPath(zutHome), cfg); err != nil {
		t.Fatal(err)
	}
	return project
}

func startTestMCP(t *testing.T, args *Args) {
	t.Helper()
	manager, stop, err := startSessionMCP(context.Background(), *args, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	args.mcpManager = manager
	if args.mcpManager == nil {
		t.Fatalf("mcp manager not started: %v", args.mcpStartupErrors)
	}
}

func TestResolveAddsMCPToolsAndPrompt(t *testing.T) {
	project := setupMCPHome(t, fakeMCPHTTPServer(t).URL)
	args := Args{Provider: "ollama", Model: "any-local-model", CWD: project, NoLSP: true, NoSkill: true, Mode: ModePrint}
	startTestMCP(t, &args)

	r, err := Resolve(args, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mcp__fake__lookup", "mcp__fake__deploy"} {
		if _, ok := r.ToolRegistry[name]; !ok {
			t.Fatalf("registry missing %s", name)
		}
	}
	if !strings.Contains(r.SystemPrompt, "[mcp_servers]") || !strings.Contains(r.SystemPrompt, "Use lookup before deploy.") {
		t.Fatalf("system prompt lacks MCP addendum:\n%s", r.SystemPrompt)
	}
	res, err := r.ToolRegistry["mcp__fake__lookup"].Execute(context.Background(), json.RawMessage(`{"q":"x"}`), nil)
	if err != nil || res.IsError {
		t.Fatalf("execute = %+v, %v", res, err)
	}
}

func TestMCPToolSelection(t *testing.T) {
	project := setupMCPHome(t, fakeMCPHTTPServer(t).URL)
	base := Args{Provider: "ollama", Model: "any-local-model", CWD: project, NoLSP: true, NoSkill: true, Mode: ModePrint}
	startTestMCP(t, &base)

	for _, tc := range []struct {
		name  string
		edit  func(*Args)
		wants []string
		drops []string
	}{
		{name: "selector", edit: func(a *Args) { a.Tools, a.ToolsSet = []string{"read", "mcp"}, true }, wants: []string{"mcp__fake__lookup", "mcp__fake__deploy"}},
		{name: "exact", edit: func(a *Args) { a.Tools, a.ToolsSet = []string{"mcp__fake__lookup"}, true }, wants: []string{"mcp__fake__lookup"}, drops: []string{"mcp__fake__deploy"}},
		{name: "not listed", edit: func(a *Args) { a.Tools, a.ToolsSet = []string{"read"}, true }, drops: []string{"mcp__fake__lookup", "mcp__fake__deploy"}},
		{name: "no-mcp", edit: func(a *Args) { a.NoMCP = true }, drops: []string{"mcp__fake__lookup"}},
		{name: "packaged agent", edit: func(a *Args) { a.PermissionSet = &tools.PermissionSet{} }, drops: []string{"mcp__fake__lookup"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := base
			tc.edit(&args)
			reg := core.Registry{}
			addMCPTools(reg, args)
			for _, name := range tc.wants {
				if _, ok := reg[name]; !ok {
					t.Fatalf("missing %s in %v", name, keys(reg))
				}
			}
			for _, name := range tc.drops {
				if _, ok := reg[name]; ok {
					t.Fatalf("unexpected %s", name)
				}
			}
		})
	}
}

func TestOrchestratorHoldsReadOnlyMCPToolsAndWorkersGetAll(t *testing.T) {
	project := setupMCPHome(t, fakeMCPHTTPServer(t).URL)
	args := Args{Provider: "ollama", Model: "any-local-model", CWD: project, NoLSP: true, NoSkill: true, Mode: ModePrint, Orchestrate: true}
	startTestMCP(t, &args)

	r, err := Resolve(args, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.ToolRegistry["mcp__fake__lookup"]; !ok {
		t.Fatal("orchestrator lacks read-only MCP tool")
	}
	if _, ok := r.ToolRegistry["mcp__fake__deploy"]; ok {
		t.Fatal("orchestrator holds a mutating MCP tool")
	}
	narrowed := args
	narrowed.Tools, narrowed.ToolsSet = []string{"subagent", "mcp__fake__deploy"}, true
	narrowReg := core.Registry{}
	addMCPTools(narrowReg, narrowed)
	if len(narrowReg) != 0 {
		t.Fatalf("orchestrator --tools selection ignored: %v", keys(narrowReg))
	}
	if !strings.Contains(r.SystemPrompt, "delegate MCP actions") {
		t.Fatalf("orchestrator prompt lacks MCP delegation guidance:\n%s", r.SystemPrompt)
	}

	runtime := newSubagentRuntime(subagentRuntimeConfig{
		Args: args, Root: t.TempDir(), RepoRoot: project, Provider: "ollama", Model: "any-local-model",
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	spec, err := runtime.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{Task: "deploy"}, r.ToolRegistry)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mcp__fake__lookup", "mcp__fake__deploy"} {
		if !slices.Contains(spec.Tools, name) {
			t.Fatalf("worker tools %v missing %s", spec.Tools, name)
		}
	}

	childArgs := residentChildArgs(args, "ollama", spec)
	child, err := Resolve(childArgs, false)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := residentChildRegistry(child.ToolRegistry, spec.Tools)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg["mcp__fake__deploy"]; !ok {
		t.Fatal("worker registry lacks mutating MCP tool")
	}
}

func TestResidentChildPolicyMCPSelector(t *testing.T) {
	project := setupMCPHome(t, fakeMCPHTTPServer(t).URL)
	args := Args{Provider: "ollama", Model: "any-local-model", CWD: project, NoLSP: true, NoSkill: true}
	startTestMCP(t, &args)
	r, err := Resolve(args, false)
	if err != nil {
		t.Fatal(err)
	}
	runtime := newSubagentRuntime(subagentRuntimeConfig{
		Args: args, Root: t.TempDir(), RepoRoot: project, Provider: "ollama", Model: "any-local-model",
		Policy: subagents.SubagentPolicy{AllowedTools: []string{"read", "mcp"}},
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	spec, err := runtime.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{Task: "x"}, r.ToolRegistry)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(spec.Tools, "mcp__fake__deploy") || slices.Contains(spec.Tools, "write") {
		t.Fatalf("policy-filtered child tools = %v", spec.Tools)
	}
}

func TestResidentChildRegistrySkipsMissingMCPTool(t *testing.T) {
	reg, err := residentChildRegistry(core.Registry{"read": &tools.ReadTool{}}, []string{"read", "mcp__gone__tool"})
	if err != nil {
		t.Fatal(err)
	}
	if len(reg) != 1 {
		t.Fatalf("registry = %v", keys(reg))
	}
	if _, err := residentChildRegistry(core.Registry{}, []string{"write"}); err == nil {
		t.Fatal("missing non-MCP tool must still fail")
	}
}

func TestRequiredMCPServerFailureAbortsStart(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ZUT_HOME", root)
	cfg := mcp.Config{Servers: map[string]mcp.ServerConfig{"broken": {Command: filepath.Join(root, "missing-binary"), Required: true}}}
	if err := mcp.WriteConfigFile(mcp.GlobalConfigPath(root), cfg); err != nil {
		t.Fatal(err)
	}
	args := Args{CWD: root, Mode: ModePrint}
	if _, _, err := startMCPWithEnv(context.Background(), &args, "test", os.Environ()); err == nil || !strings.Contains(err.Error(), "required MCP server") {
		t.Fatalf("startMCPWithEnv err = %v", err)
	}
	cfg.Servers["broken"] = mcp.ServerConfig{Command: filepath.Join(root, "missing-binary")}
	if err := mcp.WriteConfigFile(mcp.GlobalConfigPath(root), cfg); err != nil {
		t.Fatal(err)
	}
	args = Args{CWD: root, Mode: ModeInteractive}
	_, stop, err := startMCPWithEnv(context.Background(), &args, "test", os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if len(args.mcpStartupErrors) != 1 {
		t.Fatalf("startup errors = %v", args.mcpStartupErrors)
	}
}

func TestMCPCommandAddListRemove(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ZUT_HOME", root)
	t.Setenv("FAKE_TOKEN", "secret-value")
	var out strings.Builder
	run := func(args ...string) error {
		out.Reset()
		_, err := runMCPCommand(context.Background(), append([]string{"mcp"}, args...), "test", &out)
		return err
	}
	if err := run("add", "local", "--env", "KEY=secret-value", "--", "server-bin", "--flag"); err != nil {
		t.Fatal(err)
	}
	if err := run("add", "remote", "--url", "https://example.test/mcp", "--bearer-token-env-var", "FAKE_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if err := run("add", "bad", "--url", "https://x.test", "--", "cmd"); err == nil {
		t.Fatal("mixed transport accepted")
	}
	if err := run("list"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "local\tstdio\tserver-bin --flag") || !strings.Contains(out.String(), "remote\thttp\thttps://example.test/mcp") {
		t.Fatalf("list output:\n%s", out.String())
	}
	if err := run("get", "local"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "secret-value") {
		t.Fatalf("get leaked a secret:\n%s", out.String())
	}
	if err := run("remove", "local"); err != nil {
		t.Fatal(err)
	}
	file, err := mcp.ReadConfigFile(mcp.GlobalConfigPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := file.Servers["local"]; ok || len(file.Servers) != 1 {
		t.Fatalf("servers after remove = %v", file.Servers)
	}
	if handled, _ := runMCPCommand(context.Background(), []string{"chat"}, "test", &out); handled {
		t.Fatal("non-mcp args handled")
	}
}

func TestMCPCommandListRedactsAndKeepsDisabledEntries(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ZUT_HOME", root)
	t.Setenv("REVIEW_SECRET", "synthetic-review-secret")
	off := false
	cfg := mcp.Config{Servers: map[string]mcp.ServerConfig{
		"local":  {Command: "server", Args: []string{"--token", "${REVIEW_SECRET}"}},
		"remote": {Type: "http", URL: "https://user:pw@example.test/mcp?key=${REVIEW_SECRET}"},
		"off":    {Command: "server", Enabled: &off},
	}}
	if err := mcp.WriteConfigFile(mcp.GlobalConfigPath(root), cfg); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"list"}, {"list", "--json"}, {"get", "remote"}, {"get", "local"}} {
		var out strings.Builder
		if _, err := runMCPCommand(context.Background(), append([]string{"mcp"}, args...), "test", &out); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if strings.Contains(out.String(), "synthetic-review-secret") || strings.Contains(out.String(), "pw@") {
			t.Fatalf("%v leaked a secret:\n%s", args, out.String())
		}
		if args[0] == "list" && !strings.Contains(out.String(), "off") {
			t.Fatalf("%v hides disabled entry:\n%s", args, out.String())
		}
	}
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://example.test/mcp":                                   "https://example.test/mcp",
		"https://u:p@example.test/mcp?key=s":                         "https://*****@example.test/mcp?*****",
		"https://${U}:${P}@example.test/mcp?key=literal-secret#frag": "https://*****@example.test/mcp?*****",
		"${BASE}/mcp?token=literal-secret":                           "${BASE}/mcp?*****",
		"https://example.test/a@b":                                   "https://example.test/a@b",
	} {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMCPCommandFailsOnMalformedConfig(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ZUT_HOME", root)
	if err := os.WriteFile(mcp.GlobalConfigPath(root), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"list"}, {"list", "--json"}, {"status"}} {
		var out strings.Builder
		if _, err := runMCPCommand(context.Background(), append([]string{"mcp"}, args...), "test", &out); err == nil {
			t.Fatalf("%v succeeded on malformed config:\n%s", args, out.String())
		}
	}
}

func TestParseArgsNoMCP(t *testing.T) {
	args, err := ParseArgs([]string{"--no-mcp", "-p", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if !args.NoMCP || mcpAllowed(args) {
		t.Fatalf("--no-mcp not applied: %+v", args.NoMCP)
	}
}

func keys(reg core.Registry) []string {
	out := make([]string, 0, len(reg))
	for name := range reg {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}
