package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

func pythonTestEnv(t *testing.T) (project string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("ZUT_HOME", filepath.Join(root, "zut-home"))
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("USERPROFILE", filepath.Join(root, "home"))
	project = filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	return project
}

func pythonTestArgs(project string) Args {
	return Args{
		Provider:       "ollama",
		Model:          "any-local-model",
		CWD:            project,
		NoLSP:          true,
		NoSkill:        true,
		NoContextFiles: true,
	}
}

func resolvePython(t *testing.T, args Args) Resolved {
	t.Helper()
	resolved, err := Resolve(args, false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return resolved
}

func pythonToolFrom(t *testing.T, r Resolved) *tools.PythonTool {
	t.Helper()
	tool, ok := r.ToolRegistry["python"]
	if !ok {
		t.Fatal("python not registered")
	}
	py, ok := tool.(*tools.PythonTool)
	if !ok {
		t.Fatalf("python = %T, want *tools.PythonTool", tool)
	}
	return py
}

func TestPythonRegisteredByDefault(t *testing.T) {
	project := pythonTestEnv(t)
	resolved := resolvePython(t, pythonTestArgs(project))
	py := pythonToolFrom(t, resolved)
	if py.CWD != project {
		t.Fatalf("python CWD = %q, want %q", py.CWD, project)
	}
	if py.Interpreter != "" {
		t.Fatalf("python Interpreter = %q, want empty default", py.Interpreter)
	}
	if py.Sandbox != resolved.Sandbox {
		t.Fatal("python must share the live sandbox pointer")
	}
}

func TestPythonExplicitEmptySelectionDenied(t *testing.T) {
	project := pythonTestEnv(t)
	args := pythonTestArgs(project)
	args.ToolsSet = true
	args.Tools = []string{}
	resolved := resolvePython(t, args)
	if _, ok := resolved.ToolRegistry["python"]; ok {
		t.Fatal("explicit empty selection must not enable python")
	}
	// Other tools retain their historical empty-list behavior.
	if _, ok := resolved.ToolRegistry["bash"]; !ok {
		t.Fatal("explicit empty selection must preserve other tools' behavior")
	}
}

func TestPythonExplicitInclusionExclusion(t *testing.T) {
	project := pythonTestEnv(t)
	included := pythonTestArgs(project)
	included.Tools = []string{"python"}
	resolved := resolvePython(t, included)
	if _, ok := resolved.ToolRegistry["python"]; !ok {
		t.Fatal("explicit python selection must register python")
	}
	if _, ok := resolved.ToolRegistry["bash"]; ok {
		t.Fatal("explicit python-only selection must not register bash")
	}

	excluded := pythonTestArgs(project)
	excluded.Tools = []string{"bash"}
	resolved = resolvePython(t, excluded)
	if _, ok := resolved.ToolRegistry["python"]; ok {
		t.Fatal("bash-only selection must not register python")
	}
	if _, ok := resolved.ToolRegistry["bash"]; !ok {
		t.Fatal("bash-only selection must register bash")
	}
}

func TestPythonNoToolsAndPackagedDenial(t *testing.T) {
	project := pythonTestEnv(t)
	args := pythonTestArgs(project)
	args.NoTools = true
	if resolved := resolvePython(t, args); len(resolved.ToolRegistry) != 0 {
		t.Fatalf("NoTools registry = %d tools, want empty", len(resolved.ToolRegistry))
	}

	args = pythonTestArgs(project)
	args.PermissionSet = &tools.PermissionSet{}
	resolved := resolvePython(t, args)
	if _, ok := resolved.ToolRegistry["python"]; ok {
		t.Fatal("packaged agents must not receive python")
	}

	// SDK-shaped selections: nil/empty without provenance keeps the default.
	if !pythonToolAllowed(Args{}) {
		t.Fatal("omitted selection must allow python")
	}
	if pythonToolAllowed(Args{ToolsSet: true}) {
		t.Fatal("explicit empty selection must deny python")
	}
	if !pythonToolAllowed(Args{Tools: []string{"bash", "python"}}) {
		t.Fatal("explicit inclusion must allow python")
	}
}

func TestPythonSummaryPresenceAbsence(t *testing.T) {
	project := pythonTestEnv(t)
	resolved := resolvePython(t, pythonTestArgs(project))
	found := false
	for _, s := range resolved.ToolSummary {
		if s.Name == "python" {
			found = true
		}
	}
	if !found {
		t.Fatal("default summary must advertise python")
	}

	args := pythonTestArgs(project)
	args.Tools = []string{"bash"}
	resolved = resolvePython(t, args)
	for _, s := range resolved.ToolSummary {
		if s.Name == "python" {
			t.Fatal("bash-only summary must not advertise python")
		}
	}
}

func TestPythonConfigRoundTripAndAssembly(t *testing.T) {
	project := pythonTestEnv(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.PythonInterpreter = "/opt/py/bin/python"
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.PythonInterpreter != "/opt/py/bin/python" {
		t.Fatalf("config round-trip = %q", loaded.PythonInterpreter)
	}
	resolved := resolvePython(t, pythonTestArgs(project))
	if got := pythonToolFrom(t, resolved).Interpreter; got != "/opt/py/bin/python" {
		t.Fatalf("assembled interpreter = %q", got)
	}
}

func TestPythonRebuildUsesCurrentCWDAndSandbox(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ZUT_HOME", filepath.Join(root, "zut-home"))
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("USERPROFILE", filepath.Join(root, "home"))
	dirA := filepath.Join(root, "a")
	dirB := filepath.Join(root, "b")
	if err := os.MkdirAll(dirA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirB, 0o755); err != nil {
		t.Fatal(err)
	}
	args := pythonTestArgs(dirA)
	first := resolvePython(t, args)
	if pythonToolFrom(t, first).CWD != dirA {
		t.Fatal("first build must use its own CWD")
	}

	// A normal rebuild picks up the current CWD rather than stale state,
	// and UseSandbox keeps the live jail state across rebuilds.
	args.CWD = dirB
	second := resolvePython(t, args)
	if pythonToolFrom(t, second).CWD != dirB {
		t.Fatalf("rebuild CWD = %q, want %q", pythonToolFrom(t, second).CWD, dirB)
	}
	fresh := tools.NewSandbox(dirB)
	fresh.Lock()
	second.UseSandbox(fresh)
	if got := pythonToolFrom(t, second).Sandbox; got != fresh {
		t.Fatal("UseSandbox must propagate the live sandbox to python")
	}
	if !pythonToolFrom(t, second).Sandbox.Locked() {
		t.Fatal("python must observe the live jail state")
	}
}

type pythonExtStub struct {
	infos []ExtensionToolInfo
}

func (s pythonExtStub) Tools() []ExtensionToolInfo { return s.infos }

func (s pythonExtStub) NewExtensionTool(info ExtensionToolInfo) core.Tool {
	return &tools.GrepTool{}
}

func TestPythonExtensionCannotBypassPolicyExclusion(t *testing.T) {
	project := pythonTestEnv(t)
	args := pythonTestArgs(project)
	args.Tools = []string{"bash"}
	resolved := resolvePython(t, args)
	stub := pythonExtStub{infos: []ExtensionToolInfo{
		{Extension: "x", Name: "python"},
		{Extension: "x", Name: "weather"},
	}}
	resolved.MergeExtensionTools(stub)
	if _, ok := resolved.ToolRegistry["python"]; ok {
		t.Fatal("an extension must not reintroduce a policy-excluded python")
	}
	if _, ok := resolved.ToolRegistry["weather"]; !ok {
		t.Fatal("unrelated extension tools must still merge")
	}
}

func TestPythonUnavailableInterpreterDoesNotBlockStartup(t *testing.T) {
	project := pythonTestEnv(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.PythonInterpreter = filepath.Join(project, "missing", "python")
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	// Startup succeeds; the missing interpreter fails only on invocation
	// with an actionable error (covered at the tool boundary).
	resolved := resolvePython(t, pythonTestArgs(project))
	pythonToolFrom(t, resolved)
}
