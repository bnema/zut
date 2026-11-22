package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/advisor"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/providermocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestAdvisorRegistrationPolicy(t *testing.T) {
	enabled := Config{Advisor: &AdvisorConfig{Enabled: true}}
	for _, tc := range []struct {
		name string
		args Args
		cfg  Config
		want bool
	}{
		{"default config", Args{}, Config{}, false},
		{"disabled block", Args{}, Config{Advisor: &AdvisorConfig{Model: "anthropic/claude-opus-4-7"}}, false},
		{"enabled without model", Args{}, enabled, true},
		{"explicit", Args{ToolsSet: true, Tools: []string{"advisor"}}, enabled, true},
		{"excluded", Args{Tools: []string{"read"}}, enabled, false},
		{"empty selection", Args{ToolsSet: true}, enabled, false},
		{"no tools", Args{NoTools: true}, enabled, false},
		{"permissions", Args{PermissionSet: &tools.PermissionSet{}}, enabled, false},
		{"resident child", Args{ResidentChild: true}, enabled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := core.Registry{}
			maybeAddAdvisorTool(reg, tc.args, tc.cfg)
			_, present := reg[tools.AdvisorToolName]
			if present != tc.want {
				t.Fatalf("present=%v want %v", present, tc.want)
			}
			if got := advisorSystemAddendum(reg) != ""; got != tc.want {
				t.Fatalf("addendum=%v want %v", got, tc.want)
			}
		})
	}
}

func TestAdvisorResolveRegistrationAndPrompt(t *testing.T) {
	project := pythonTestEnv(t)
	args := pythonTestArgs(project)
	r := resolvePython(t, args)
	if _, ok := r.ToolRegistry[tools.AdvisorToolName]; ok || strings.Contains(r.SystemPrompt, "[advisor]") {
		t.Fatal("advisor enabled by default")
	}
	if err := SaveConfig(Config{Advisor: &AdvisorConfig{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	r = resolvePython(t, args)
	if _, ok := r.ToolRegistry[tools.AdvisorToolName].(*tools.AdvisorTool); !ok {
		t.Fatalf("advisor not registered: %#v", r.ToolRegistry[tools.AdvisorToolName])
	}
	for _, want := range []string{"[advisor]", "An advisor (a stronger reviewer model) watches your work", "Do not call it on your own initiative."} {
		if !strings.Contains(r.SystemPrompt, want) {
			t.Fatalf("system prompt missing %q", want)
		}
	}
	child := args
	child.ResidentChild = true
	r = resolvePython(t, child)
	if _, ok := r.ToolRegistry[tools.AdvisorToolName]; ok || strings.Contains(r.SystemPrompt, "[advisor]") {
		t.Fatal("advisor offered to a resident child")
	}
}

func TestAdvisorExcludedFromResidentChildRegistry(t *testing.T) {
	catalogue := core.Registry{tools.AdvisorToolName: &tools.AdvisorTool{}}
	if _, err := residentChildRegistry(catalogue, []string{tools.AdvisorToolName}); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("err=%v", err)
	}
}

func TestAdvisorExtensionCannotClaimName(t *testing.T) {
	project := pythonTestEnv(t)
	r := resolvePython(t, pythonTestArgs(project))
	r.MergeExtensionTools(pythonExtStub{infos: []ExtensionToolInfo{{Extension: "x", Name: tools.AdvisorToolName}}})
	if _, ok := r.ToolRegistry[tools.AdvisorToolName]; ok {
		t.Fatal("extension registered the reserved advisor name")
	}
}

func TestAdvisorToolTargetErrors(t *testing.T) {
	pythonTestEnv(t)
	reg := core.Registry{}
	maybeAddAdvisorTool(reg, Args{Env: []string{"ANTHROPIC_API_KEY=", "ANTHROPIC_OAUTH_TOKEN="}}, Config{Advisor: &AdvisorConfig{Enabled: true}})
	tool := reg[tools.AdvisorToolName].(*tools.AdvisorTool)
	for _, tc := range []struct {
		cfg  *AdvisorConfig
		want string
	}{
		{&AdvisorConfig{Enabled: true}, "no advisor model configured; run /advisor provider/model:reasoning"},
		{&AdvisorConfig{Enabled: false, Model: "anthropic/claude-opus-4-7"}, "advisor is disabled"},
		{&AdvisorConfig{Enabled: true, Model: "anthropic/claude-opus-4-7"}, `no credential for advisor provider "anthropic"`},
	} {
		if err := SaveConfig(Config{Advisor: tc.cfg}); err != nil {
			t.Fatal(err)
		}
		_, err := tool.Target(context.Background())
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%+v: err=%v want %q", tc.cfg, err, tc.want)
		}
	}
}

func TestInstallAdvisorForwardsLiveAgentContext(t *testing.T) {
	pythonTestEnv(t)
	advisorClient := providermocks.NewMockClient(t)
	var body string
	advisorClient.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
		body = req.Messages[0].Content[0].(provider.TextBlock).Text
		ch := make(chan provider.Event, 2)
		ch <- provider.EventTextDelta{Delta: "advice"}
		ch <- provider.EventDone{}
		close(ch)
		return ch, nil
	})
	newTool := func() *tools.AdvisorTool {
		return &tools.AdvisorTool{Target: func(context.Context) (advisor.Target, error) {
			return advisor.Target{Client: advisorClient, Model: provider.Model{ID: "adv"}}, nil
		}}
	}
	first := core.Registry{tools.AdvisorToolName: newTool()}
	ag := core.NewAgent(providermocks.NewMockClient(t), "exec", "first system", first)
	installAdvisor(ag, clientAdvisorTool(first), nil, AdvisorSettings{})
	if ag.Watcher() == nil {
		t.Fatal("installAdvisor must install the passive watcher")
	}

	// A refresh installs a freshly built registry; it must be bound too.
	second := core.Registry{tools.AdvisorToolName: newTool()}
	installAdvisor(ag, clientAdvisorTool(second), nil, AdvisorSettings{})
	ag.SetPromptConfig("refreshed system", second)
	ag.SetMessages([]provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "user task"}}}})

	r, err := second[tools.AdvisorToolName].Execute(context.Background(), nil, nil)
	if err != nil || r.IsError {
		t.Fatalf("result=%+v err=%v", r, err)
	}
	if !strings.Contains(body, "refreshed system") || !strings.Contains(body, "user task") || !strings.Contains(body, "## advisor") {
		t.Fatalf("transcript=%s", body)
	}
}

