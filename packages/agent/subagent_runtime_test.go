package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestExpandWebCapabilityTools(t *testing.T) {
	catalogue := append([]string{"read"}, tools.WebCapabilityNames...)
	got := expandWebCapabilityTools([]string{"read", "web_search"}, catalogue, func(string) bool { return true })
	for _, name := range append([]string{"read"}, tools.WebCapabilityNames...) {
		found := false
		for _, candidate := range got {
			if candidate == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expanded child tools = %v, missing %s", got, name)
		}
	}
}

func TestExpandWebCapabilityToolsPreservesPolicyAndNonWebSelections(t *testing.T) {
	catalogue := append([]string{"read"}, tools.WebCapabilityNames...)
	partial := expandWebCapabilityTools([]string{"web_open"}, catalogue, func(string) bool { return true })
	for _, name := range tools.WebCapabilityNames {
		found := false
		for _, candidate := range partial {
			if candidate == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("partial web selection did not expand: %v", partial)
		}
	}
	denied := expandWebCapabilityTools([]string{"read", "web_search"}, catalogue, func(name string) bool { return name == "read" })
	if len(denied) != 1 || denied[0] != "read" {
		t.Fatalf("denied web capability = %v, want [read]", denied)
	}
	nonWeb := expandWebCapabilityTools([]string{"read"}, catalogue, func(string) bool { return true })
	if len(nonWeb) != 1 || nonWeb[0] != "read" {
		t.Fatalf("non-web selection changed to %v", nonWeb)
	}
}

func TestResidentChildSpecSnapshotsCurrentProviderTransportSettings(t *testing.T) {
	runtime := newSubagentRuntime(subagentRuntimeConfig{
		Args: Args{}, Root: t.TempDir(), RepoRoot: t.TempDir(),
		Provider: "openai", Model: "gpt-5.6-sol", BaseURL: "https://old.example/v1", InsecureTLS: false, ContextWindow: 500_000,
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	runtime.SetProviderSettings("https://current.example/v1", true)
	runtime.SetModel("gpt-current")
	spec, err := runtime.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{Task: "review"}, core.Registry{"read": nil})
	if err != nil {
		t.Fatal(err)
	}
	if spec.BaseURL != "https://current.example/v1" || !spec.InsecureTLS || spec.Model != "gpt-current" {
		t.Fatalf("resident spec transport/model = %q insecure=%t model=%q", spec.BaseURL, spec.InsecureTLS, spec.Model)
	}
}

func TestResidentChildSpecTracksFastMode(t *testing.T) {
	runtime := newSubagentRuntime(subagentRuntimeConfig{
		Args: Args{}, Root: t.TempDir(), RepoRoot: t.TempDir(),
		Provider: "openai", Model: "gpt-5.6-sol", FastMode: false,
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	spec, err := runtime.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{Task: "review"}, core.Registry{"read": nil})
	if err != nil {
		t.Fatal(err)
	}
	if spec.FastMode {
		t.Fatal("initial resident fast mode = true, want false")
	}
	runtime.SetFastMode(true)
	spec, err = runtime.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{Task: "review"}, core.Registry{"read": nil})
	if err != nil {
		t.Fatal(err)
	}
	if !spec.FastMode {
		t.Fatal("resident fast mode = false after update, want true")
	}
}

func TestResidentChildSpecCarriesNoCumulativeBudget(t *testing.T) {
	runtime := newSubagentRuntime(subagentRuntimeConfig{
		Args: Args{}, Root: t.TempDir(), RepoRoot: t.TempDir(),
		Provider: "openai", Model: "gpt-5.6-sol", ContextWindow: 42_000,
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	spec, err := runtime.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{Task: "review"}, core.Registry{"read": nil})
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Tools) != 1 || spec.Tools[0] != "read" {
		t.Fatalf("child spec tools = %#v, want resolved tool list", spec)
	}
	// The budget fields are gone from the spec struct; the serialized
	// form must carry no budget bytes either.
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "budget") {
		t.Fatalf("child spec carries budget = %s", encoded)
	}
}

