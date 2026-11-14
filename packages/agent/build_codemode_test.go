package agent

import (
	"context"
	"encoding/json"
	"github.com/bnema/zut/packages/provider"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

func TestCodemodeRegistrationPolicy(t *testing.T) {
	enabled := true
	cfg := Config{Experimental: &ExperimentalConfig{Codemode: &enabled}, PythonInterpreter: "/configured/python"}
	for _, tc := range []struct {
		name string
		args Args
		want bool
	}{
		{"main", Args{}, true},
		{"explicit", Args{ToolsSet: true, Tools: []string{"codemode"}}, true},
		{"excluded", Args{Tools: []string{"python"}}, false},
		{"empty selection", Args{ToolsSet: true}, false},
		{"no tools", Args{NoTools: true}, false},
		{"permissions", Args{PermissionSet: &tools.PermissionSet{}}, false},
		{"orchestrator", Args{Orchestrate: true}, false},
		{"resident", Args{ResidentChild: true}, false},
		{"orchestrator resident", Args{Orchestrate: true, ResidentChild: true}, false},
		{"packaged name", Args{AgentName: "packaged"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := core.Registry{}
			sandbox := tools.NewSandbox(t.TempDir())
			tc.args.Env = []string{"SESSION=value"}
			got := maybeAddCodemodeTool(reg, tc.args, sandbox.Root, sandbox, cfg)
			_, present := reg["codemode"]
			if got != tc.want || present != tc.want {
				t.Fatalf("registered=%v present=%v want %v", got, present, tc.want)
			}
			if got {
				tool, ok := reg["codemode"].(*tools.CodemodeTool)
				if !ok || tool.SnippetHome == nil || tool.SnippetHome() != ZutHome() || tool.CWD != sandbox.Root || tool.Sandbox != sandbox || tool.Interpreter != cfg.PythonInterpreter || !reflect.DeepEqual(tool.Env, tc.args.Env) {
					t.Fatalf("tool=%#v", reg["codemode"])
				}
			}
		})
	}
	if maybeAddCodemodeTool(nil, Args{}, "", nil, cfg) {
		t.Fatal("nil registry registered")
	}
}

func TestCodemodeResolveRegistration(t *testing.T) {
	project := pythonTestEnv(t)
	args := pythonTestArgs(project)
	resolved := resolvePython(t, args)
	if _, ok := resolved.ToolRegistry["codemode"]; ok {
		t.Fatal("codemode enabled by default")
	}
	enabled := true
	cfg := Config{Experimental: &ExperimentalConfig{Codemode: &enabled}, PythonInterpreter: "/missing/python"}
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	resolved = resolvePython(t, args)
	cm, ok := resolved.ToolRegistry["codemode"].(*tools.CodemodeTool)
	if !ok || cm.Interpreter != cfg.PythonInterpreter || cm.CWD != project || cm.Sandbox != resolved.Sandbox {
		t.Fatalf("codemode=%#v", resolved.ToolRegistry["codemode"])
	}
	for _, tc := range []struct {
		name string
		args Args
	}{
		{"permissions", func() Args { a := args; a.PermissionSet = &tools.PermissionSet{}; return a }()},
		{"orchestrator", func() Args { a := args; a.Orchestrate = true; return a }()},
		{"resident", func() Args { a := args; a.ResidentChild = true; return a }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := resolvePython(t, tc.args)
			if _, ok := r.ToolRegistry["codemode"]; ok {
				t.Fatal("codemode registered")
			}
		})
	}
}

func TestCodemodeExtensionCannotBypassOptIn(t *testing.T) {
	project := pythonTestEnv(t)
	resolved := resolvePython(t, pythonTestArgs(project))
	resolved.MergeExtensionTools(pythonExtStub{infos: []ExtensionToolInfo{
		{Extension: "x", Name: "codemode"},
		{Extension: "x", Name: "weather"},
	}})
	if _, ok := resolved.ToolRegistry["codemode"]; ok {
		t.Fatal("extension bypassed codemode opt-in")
	}
	if _, ok := resolved.ToolRegistry["weather"]; !ok {
		t.Fatal("unrelated extension missing")
	}
}

type codemodeSandboxClient struct{ sent bool }

func (*codemodeSandboxClient) Name() string { return "codemode-sandbox-test" }
func (c *codemodeSandboxClient) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	events := make(chan provider.Event, 1)
	message := provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "done"}}}
	stop := provider.StopEnd
	if !c.sent {
		c.sent = true
		message.Content = []provider.Content{provider.ToolCallBlock{ID: "codemode-jail", Name: "codemode", Arguments: json.RawMessage(`{"code":"raise RuntimeError('must not spawn')","timeout":1}`)}}
		stop = provider.StopToolUse
	}
	events <- provider.EventDone{Stop: stop, Message: message}
	close(events)
	return events, nil
}

func TestCodemodeUseSandboxDeniesBeforeSpawn(t *testing.T) {
	project := pythonTestEnv(t)
	enabled := true
	if err := SaveConfig(Config{Experimental: &ExperimentalConfig{Codemode: &enabled}, PythonInterpreter: "/does-not-exist/must-not-probe"}); err != nil {
		t.Fatal(err)
	}
	args := pythonTestArgs(project)
	args.Env = []string{}
	resolved := resolvePython(t, args)
	cm := resolved.ToolRegistry["codemode"].(*tools.CodemodeTool)
	if cm.Sandbox.Locked() {
		t.Fatal("expected fresh unlocked sandbox")
	}
	shared := tools.NewSandbox(project)
	shared.Lock()
	resolved.UseSandbox(shared)
	if cm.Sandbox != shared || resolved.Sandbox != shared {
		t.Fatal("codemode did not rebind to live sandbox")
	}
	agent := core.NewAgent(&codemodeSandboxClient{}, "test", "", resolved.ToolRegistry)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	found := false
	if err := agent.Prompt(ctx, "run", nil, func(event core.AgentEvent) {
		if result, ok := event.(core.EvToolResult); ok && result.ID == "codemode-jail" {
			found = true
			if !result.Result.IsError || len(result.Result.Content) == 0 {
				t.Errorf("not denied: %+v", result.Result)
				return
			}
			text := result.Result.Content[0].(provider.TextBlock).Text
			if !strings.Contains(text, "jailed: arbitrary Python cannot be confined") || strings.Contains(text, "interpreter") {
				t.Errorf("expected admission denial before interpreter resolution: %s", text)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("missing codemode tool result")
	}
}

func TestCodemodeConnectorHintsWiring(t *testing.T) {
	project := setupMCPHome(t, fakeMCPHTTPServer(t).URL)
	args := pythonTestArgs(project)
	args.Env = os.Environ()
	startTestMCP(t, &args)
	yes := true
	reg := core.Registry{}
	if !maybeAddCodemodeTool(reg, args, project, nil, Config{Experimental: &ExperimentalConfig{Codemode: &yes}}) {
		t.Fatal("not registered")
	}
	cm := reg["codemode"].(*tools.CodemodeTool)
	if cm.MCPServers == nil || cm.MCPServers()["fake"] < 1 || !strings.Contains(cm.Description(), "MCP servers: fake (") {
		t.Fatal(cm.Description())
	}
	args.Tools = []string{"codemode", "mcp__fake__lookup"}
	reg = core.Registry{}
	maybeAddCodemodeTool(reg, args, project, nil, Config{Experimental: &ExperimentalConfig{Codemode: &yes}})
	if got := reg["codemode"].(*tools.CodemodeTool).MCPServers()["fake"]; got != 1 {
		t.Fatal(got)
	}
}
