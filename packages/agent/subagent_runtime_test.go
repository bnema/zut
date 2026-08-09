package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

type runtimeTestRunner struct {
	started  chan struct{}
	startOne sync.Once
	canceled atomic.Int32
	block    bool
}

type stubbornRuntimeTestRunner struct {
	started chan struct{}
	release chan struct{}
}

func (r *runtimeTestRunner) Run(ctx context.Context, _ subagents.Sink) error {
	if r.started != nil {
		r.startOne.Do(func() { close(r.started) })
	}
	if !r.block {
		return nil
	}
	<-ctx.Done()
	r.canceled.Add(1)
	return ctx.Err()
}

func (r *stubbornRuntimeTestRunner) Run(context.Context, subagents.Sink) error {
	close(r.started)
	<-r.release
	return nil
}

func newRuntimeTestConfig(root, repoRoot string, runner func(*subagents.Agent) subagents.Runner) subagentRuntimeConfig {
	return subagentRuntimeConfig{
		Context:     context.Background(),
		Root:        root,
		RepoRoot:    repoRoot,
		Provider:    "parent-provider",
		Model:       "parent-model",
		Reasoning:   "high",
		BaseURL:     "https://parent.example/v1",
		InsecureTLS: true,
		FastMode:    true,
		Policy: subagents.SubagentPolicy{
			MaxConcurrent:     2,
			MaxTurns:          4,
			DefaultTimeout:    time.Minute,
			CancelGracePeriod: time.Second,
		},
		NewRunner: runner,
	}
}

func TestSubagentRuntimeLaunchCredentialStaysWithLaunchProvider(t *testing.T) {
	activeProvider := "parent-provider"
	cfg := newRuntimeTestConfig(t.TempDir(), t.TempDir(), func(*subagents.Agent) subagents.Runner {
		return &runtimeTestRunner{}
	})
	cfg.APIKey = "launch-key"
	cfg.ActiveProvider = func() string { return activeProvider }
	rt := newSubagentRuntime(cfg)
	defer func() { _ = rt.Close(context.Background()) }()

	credential, err := rt.resolveCredential(context.Background(), "parent-provider")
	if err != nil {
		t.Fatalf("launch provider credential: %v", err)
	}
	if credential.Value != "launch-key" {
		t.Fatalf("launch provider credential = %q, want launch key", credential.Value)
	}

	activeProvider = "later-provider"
	for _, providerID := range []string{"", "later-provider"} {
		credential, err = rt.resolveCredential(context.Background(), providerID)
		if err != nil {
			t.Fatalf("later provider credential (%q): %v", providerID, err)
		}
		if credential.Value == "launch-key" {
			t.Fatalf("launch API key leaked to provider selected after construction (%q)", providerID)
		}
	}
}