func TestResidentChildSpecDoesNotCarryTransportAcrossProviderOverride(t *testing.T) {
	runtime := newSubagentRuntime(subagentRuntimeConfig{
		Args: Args{}, Root: t.TempDir(), RepoRoot: t.TempDir(),
		Provider: "openai", Model: "gpt-5.6-sol", BaseURL: "https://parent.example/v1", InsecureTLS: true, ContextWindow: 500_000,
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	spec, err := runtime.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{
		Task: "review", Provider: "anthropic", Model: "claude-sonnet-4-5",
	}, core.Registry{"read": nil})
	if err != nil {
		t.Fatal(err)
	}
	if spec.BaseURL != "" || spec.InsecureTLS {
		t.Fatalf("cross-provider spec inherited transport: %#v", spec)
	}
}

func TestResidentChildSpecOmitsParentOnlyTools(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	cwd := t.TempDir()
	runtime := newSubagentRuntime(subagentRuntimeConfig{
		Args:     Args{CWD: cwd, NoSkill: true, NoContextFiles: true, NoLSP: true},
		Root:     t.TempDir(),
		RepoRoot: cwd,
		Provider: "openai", Model: "gpt-5.6-sol",
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	parent := core.Registry{
		"read":           nil,
		"schedule":       nil,
		"host-only-tool": nil,
		"subagent":       nil,
		"update_goal":    nil,
		"plan":           nil,
	}
	spec, err := runtime.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{Task: "review"}, parent)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(spec.Tools, "schedule") || slices.Contains(spec.Tools, "host-only-tool") {
		t.Fatalf("generic child inherited parent-only tools: %v", spec.Tools)
	}
	if !slices.Contains(spec.Tools, "read") {
		t.Fatalf("generic child lost supported tool: %v", spec.Tools)
	}
	for _, name := range []string{"subagent", "update_goal", "plan"} {
		if slices.Contains(spec.Tools, name) {
			t.Fatalf("generic child kept forbidden tool %q: %v", name, spec.Tools)
		}
	}
	resolved, err := Resolve(residentChildArgs(runtime.args, runtime.credentialProvider, spec), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := residentChildRegistry(resolved.ToolRegistry, spec.Tools); err != nil {
		t.Fatalf("generic spec failed child registry validation: %v (tools=%v)", err, spec.Tools)
	}
}

func TestResidentChildSpecProfileToolDeclarations(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	cwd := t.TempDir()
	newRuntime := func(policy subagents.SubagentPolicy) *subagentRuntime {
		rt := newSubagentRuntime(subagentRuntimeConfig{
			Args:     Args{CWD: cwd, NoSkill: true, NoContextFiles: true, NoLSP: true},
			Root:     t.TempDir(),
			RepoRoot: cwd,
			Provider: "openai", Model: "gpt-5.6-sol",
			Policy: policy,
		})
		t.Cleanup(func() { _ = rt.Close(context.Background()) })
		return rt
	}
	parent := core.Registry{"read": nil, "bash": nil, "schedule": nil}

	t.Run("omitted inherits like generic", func(t *testing.T) {
		rt := newRuntime(subagents.SubagentPolicy{})
		generic, err := rt.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{Task: "review"}, parent)
		if err != nil {
			t.Fatal(err)
		}
		withProfile, err := rt.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{
			Task:    "review",
			Profile: &subagents.Profile{Name: "generic"},
		}, parent)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(generic.Tools, withProfile.Tools) {
			t.Fatalf("omitted profile tools = %v, want generic %v", withProfile.Tools, generic.Tools)
		}
	})

	t.Run("explicit keeps only supported permitted names", func(t *testing.T) {
		rt := newRuntime(subagents.SubagentPolicy{})
		spec, err := rt.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{
			Task: "review",
			Profile: &subagents.Profile{
				Name: "reviewer", ToolsDeclared: true,
				Tools: []string{"read", "schedule", "host-only-tool", "missing"},
			},
		}, parent)
		if err != nil {
			t.Fatal(err)
		}
		if len(spec.Tools) != 1 || spec.Tools[0] != "read" {
			t.Fatalf("explicit profile tools = %v, want [read]", spec.Tools)
		}
	})

	t.Run("explicit empty stays empty", func(t *testing.T) {
		rt := newRuntime(subagents.SubagentPolicy{})
		spec, err := rt.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{
			Task:    "review",
			Profile: &subagents.Profile{Name: "empty", ToolsDeclared: true, Tools: []string{}},
		}, parent)
		if err != nil {
			t.Fatal(err)
		}
		if len(spec.Tools) != 0 {
			t.Fatalf("explicit empty profile tools = %v, want []", spec.Tools)
		}
	})

	t.Run("policy denials hold", func(t *testing.T) {
		rt := newRuntime(subagents.SubagentPolicy{AllowedTools: []string{"read"}})
		spec, err := rt.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{Task: "review"}, parent)
		if err != nil {
			t.Fatal(err)
		}
		if len(spec.Tools) != 1 || spec.Tools[0] != "read" {
			t.Fatalf("policy-restricted tools = %v, want [read]", spec.Tools)
		}
		explicit, err := rt.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{
			Task: "review",
			Profile: &subagents.Profile{
				Name: "reviewer", ToolsDeclared: true,
				Tools: []string{"read", "bash"},
			},
		}, parent)
		if err != nil {
			t.Fatal(err)
		}
		if len(explicit.Tools) != 1 || explicit.Tools[0] != "read" {
			t.Fatalf("policy-restricted explicit tools = %v, want [read]", explicit.Tools)
		}
	})
}