func TestAdvisorConfirmationExempt(t *testing.T) {
	for name, want := range map[string]bool{
		tools.AdvisorToolName:         true,
		tools.AskUserQuestionToolName: true,
		tools.SessionsToolName:        true,
		"bash":                        false,
		"unknown":                     false,
	} {
		if got := confirmationExempt(name); got != want {
			t.Fatalf("%s: exempt=%v want %v", name, got, want)
		}
	}
}

func TestAdvisorSettingsStoreSetAdvisor(t *testing.T) {
	pythonTestEnv(t)
	env := map[string]string{}
	store := advisorSettingsStore{getenv: func(k string) string { return env[k] }}
	str := func(s string) *string { return &s }
	boolp := func(b bool) *bool { return &b }

	status, err := store.SetAdvisor(session.AdvisorChange{})
	if err != nil || status != (session.AdvisorStatus{Mode: "auto"}) {
		t.Fatalf("empty change: %+v %v", status, err)
	}
	if _, err := store.SetAdvisor(session.AdvisorChange{Enabled: boolp(true)}); !errors.Is(err, advisor.ErrNoModel) {
		t.Fatalf("enable without model: %v", err)
	}
	if _, err := store.SetAdvisor(session.AdvisorChange{Model: str("anthropic/nope")}); err == nil || !strings.Contains(err.Error(), "unknown advisor model") {
		t.Fatalf("bad model: %v", err)
	}
	if _, err := store.SetAdvisor(session.AdvisorChange{Model: str("anthropic/claude-opus-4-7:low")}); err == nil || !strings.Contains(err.Error(), "no credential") {
		t.Fatalf("missing credential: %v", err)
	}
	if cfg, _ := LoadConfig(); cfg.Advisor != nil {
		t.Fatalf("failed validation persisted: %+v", cfg.Advisor)
	}

	env["ANTHROPIC_API_KEY"] = "synthetic-test-key"
	status, err = store.SetAdvisor(session.AdvisorChange{Model: str("anthropic/claude-opus-4-7:low")})
	if err != nil || !status.Enabled || !strings.HasPrefix(status.Model, "anthropic/claude-opus-4-7") || status.Mode != "auto" {
		t.Fatalf("set model: %+v %v", status, err)
	}
	status, err = store.SetAdvisor(session.AdvisorChange{Enabled: boolp(false)})
	if err != nil || status.Enabled || status.Model == "" {
		t.Fatalf("disable: %+v %v", status, err)
	}
	if got, _ := store.AdvisorStatus(); got != status {
		t.Fatalf("status=%+v want %+v", got, status)
	}
	status, err = store.SetAdvisor(session.AdvisorChange{Enabled: boolp(true)})
	if err != nil || !status.Enabled {
		t.Fatalf("re-enable: %+v %v", status, err)
	}
	// Model and Enabled in one change apply together.
	status, err = store.SetAdvisor(session.AdvisorChange{Model: str("anthropic/claude-opus-4-7:off"), Enabled: boolp(false)})
	if err != nil || status.Enabled || status.Model != "anthropic/claude-opus-4-7:off" {
		t.Fatalf("model+disable: %+v %v", status, err)
	}
	// An explicit disable without a block writes nothing.
	if err := SaveConfig(Config{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAdvisor(session.AdvisorChange{Enabled: boolp(false)}); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := LoadConfig(); cfg.Advisor != nil {
		t.Fatalf("disable created a block: %+v", cfg.Advisor)
	}
}

func TestDecideAdvisorMode(t *testing.T) {
	native := AdvisorSettings{Enabled: true, Model: "anthropic/claude-opus-4-7:high", Mode: AdvisorModeAuto, MaxTokens: 4096}
	with := func(f func(*AdvisorSettings)) AdvisorSettings { s := native; f(&s); return s }
	for _, tc := range []struct {
		name          string
		available     bool
		s             AdvisorSettings
		prov, model   string
		wantServerMdl string
	}{
		{"anthropic pair compatible", true, native, "anthropic", "claude-sonnet-4-6", "claude-opus-4-7"},
		{"incompatible pair", true, native, "anthropic", "claude-opus-5-5", ""},
		{"openai executor", true, native, "openai", "gpt-6.1-sol", ""},
		{"openai advisor", true, with(func(s *AdvisorSettings) { s.Model = "openai/gpt-6.1-sol" }), "anthropic", "claude-sonnet-4-6", ""},
		{"anthropic-compatible endpoint", true, native, "kimi", "claude-sonnet-4-6", ""},
		{"mode client", true, with(func(s *AdvisorSettings) { s.Mode = AdvisorModeClient }), "anthropic", "claude-sonnet-4-6", ""},
		{"no model", true, with(func(s *AdvisorSettings) { s.Model = "" }), "anthropic", "claude-sonnet-4-6", ""},
		{"disabled", true, with(func(s *AdvisorSettings) { s.Enabled = false }), "anthropic", "claude-sonnet-4-6", ""},
		{"not available", false, native, "anthropic", "claude-sonnet-4-6", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := decideAdvisorMode(tc.available, tc.s, tc.prov, tc.model)
			if tc.wantServerMdl == "" {
				if server != nil {
					t.Fatalf("server=%+v", server)
				}
				return
			}
			if server == nil || server.Model != tc.wantServerMdl || server.MaxTokens != 4096 {
				t.Fatalf("server=%+v", server)
			}
		})
	}
}

func advisorAnthropicArgs(project, model string) Args {
	return Args{Provider: "anthropic", Model: model, CWD: project, NoLSP: true, NoSkill: true, NoContextFiles: true}
}

func TestAdvisorNativeModeFollowsExecutor(t *testing.T) {
	project := pythonTestEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "synthetic-test-key")
	t.Setenv("OPENAI_API_KEY", "synthetic-test-key")
	if err := SaveConfig(Config{Advisor: &AdvisorConfig{Enabled: true, Model: "anthropic/claude-opus-4-7:high", MaxTokens: 2048}}); err != nil {
		t.Fatal(err)
	}

	// Native: no client tool, guidance kept, server advisor set.
	r := resolvePython(t, advisorAnthropicArgs(project, "claude-sonnet-4-6"))
	ag := r.NewAgent()
	_, tools0 := ag.PromptConfig()
	if _, ok := tools0[tools.AdvisorToolName]; ok {
		t.Fatal("client advisor tool registered in native mode")
	}
	if !strings.Contains(r.SystemPrompt, "[advisor]") {
		t.Fatal("native mode lost the advisor guidance")
	}
	if got := ag.ServerAdvisor(); got == nil || got.Model != "claude-opus-4-7" || got.MaxTokens != 2048 {
		t.Fatalf("server advisor = %+v", got)
	}

	// Model switch (buildFor -> Resolve -> NewAgent) to a non-Anthropic executor.
	switched := resolvePython(t, Args{Provider: "openai", Model: "gpt-5", CWD: project, NoLSP: true, NoSkill: true, NoContextFiles: true}).NewAgent()
	if switched.ServerAdvisor() != nil {
		t.Fatal("server advisor kept for an openai executor")
	}
	if _, ok := switched.ToolsSnapshot()[tools.AdvisorToolName]; !ok {
		t.Fatal("client advisor tool missing after switching away from anthropic")
	}

	// A refresh with stale launch args follows the live executor.
	if _, err := refreshAgentToolsAndPrompt(advisorAnthropicArgs(project, "claude-sonnet-4-6"), nil, nil, switched, nil, nil); err != nil {
		t.Fatal(err)
	}
	if switched.ServerAdvisor() != nil {
		t.Fatal("refresh re-enabled native advisor for an openai executor")
	}
	if _, ok := switched.ToolsSnapshot()[tools.AdvisorToolName]; !ok {
		t.Fatal("refresh dropped the client advisor tool")
	}

	// /advisor off + refresh clears native mode on the anthropic agent.
	off := false
	if _, err := (advisorSettingsStore{}).SetAdvisor(session.AdvisorChange{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if _, err := refreshAgentToolsAndPrompt(advisorAnthropicArgs(project, "claude-sonnet-4-6"), nil, nil, ag, nil, nil); err != nil {
		t.Fatal(err)
	}
	system, current := ag.PromptConfig()
	if ag.ServerAdvisor() != nil || strings.Contains(system, "[advisor]") {
		t.Fatal("disabled advisor still active after refresh")
	}
	if _, ok := current[tools.AdvisorToolName]; ok {
		t.Fatal("disabled advisor tool still registered")
	}
}

func TestAdvisorExcludedForPackagedAndOptedOutRuntimes(t *testing.T) {
	enabled := Config{Advisor: &AdvisorConfig{Enabled: true}}
	for name, args := range map[string]Args{
		"no advisor": {NoAdvisor: true},
		"agent name": {AgentName: "packaged"},
		"zutfile":    {zutfile: &session.ZutfileSpec{Name: "z"}},
	} {
		if advisorToolAllowed(args, enabled) {
			t.Fatalf("%s: advisor allowed", name)
		}
	}
}

func TestNewAgentDoesNotMutateResolvedRegistry(t *testing.T) {
	project := pythonTestEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "synthetic-test-key")
	if err := SaveConfig(Config{Advisor: &AdvisorConfig{Enabled: true, Model: "anthropic/claude-opus-4-7:high"}}); err != nil {
		t.Fatal(err)
	}
	r := resolvePython(t, advisorAnthropicArgs(project, "claude-sonnet-4-6"))
	if _, ok := r.ToolRegistry[tools.AdvisorToolName]; !ok {
		t.Fatal("resolve did not register the advisor tool")
	}
	for n := 0; n < 2; n++ {
		ag := r.NewAgent()
		if ag.ServerAdvisor() == nil {
			t.Fatalf("agent %d: native advisor missing", n)
		}
		if _, ok := ag.ToolsSnapshot()[tools.AdvisorToolName]; ok {
			t.Fatalf("agent %d: client tool in native mode", n)
		}
	}
	if _, ok := r.ToolRegistry[tools.AdvisorToolName]; !ok {
		t.Fatal("NewAgent removed the tool from the shared registry")
	}
}

// A /btw daemon fork (CreateSpec.NoAdvisor) gets neither the client tool nor
// the native server advisor, even with a native-capable executor.
func TestSessionDepsNoAdvisorFork(t *testing.T) {
	sessionAssemblyEnv(t)
	server := sessionAssemblyProvider(t)
	if err := SaveConfig(Config{Advisor: &AdvisorConfig{Enabled: true, Model: "anthropic/claude-opus-4-7:high"}}); err != nil {
		t.Fatal(err)
	}
	args := []string{"--provider", "anthropic", "--model", "claude-sonnet-4-6", "--base-url", server.URL, "--api-key", "test-key", "--no-ext", "--no-mcp"}
	for _, tc := range []struct {
		noAdvisor bool
		native    bool
	}{{false, true}, {true, false}} {
		deps, _ := newTestSessionDeps(t, session.CreateSpec{CWD: t.TempDir(), Env: []string{}, Args: args, Headless: true, NoAdvisor: tc.noAdvisor})
		ag, _, _, err := deps.AgentBuilder.BuildAgent()
		if err != nil {
			t.Fatal(err)
		}
		if got := ag.ServerAdvisor() != nil; got != tc.native {
			t.Fatalf("noAdvisor=%v: native=%v", tc.noAdvisor, got)
		}
		if _, ok := ag.ToolsSnapshot()[tools.AdvisorToolName]; ok {
			t.Fatalf("noAdvisor=%v: client tool registered", tc.noAdvisor)
		}
	}
}

func TestResolveAdvisorEndpointMatchesExecutorRules(t *testing.T) {
	pythonTestEnv(t)
	if err := os.MkdirAll(ZutHome(), 0o755); err != nil {
		t.Fatal(err)
	}
	loadTestModelsJSON(t, `{"providers":{
		"unsloth":{"baseUrl":"http://127.0.0.1:9/v1","api":"openai","auth":"none","models":[{"id":"qista"}]},
		"ollama":{"baseUrl":"http://127.0.0.1:11434","models":[{"id":"qwen-local"}]}}}`)
	noEnv := func(string) string { return "" }

	spec, err := ParseAdvisorModelSpec("unsloth/qista")
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := resolveAdvisorEndpoint(context.Background(), spec, noEnv)
	if err != nil || r.Credential != "" || !r.credentialOptional || r.BaseURL != "http://127.0.0.1:9/v1" {
		t.Fatalf("keyless custom: %+v %v", r, err)
	}

	spec, err = ParseAdvisorModelSpec("ollama/qwen-local")
	if err != nil {
		t.Fatal(err)
	}
	r, _, err = resolveAdvisorEndpoint(context.Background(), spec, noEnv)
	if err != nil || r.Credential != "ollama" || r.BaseURL != "http://127.0.0.1:11434" {
		t.Fatalf("ollama: %+v %v", r, err)
	}

	// The keyless store check accepts both at set time too.
	store := advisorSettingsStore{getenv: noEnv}
	model := "unsloth/qista"
	if _, err := store.SetAdvisor(session.AdvisorChange{Model: &model}); err != nil {
		t.Fatalf("set keyless advisor: %v", err)
	}

	spec, err = ParseAdvisorModelSpec("anthropic/claude-opus-4-7")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = resolveAdvisorEndpoint(context.Background(), spec, noEnv)
	if err == nil || !strings.Contains(err.Error(), `no credential for advisor provider "anthropic"`) || errors.Unwrap(err) == nil {
		t.Fatalf("missing credential: %v", err)
	}
}

func TestInstallServerAdvisorDropsStaleNativeAdvisor(t *testing.T) {
	pythonTestEnv(t)
	cfg := Config{Advisor: &AdvisorConfig{Enabled: true, Model: "anthropic/claude-opus-4-7:high", MaxTokens: 2048}}
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("anthropic").Maybe()
	var sent []*provider.NativeAdvisor
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
		sent = append(sent, req.Advisor)
		ch := make(chan provider.Event, 1)
		ch <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "ok"}}}}
		close(ch)
		return ch, nil
	})
	ag := core.NewAgent(client, "claude-sonnet-4-6", "system", core.Registry{})
	installServerAdvisor(ag, &provider.NativeAdvisor{Model: "claude-opus-4-7", MaxTokens: 2048}, cfg.AdvisorSettings())
	prompt := func() {
		if err := ag.Prompt(context.Background(), "go", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	prompt()
	// Another session switches the advisor model in the shared config.
	if _, err := (advisorSettingsStore{getenv: func(string) string { return "" }}).SetAdvisor(session.AdvisorChange{Enabled: func() *bool { b := false; return &b }()}); err != nil {
		t.Fatal(err)
	}
	prompt()
	if len(sent) != 2 || sent[0] == nil || sent[0].Model != "claude-opus-4-7" || sent[1] != nil {
		t.Fatalf("sent advisors = %+v", sent)
	}
	installServerAdvisor(ag, nil, AdvisorSettings{})
	if ag.ServerAdvisor() != nil {
		t.Fatal("clearing left a static advisor")
	}
}

func TestNewAgentBindsClonedAdvisorTool(t *testing.T) {
	project := pythonTestEnv(t)
	t.Setenv("OPENAI_API_KEY", "synthetic-test-key")
	if err := SaveConfig(Config{Advisor: &AdvisorConfig{Enabled: true, Model: "anthropic/claude-opus-4-7:high"}}); err != nil {
		t.Fatal(err)
	}
	r := resolvePython(t, Args{Provider: "openai", Model: "gpt-5", CWD: project, NoLSP: true, NoSkill: true, NoContextFiles: true})
	shared := r.ToolRegistry[tools.AdvisorToolName].(*tools.AdvisorTool)
	a1, a2 := r.NewAgent(), r.NewAgent()
	_, t1 := a1.PromptConfig()
	_, t2 := a2.PromptConfig()
	c1, c2 := t1[tools.AdvisorToolName].(*tools.AdvisorTool), t2[tools.AdvisorToolName].(*tools.AdvisorTool)
	if c1 == shared || c2 == shared || c1 == c2 {
		t.Fatal("agents share the advisor tool instance")
	}
	// The shared instance stays unbound.
	if res, _ := shared.Execute(context.Background(), nil, nil); !res.IsError {
		t.Fatal("shared advisor tool was bound by NewAgent")
	}
}

func TestInstallAdvisorKeepsWatcherAcrossRefreshAndClearsWhenOff(t *testing.T) {
	pythonTestEnv(t)
	newTool := func() *tools.AdvisorTool {
		return &tools.AdvisorTool{Target: func(context.Context) (advisor.Target, error) { return advisor.Target{}, advisor.ErrNoModel }}
	}
	ag := core.NewAgent(providermocks.NewMockClient(t), "exec", "system", core.Registry{})
	installAdvisor(ag, newTool(), nil, AdvisorSettings{Mode: AdvisorModeAuto})
	first := ag.Watcher()
	require.NotNil(t, first)

	installAdvisor(ag, newTool(), nil, AdvisorSettings{Mode: AdvisorModeAuto})
	require.Same(t, first, ag.Watcher(), "a refresh keeps the advisor's conversation")

	installAdvisor(ag, nil, nil, AdvisorSettings{})
	require.Nil(t, ag.Watcher(), "/advisor off removes the watcher")
}

func TestInstallAdvisorPausesWhenSettingsChanged(t *testing.T) {
	pythonTestEnv(t)
	advisorClient := providermocks.NewMockClient(t) // any review would fail the test
	tool := &tools.AdvisorTool{Target: func(context.Context) (advisor.Target, error) {
		return advisor.Target{Client: advisorClient, Model: provider.Model{ID: "adv"}}, nil
	}}
	ag := core.NewAgent(providermocks.NewMockClient(t), "exec", "system", core.Registry{})
	// The session decided from settings that another session has since
	// changed: the watcher must stay quiet until the next refresh.
	installAdvisor(ag, tool, nil, AdvisorSettings{Enabled: true, Model: "anthropic/x", Mode: AdvisorModeAuto})
	w := ag.Watcher()
	ctx := context.Background()
	w.RunStarted(ctx)
	defer w.RunEnded()
	edit := provider.ToolCallBlock{ID: "e", Name: "edit", Arguments: []byte(`{}`)}
	require.Equal(t, core.WatcherVerdict{}, w.BeforeTool(ctx, edit))
}