func TestSubagentRuntimeOwnsSupervisorConfigurationAndResolution(t *testing.T) {
	root := t.TempDir()
	repoRoot := t.TempDir()
	var captured *subagents.Agent
	runner := func(a *subagents.Agent) subagents.Runner {
		captured = a
		return &runtimeTestRunner{}
	}
	rt := newSubagentRuntime(newRuntimeTestConfig(root, repoRoot, runner))
	if rt == nil || rt.Supervisor() == nil {
		t.Fatal("runtime did not create a supervisor")
	}
	defer func() { _ = rt.Close(context.Background()) }()

	registry := rt.InjectTools(core.NewRegistry())
	tool, ok := registry["subagent_spawn"]
	if !ok {
		t.Fatal("runtime did not inject canonical spawn tool")
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"task":"configure child"}`), nil); err != nil {
		t.Fatalf("spawn tool: %v", err)
	}
	if captured == nil {
		t.Fatal("runner did not receive an agent")
	}
	if captured.Provider != "parent-provider" || captured.Model != "parent-model" || captured.Reasoning != "high" {
		t.Fatalf("child model settings = provider=%q model=%q reasoning=%q", captured.Provider, captured.Model, captured.Reasoning)
	}
	if captured.BaseURL != "https://parent.example/v1" || !captured.InsecureTLS || !captured.FastMode {
		t.Fatalf("child connection/settings = base_url=%q insecure_tls=%v fast_mode=%v", captured.BaseURL, captured.InsecureTLS, captured.FastMode)
	}
	if captured.Dir != repoRoot {
		t.Fatalf("child repo root = %q, want %q", captured.Dir, repoRoot)
	}
}

func TestSubagentRuntimeConfigurationConcurrentAccess(t *testing.T) {
	cfg := newRuntimeTestConfig(t.TempDir(), t.TempDir(), func(*subagents.Agent) subagents.Runner {
		return &runtimeTestRunner{}
	})
	rt := newSubagentRuntime(cfg)
	defer func() { _ = rt.Close(context.Background()) }()
	repoRoot := t.TempDir()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for index := 0; index < 100; index++ {
			rt.SetProvider("provider")
			rt.SetModel("model")
			rt.SetReasoning("medium")
			rt.SetProviderSettings("https://provider.example/v1", index%2 == 0)
			rt.SetFastMode(index%2 == 0)
			rt.SetRepoRoot(repoRoot)
			rt.SetWebSearchPolicy(subagents.WebSearchDeny)
		}
	}()
	go func() {
		defer wg.Done()
		for index := 0; index < 100; index++ {
			_ = rt.currentProvider()
			_ = rt.currentModel()
			_ = rt.currentReasoning()
			_ = rt.snapshotConfiguration()
		}
	}()
	wg.Wait()
}

func TestSubagentRuntimeResolvedWebSearchPolicyRefreshesSupervisor(t *testing.T) {
	var captured []*subagents.Agent
	cfg := newRuntimeTestConfig(t.TempDir(), t.TempDir(), func(a *subagents.Agent) subagents.Runner {
		captured = append(captured, a)
		return &runtimeTestRunner{}
	})
	cfg.WebSearchPolicy = subagents.WebSearchAllow
	rt := newSubagentRuntime(cfg)
	defer func() { _ = rt.Close(context.Background()) }()

	allowedRegistry := core.Registry{"web_search": &recordingWebSearchTool{}}
	rt.PrepareResolvedRegistry(allowedRegistry, subagents.WebSearchAllow)
	if rt.Supervisor().WebSearchPolicy() != subagents.WebSearchAllow {
		t.Fatal("resolved allow policy was not propagated to supervisor")
	}
	allowed, err := rt.Supervisor().SpawnReq(context.Background(), subagents.SpawnRequest{
		Task:  "allowed child",
		Tools: []string{"web_search"},
	})
	if err != nil {
		t.Fatalf("spawn allowed child: %v", err)
	}
	if allowed.WebSearchPolicy != subagents.WebSearchAllow {
		t.Fatalf("allowed child policy = %v, want allow", allowed.WebSearchPolicy)
	}

	deniedRegistry := core.Registry{"web_search": &recordingWebSearchTool{}}
	rt.PrepareResolvedRegistry(deniedRegistry, subagents.WebSearchDeny)
	if _, ok := deniedRegistry["web_search"]; ok {
		t.Fatal("resolved deny policy left web_search in parent registry")
	}
	if rt.Supervisor().WebSearchPolicy() != subagents.WebSearchDeny {
		t.Fatal("resolved deny policy was not propagated to supervisor")
	}
	denied, err := rt.Supervisor().SpawnReq(context.Background(), subagents.SpawnRequest{
		Task:  "denied child",
		Tools: []string{"web_search"},
	})
	if err != nil {
		t.Fatalf("spawn denied child: %v", err)
	}
	if denied.WebSearchPolicy != subagents.WebSearchDeny {
		t.Fatalf("denied child policy = %v, want deny", denied.WebSearchPolicy)
	}
	if len(captured) != 2 {
		t.Fatalf("captured children = %d, want 2", len(captured))
	}
}

func TestSubagentRuntimeInjectsCanonicalToolsAccordingToPolicy(t *testing.T) {
	cases := []struct {
		name string
		args Args
		want []string
	}{
		{name: "default", want: []string{"subagent_spawn", "subagent_status", "subagent_stop", "subagent_resume"}},
		{name: "explicit spawn and status", args: Args{ToolsSet: true, Tools: []string{"subagent_spawn", "subagent_status"}}, want: []string{"subagent_spawn", "subagent_status"}},
		{name: "no tools", args: Args{NoTools: true}},
		{name: "permission set", args: Args{PermissionSet: &tools.PermissionSet{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newRuntimeTestConfig(t.TempDir(), t.TempDir(), func(*subagents.Agent) subagents.Runner {
				return &runtimeTestRunner{}
			})
			cfg.Args = tc.args
			rt := newSubagentRuntime(cfg)
			defer func() { _ = rt.Close(context.Background()) }()
			got := rt.InjectTools(core.NewRegistry())
			for _, name := range tc.want {
				if _, ok := got[name]; !ok {
					t.Errorf("missing injected tool %q in %#v", name, got)
				}
			}
			if len(got) != len(tc.want) {
				t.Errorf("injected tools = %#v, want exactly %v", got, tc.want)
			}
		})
	}
}

func TestSubagentRuntimeFailedTransitionRestoresConfiguration(t *testing.T) {
	rt := newSubagentRuntime(newRuntimeTestConfig(t.TempDir(), t.TempDir(), func(*subagents.Agent) subagents.Runner {
		return &runtimeTestRunner{}
	}))
	defer func() { _ = rt.Close(context.Background()) }()
	before := rt.snapshotConfiguration()

	if err := func() error {
		rt.SetProvider("candidate-provider")
		rt.SetProviderSettings("https://candidate.example/v1", false)
		rt.SetFastMode(false)
		rt.SetRepoRoot(t.TempDir())
		rt.SetWebSearchPolicy(subagents.WebSearchDeny)
		return errors.New("candidate transition failed")
	}(); err != nil {
		rt.restoreConfiguration(before)
	}

	if got := rt.snapshotConfiguration(); got != before {
		t.Fatalf("runtime configuration after rollback = %+v, want %+v", got, before)
	}
	if got := rt.Supervisor().WebSearchPolicy(); got != before.webSearchPolicy {
		t.Fatalf("supervisor web-search policy after rollback = %v, want %v", got, before.webSearchPolicy)
	}
}

func TestSubagentRuntimeFreshShutdownContextCleansCanceledParent(t *testing.T) {
	runner := &runtimeTestRunner{started: make(chan struct{}), block: true}
	cfg := newRuntimeTestConfig(t.TempDir(), t.TempDir(), func(*subagents.Agent) subagents.Runner { return runner })
	rt := newSubagentRuntime(cfg)
	spawn := rt.InjectTools(core.NewRegistry())["subagent_spawn"]
	if _, err := spawn.Execute(context.Background(), json.RawMessage(`{"task":"cancel me"}`), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}
	if err := closeSubagentRuntimeFresh(rt); err != nil {
		t.Fatalf("fresh shutdown: %v", err)
	}
	if got := runner.canceled.Load(); got != 1 {
		t.Fatalf("runner cancellation count = %d, want 1", got)
	}
}

func TestSubagentRuntimeCloseRetriesAfterCanceledContext(t *testing.T) {
	runner := &stubbornRuntimeTestRunner{started: make(chan struct{}), release: make(chan struct{})}
	cfg := newRuntimeTestConfig(t.TempDir(), t.TempDir(), func(*subagents.Agent) subagents.Runner { return runner })
	rt := newSubagentRuntime(cfg)
	spawn := rt.InjectTools(core.NewRegistry())["subagent_spawn"]
	if _, err := spawn.Execute(context.Background(), json.RawMessage(`{"task":"retry cleanup"}`), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rt.Close(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("first Close error = %v, want context canceled", err)
	}
	close(runner.release)
	if err := rt.Close(context.Background()); err != nil {
		t.Fatalf("retry Close: %v", err)
	}
	if err := rt.Close(context.Background()); err != nil {
		t.Fatalf("idempotent Close: %v", err)
	}
}

func TestSubagentRuntimeCallbacksAndExactlyOnceClose(t *testing.T) {
	root := t.TempDir()
	runner := &runtimeTestRunner{started: make(chan struct{}), block: true}
	var spawned atomic.Int32
	var stopped atomic.Int32
	cfg := newRuntimeTestConfig(root, t.TempDir(), func(*subagents.Agent) subagents.Runner { return runner })
	cfg.OnSpawned = func(*subagents.Agent, string) { spawned.Add(1) }
	cfg.OnStopRequested = func(*subagents.Agent) { stopped.Add(1) }
	rt := newSubagentRuntime(cfg)
	registry := rt.InjectTools(core.NewRegistry())
	spawn, ok := registry["subagent_spawn"]
	if !ok {
		t.Fatal("spawn tool was not injected")
	}
	if _, err := spawn.Execute(context.Background(), json.RawMessage(`{"task":"callback task"}`), nil); err != nil {
		t.Fatalf("spawn tool: %v", err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}
	if got := spawned.Load(); got != 1 {
		t.Fatalf("OnSpawned calls = %d, want 1", got)
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rt.Close(closeCtx); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := rt.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := runner.canceled.Load(); got != 1 {
		t.Fatalf("runner cancellation count = %d, want exactly once", got)
	}
	if got := stopped.Load(); got != 0 {
		t.Fatalf("unexpected stop callback count = %d", got)
	}
}

func TestSubagentRuntimeInjectedSpawnProfileInheritsAndOverridesFrontmatter(t *testing.T) {
	cwd := t.TempDir()
	profilesDir := filepath.Join(cwd, ".agents")
	if err := os.MkdirAll(profilesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRuntimeProfile := func(name, frontmatter string) {
		t.Helper()
		contents := "---\nname: " + name + "\n" + frontmatter + "---\nDo the work.\n"
		if err := os.WriteFile(filepath.Join(profilesDir, name+".md"), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeRuntimeProfile("inherited", "")
	writeRuntimeProfile("overridden", "model: override-model\nprovider: override-provider\nthinking: low\n")
	t.Setenv("ZUT_AGENT_PROFILES", profilesDir)

	var captured []*subagents.Agent
	cfg := newRuntimeTestConfig(t.TempDir(), cwd, func(a *subagents.Agent) subagents.Runner {
		captured = append(captured, a)
		return &runtimeTestRunner{}
	})
	cfg.Reasoning = "high"
	rt := newSubagentRuntime(cfg)
	// Change the host-owned setting after runtime construction. A child without
	// a profile override must observe this new live value.
	rt.SetReasoning("medium")
	defer func() { _ = rt.Close(context.Background()) }()
	spawn := rt.InjectTools(core.NewRegistry())["subagent_spawn"]

	for _, name := range []string{"inherited", "overridden"} {
		raw := []byte(`{"task":"` + name + ` task","agent":"` + name + `"}`)
		result, err := spawn.Execute(context.Background(), raw, nil)
		if err != nil {
			t.Fatalf("spawn %s: %v", name, err)
		}
		if result.IsError {
			t.Fatalf("spawn %s returned tool error: %#v", name, result)
		}
	}
	if len(captured) != 2 {
		t.Fatalf("captured agents = %d, want 2", len(captured))
	}
	inherited, overridden := captured[0], captured[1]
	if inherited.Provider != "parent-provider" || inherited.Model != "parent-model" || inherited.Reasoning != "medium" {
		t.Fatalf("inherited profile settings = provider=%q model=%q reasoning=%q", inherited.Provider, inherited.Model, inherited.Reasoning)
	}
	if overridden.Provider != "override-provider" || overridden.Model != "override-model" || overridden.Reasoning != "low" {
		t.Fatalf("overridden profile settings = provider=%q model=%q reasoning=%q", overridden.Provider, overridden.Model, overridden.Reasoning)
	}
}