func TestResidentChildSpecNeverGrantsToolsOutsideParent(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	cwd := t.TempDir()
	runtime := newSubagentRuntime(subagentRuntimeConfig{
		Args:     Args{CWD: cwd, NoSkill: true, NoContextFiles: true, NoLSP: true},
		Root:     t.TempDir(),
		RepoRoot: cwd,
		Provider: "openai", Model: "gpt-5.6-sol",
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	spec, err := runtime.buildResidentChildSpec(context.Background(),
		tools.ResidentSpawnRequest{Task: "review"}, core.Registry{"read": nil})
	if err != nil {
		t.Fatal(err)
	}
	// bash/write exist in the resolved child runtime but were never offered
	// by the parent catalogue; the child must not receive them.
	for _, name := range []string{"bash", "write", "schedule"} {
		if slices.Contains(spec.Tools, name) {
			t.Fatalf("child gained tool %q outside parent catalogue: %v", name, spec.Tools)
		}
	}
	explicit, err := runtime.buildResidentChildSpec(context.Background(),
		tools.ResidentSpawnRequest{
			Task: "review",
			Profile: &subagents.Profile{
				Name: "reviewer", ToolsDeclared: true,
				Tools: []string{"read", "bash"},
			},
		}, core.Registry{"read": nil})
	if err != nil {
		t.Fatal(err)
	}
	if len(explicit.Tools) != 1 || explicit.Tools[0] != "read" {
		t.Fatalf("explicit profile gained tool outside parent catalogue: %v", explicit.Tools)
	}
}

func TestResidentChildSpecDisabledSkillsDoNotLeak(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	cwd := t.TempDir()
	runtime := newSubagentRuntime(subagentRuntimeConfig{
		Args:     Args{CWD: cwd, NoSkill: true, NoContextFiles: true, NoLSP: true},
		Root:     t.TempDir(),
		RepoRoot: cwd,
		Provider: "openai", Model: "gpt-5.6-sol",
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	inherit := false
	parent := core.Registry{"read": nil, "skill": nil}
	spec, err := runtime.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{
		Task:    "review",
		Profile: &subagents.Profile{Name: "noskill", InheritSkills: &inherit},
	}, parent)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(spec.Tools, "skill") {
		t.Fatalf("skill-disabled child inherited skill tool: %v", spec.Tools)
	}
	resolved, err := Resolve(residentChildArgs(runtime.args, runtime.credentialProvider, spec), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resolved.ToolRegistry["skill"]; ok {
		t.Fatalf("skill-disabled child registry contains skill tool")
	}
	if _, err := residentChildRegistry(resolved.ToolRegistry, spec.Tools); err != nil {
		t.Fatalf("skill-disabled spec failed validation: %v", err)
	}
}

// TestInjectToolsRegistersOneSubagentFacade pins the registry contract: the four
// legacy tools collapse into exactly one facade entry whose non-nil fields are
// the granted actions.
func TestInjectToolsRegistersOneSubagentFacade(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	cwd := t.TempDir()
	cwdArgs := func() Args {
		return Args{CWD: cwd, NoSkill: true, NoContextFiles: true, NoLSP: true}
	}
	cases := []struct {
		name                                   string
		args                                   Args
		spawn, status, stop, resume, interrupt bool
	}{
		{name: "default grants every action", args: cwdArgs(), spawn: true, status: true, stop: true, resume: true, interrupt: true},
		{name: "resume suffix grants resume only", args: func() Args {
			args := cwdArgs()
			args.ToolsSet = true
			args.Tools = []string{"subagent:resume"}
			return args
		}(), resume: true},
		{name: "interrupt suffix grants interrupt only", args: func() Args {
			args := cwdArgs()
			args.ToolsSet = true
			args.Tools = []string{"subagent:interrupt"}
			return args
		}(), interrupt: true},
	}
	legacy := []string{"subagent_spawn", "subagent_status", "subagent_stop", "subagent_resume"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runtime := newSubagentRuntime(subagentRuntimeConfig{
				Args:     tc.args,
				Root:     t.TempDir(),
				RepoRoot: cwd,
				Provider: "openai", Model: "gpt-5.6-sol",
			})
			t.Cleanup(func() { _ = runtime.Close(context.Background()) })
			registry := runtime.InjectTools(core.Registry{})
			if len(registry) != 1 {
				t.Fatalf("injected registry has %d entries, want 1", len(registry))
			}
			tool, ok := registry[tools.SubagentToolName]
			if !ok {
				t.Fatalf("injected registry has no %q entry", tools.SubagentToolName)
			}
			facade, ok := tool.(*tools.SubagentTool)
			if !ok {
				t.Fatalf("registry entry %q = %T, want *tools.SubagentTool", tools.SubagentToolName, tool)
			}
			got := [5]bool{facade.Spawn != nil, facade.Status != nil, facade.Stop != nil, facade.Resume != nil, facade.Interrupt != nil}
			want := [5]bool{tc.spawn, tc.status, tc.stop, tc.resume, tc.interrupt}
			if got != want {
				t.Fatalf("facade actions = %v, want %v", got, want)
			}
			for _, name := range legacy {
				if _, ok := registry[name]; ok {
					t.Fatalf("injected registry still exposes legacy tool %q", name)
				}
			}
		})
	}
}

func TestSubagentRuntimeActiveSessionScope(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("ZUT_HOME", home)
	root, err := core.NewSession(home, cwd, "openai", "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "task"}}}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	branch := func(path, store, workspace string) *core.Session {
		t.Helper()
		path, err := core.BranchSessionHidden(path, store, workspace, "test", 1)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := core.ReadSessionSnapshot(path)
		if err != nil {
			t.Fatal(err)
		}
		return &core.Session{ID: snapshot.Meta.ID, Path: path, Meta: snapshot.Meta}
	}
	parent := branch(root.Path, agentSessionsRoot(home, Args{AgentName: "test"}), cwd)
	child := branch(parent.Path, home, t.TempDir())
	rt := &subagentRuntime{}
	rt.SetActiveSession(t.Context(), child)
	if got := rt.activeSessionScope(); !slices.Equal(got, []string{child.ID, parent.ID, root.ID}) {
		t.Fatalf("branch scope = %v", got)
	}
	rt.SetActiveSession(t.Context(), root)
	if got := rt.activeSessionScope(); !slices.Equal(got, []string{root.ID}) {
		t.Fatalf("root scope = %v", got)
	}
	rt.SetActiveSession(t.Context(), nil)
	if got := rt.activeSessionScope(); len(got) != 0 {
		t.Fatalf("cleared scope = %v", got)
	}
}

func TestSubagentRuntimeNoSessionScope(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	root := t.TempDir()
	newRuntime := func() *subagentRuntime {
		rt := newSubagentRuntime(subagentRuntimeConfig{
			Args: Args{NoSess: true, NoSkill: true, NoContextFiles: true, NoLSP: true},
			Root: root, RepoRoot: root, Provider: "openai", Model: "test",
		})
		t.Cleanup(func() { _ = rt.Close(context.Background()) })
		return rt
	}
	check := func(rt *subagentRuntime) string {
		t.Helper()
		facade := rt.InjectTools(core.Registry{})[tools.SubagentToolName].(*tools.SubagentTool)
		rt.SetActiveSession(t.Context(), nil)
		spec, err := rt.buildResidentChildSpec(t.Context(), tools.ResidentSpawnRequest{Task: "review"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(spec.ParentSessionID, "runtime:") || spec.RootCacheID != "" {
			t.Fatalf("parent = %q, root cache = %q", spec.ParentSessionID, spec.RootCacheID)
		}
		if got := facade.Status.SessionScope(); !slices.Equal(got, []string{spec.ParentSessionID}) {
			t.Fatalf("status scope = %v, want [%s]", got, spec.ParentSessionID)
		}
		return spec.ParentSessionID
	}
	first := newRuntime()
	id := check(first)
	if check(first) != id {
		t.Fatal("scope changed within one runtime")
	}
	if check(newRuntime()) == id {
		t.Fatal("distinct runtimes share a scope")
	}
	second := newRuntime()
	secondID := check(second)
	for _, fixture := range []struct{ child, parent string }{{"first-worker", id}, {"second-worker", secondID}} {
		journal, err := subagents.OpenResidentJournal(root, fixture.child)
		if err != nil {
			t.Fatal(err)
		}
		spec := subagents.ResidentChildSpec{ID: fixture.child, SessionID: fixture.child, ParentSessionID: fixture.parent, InitialTurnID: "turn", Provider: "openai", Model: "test"}
		if err := journal.Accept(spec, "task"); err != nil {
			_ = journal.Close()
			t.Fatal(err)
		}
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		rt         *subagentRuntime
		own, other string
	}{{first, "first-worker", "second-worker"}, {second, "second-worker", "first-worker"}} {
		if errs := tc.rt.resident.Reconcile(); len(errs) != 0 {
			t.Fatal(errs)
		}
		facade := tc.rt.InjectTools(core.Registry{})[tools.SubagentToolName].(*tools.SubagentTool)
		for _, child := range []string{tc.own, tc.other} {
			raw, _ := json.Marshal(map[string]string{"action": "status", "agent_id": child})
			result, err := facade.Execute(t.Context(), raw, nil)
			if err != nil || result.IsError != (child == tc.other) {
				t.Fatalf("status %s: %#v, %v", child, result, err)
			}
		}
	}
}

func TestSubagentRuntimeKeepsSpawnedChildAcrossSessionSwitch(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	started := make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	manager := subagents.NewResidentManager(t.TempDir(), func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentRuntime, error) {
		return subagents.ResidentTurnRunner(func(ctx context.Context, _ string) error {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return ctx.Err()
		}), nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	rt := &subagentRuntime{resident: manager}
	rt.SetActiveSession(t.Context(), &core.Session{ID: "original"})
	facade := rt.InjectTools(core.Registry{})[tools.SubagentToolName].(*tools.SubagentTool)
	spec := subagents.ResidentChildSpec{ID: "worker", SessionID: "worker-session", ParentSessionID: "original", Provider: "openai", Model: "test", Required: true}
	if _, err := manager.Spawn(t.Context(), spec, "task"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("resident child did not start before deadline")
	}
	rt.SetActiveSession(t.Context(), &core.Session{ID: "unrelated"})
	if got := rt.activeSessionScope(); !slices.Equal(got, []string{"unrelated"}) {
		t.Fatalf("scope = %v", got)
	}
	for _, args := range []string{`{"action":"status","agent_id":"worker"}`, `{"action":"stop","agent_id":"worker"}`, `{"action":"status","agent_id":"worker"}`} {
		result, err := facade.Execute(ctx, json.RawMessage(args), nil)
		if err != nil || result.IsError {
			t.Fatalf("%s: %#v, %v", args, result, err)
		}
	}
	if unmet := manager.UnmetRequired(); len(unmet) != 1 || unmet[0].ID != spec.ID {
		t.Fatalf("unmet = %#v", unmet)
	}
	if errs := manager.Reconcile(); len(errs) != 0 {
		t.Fatal(errs)
	}
	result, err := facade.Execute(ctx, json.RawMessage(`{"action":"resume","agent_id":"worker","prompt":"continue"}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("resume after reconcile: %#v, %v", result, err)
	}
}

func TestResidentSessionScopeDeepHeaderChain(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	dir := core.SessionsDir(home, cwd)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var want []string
	writeHeader := func(id, parent, filename string) {
		t.Helper()
		header, err := json.Marshal(map[string]any{"type": "meta", "meta": core.SessionMeta{ID: id, Parent: parent, CWD: cwd}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, filename), append(header, '\n', '{', '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 128; i++ {
		id := fmt.Sprintf("ancestor-%03d", i)
		parent := ""
		if len(want) > 0 {
			parent = want[len(want)-1]
		}
		writeHeader(id, parent, id+".jsonl")
		want = append(want, id)
	}
	slices.Reverse(want)
	child := &core.Session{ID: "child", Meta: core.SessionMeta{Parent: want[0]}}
	if got := residentSessionScope(t.Context(), home, child); !slices.Equal(got, append([]string{child.ID}, want...)) {
		t.Fatalf("scope has %d IDs, want %d", len(got), 1+len(want))
	}
	writeHeader(want[1], want[2], "duplicate.jsonl")
	if got := residentSessionScope(t.Context(), home, child); !slices.Equal(got, []string{child.ID, want[0]}) {
		t.Fatalf("scope crossed ambiguous ancestor: %v", got)
	}
}

func TestResidentSessionScopeIndexErrorKeepsDirectParent(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	dir := core.SessionsDir(home, cwd)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	header, err := json.Marshal(map[string]any{"type": "meta", "meta": core.SessionMeta{ID: "parent", Parent: "grandparent", CWD: cwd}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "parent.jsonl"), append(header, '\n', '{', '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	// A regular file where the agent-store directory belongs makes the
	// header index fail with a non-ENOENT read error.
	if err := os.WriteFile(filepath.Join(home, "sessions", "agents"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.ManagedSessionParents(t.Context(), home); err == nil {
		t.Fatal("index error was not triggered")
	}
	child := &core.Session{ID: "child", Meta: core.SessionMeta{Parent: "parent"}}
	if got := residentSessionScope(t.Context(), home, child); !slices.Equal(got, []string{"child", "parent"}) {
		t.Fatalf("scope = %v, want [child parent]", got)
	}
}
