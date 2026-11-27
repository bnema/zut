package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/config"
	"github.com/bnema/zut/packages/agent/mcp"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"sync/atomic"
)

func TestMCPDeferredRegistration(t *testing.T) {
	project := setupMCPHome(t, fakeMCPHTTPServer(t).URL)
	args := pythonTestArgs(project)
	args.Env = os.Environ()
	startTestMCP(t, &args)
	yes, no := true, false
	for _, tc := range []struct {
		name         string
		experimental *config.ExperimentalConfig
		edit         func(*Args)
		deferred     bool
	}{
		{"default", nil, nil, false},
		{"enabled", &config.ExperimentalConfig{Codemode: &yes}, nil, true},
		{"opt out", &config.ExperimentalConfig{Codemode: &yes, CodemodeDeferMCP: &no}, nil, false},
		{"codemode excluded", &config.ExperimentalConfig{Codemode: &yes}, func(a *Args) { a.Tools = []string{"mcp"} }, false},
		{"resident", &config.ExperimentalConfig{Codemode: &yes}, func(a *Args) { a.ResidentChild = true }, false},
		{"orchestrator", &config.ExperimentalConfig{Codemode: &yes}, func(a *Args) { a.Orchestrate = true }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := config.Save(config.Config{Experimental: tc.experimental}); err != nil {
				t.Fatal(err)
			}
			next := args
			if tc.edit != nil {
				tc.edit(&next)
			}
			resolved := resolvePython(t, next)
			for _, spec := range resolved.ToolRegistry.Specs() {
				if mcp.IsMCPToolName(spec.Name) && spec.Deferred != tc.deferred {
					t.Fatalf("%s deferred=%v", spec.Name, spec.Deferred)
				}
			}
			tool, err := resolved.ToolRegistry.Get("mcp__fake__lookup")
			if err != nil || !mcp.IsTool(tool) || !mcp.IsReadOnlyTool(tool) {
				t.Fatalf("lookup %T %v", tool, err)
			}
			prompt := resolved.SystemPrompt
			if strings.Contains(prompt, "MCP tools are not directly callable") != tc.deferred || !strings.Contains(prompt, "Use lookup before deploy.") || strings.Count(prompt, "MCP tools are not directly callable") > 1 {
				t.Fatal(prompt)
			}
			// Inspect actual OpenAI request definitions using the provider's normal
			// request path; deferred MCP definitions must not be advertised.
			var called atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called.Store(true)
				var body struct {
					Tools []struct {
						Function struct {
							Name string `json:"name"`
						} `json:"function"`
					} `json:"tools"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				present := false
				for _, tool := range body.Tools {
					if tool.Function.Name == "mcp__fake__lookup" {
						present = true
					}
				}
				if present == tc.deferred {
					t.Errorf("provider MCP present=%v deferred=%v", present, tc.deferred)
				}
				w.WriteHeader(http.StatusBadRequest)
			}))
			defer server.Close()
			client := provider.NewOpenAI("test-key", server.URL)
			_, _ = client.Stream(context.Background(), provider.Request{Model: "test", Tools: resolved.ToolRegistry.Specs()})
			if !called.Load() {
				t.Fatal("provider was not called")
			}
		})
	}
}

func TestCodemodeDeferredMCPDiscoveryAndCall(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 absent")
	}
	project := setupMCPHome(t, fakeMCPHTTPServer(t).URL)
	args := pythonTestArgs(project)
	args.Env = os.Environ()
	startTestMCP(t, &args)
	yes := true
	if err := config.Save(config.Config{Experimental: &config.ExperimentalConfig{Codemode: &yes}, PythonInterpreter: py}); err != nil {
		t.Fatal(err)
	}
	resolved := resolvePython(t, args)
	// Public Agent invocation provides the real NestedInvoker to codemode.
	code := `assert any(t["name"] == "mcp__fake__lookup" for t in zut.search("lookup"))
r = zut.call("mcp__fake__lookup")
assert not r.is_error
print(r.text)
zut.result("deferred MCP called")`
	raw, _ := json.Marshal(map[string]any{"code": code, "timeout": 10})
	agent := core.NewAgent(&deferredMCPClient{args: raw}, "test", "", resolved.ToolRegistry)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	found := false
	if err := agent.Prompt(ctx, "run", nil, func(event core.AgentEvent) {
		if e, ok := event.(core.EvToolResult); ok && e.ID == "cm" {
			found = true
			text := e.Result.Content[0].(provider.TextBlock).Text
			if e.Result.IsError || !strings.Contains(text, `result: "deferred MCP called"`) {
				t.Error(text)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("no result")
	}
}

type deferredMCPClient struct {
	args json.RawMessage
	sent bool
}

func (*deferredMCPClient) Name() string { return "deferred-mcp-test" }
func (c *deferredMCPClient) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
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

func TestMCPDeferredFollowsLiveJail(t *testing.T) {
	project := setupMCPHome(t, fakeMCPHTTPServer(t).URL)
	args := pythonTestArgs(project)
	args.Env = os.Environ()
	startTestMCP(t, &args)
	yes := true
	if err := config.Save(config.Config{Experimental: &config.ExperimentalConfig{Codemode: &yes}, JailByDefault: &yes}); err != nil {
		t.Fatal(err)
	}
	resolved := resolvePython(t, args)
	check := func(want bool) {
		t.Helper()
		found := false
		for _, spec := range resolved.ToolRegistry.Specs() {
			if spec.Name == "mcp__fake__lookup" {
				found = true
				if spec.Deferred != want {
					t.Fatalf("deferred=%v want %v", spec.Deferred, want)
				}
			}
		}
		if !found {
			t.Fatal("MCP missing")
		}
	}
	check(false) // jailed startup must keep the direct route.
	prompt := resolved.SystemPrompt
	if !strings.Contains(prompt, "When codemode is available") || !strings.Contains(prompt, "Otherwise MCP tools remain directly callable") {
		t.Fatal(prompt)
	}
	resolved.Sandbox.Unlock()
	check(true)
	// Rebinding must consult the shared live sandbox, not the Resolve sandbox.
	shared := tools.NewSandbox(project)
	shared.Lock()
	resolved.UseSandbox(shared)
	check(false)
	shared.Unlock()
	check(true)
	shared.Lock()
	check(false)
	shared.Unlock()
	check(true)
	shared.Permissions = &tools.PermissionSet{}
	check(false)
	if got := codemodeSystemAddendum(resolved.ToolRegistry); !strings.Contains(prompt, got) {
		t.Fatalf("conditional prompt changed: %s", got)
	}
}
