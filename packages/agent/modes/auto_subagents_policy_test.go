package modes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

type failingAutoSubagentsSettingsStore struct {
	SettingsStore
	err   error
	calls int
}

func (s *failingAutoSubagentsSettingsStore) SetAutoSubagents(bool) error {
	s.calls++
	return s.err
}

func TestAutoSubagentsToggleRollsBackInMemoryStateWhenPersistenceFails(t *testing.T) {
	previous := true
	allowed := true
	supervisor := subagents.New(subagents.Config{Root: t.TempDir(), RepoRoot: t.TempDir()})
	t.Cleanup(supervisor.StopAll)
	store := &failingAutoSubagentsSettingsStore{err: errors.New("disk full")}
	agent := core.NewAgent(nil, "test-model", "base system\n\nauto-subagents guidance", core.Registry{
		"subagent_spawn":  &tools.SubagentSpawnTool{Supervisor: supervisor},
		"subagent_status": &tools.SubagentStatusTool{Supervisor: supervisor},
	})
	interactive := NewInteractive(InteractiveConfig{
		Agent:                          agent,
		Supervisor:                     supervisor,
		AutoSubagentsEnabled:           &previous,
		AutoSubagentsToolAllowed:       &allowed,
		AutoSubagentsStatusToolAllowed: &allowed,
		AutoSubagentsSystemAddendum:    "auto-subagents guidance",
		SettingsStore:                  store,
	})
	interactive.settingsDialog = newSettingsDialog()
	interactive.rend = nil
	interactive.openSettingsDialog()
	beforeSystem, _ := agent.PromptConfig()
	beforeTools := agent.ToolsSnapshot()

	interactive.applySettingToggle("auto_subagents_enabled", false)

	interactive.mu.Lock()
	gotEnabled := interactive.cfg.AutoSubagentsEnabled != nil && *interactive.cfg.AutoSubagentsEnabled
	statusErr := interactive.statusErr
	interactive.mu.Unlock()
	if !gotEnabled {
		t.Fatal("auto-subagents flag stayed disabled after persistence failure")
	}
	if store.calls != 1 {
		t.Fatalf("SetAutoSubagents calls = %d, want 1", store.calls)
	}
	if !strings.Contains(statusErr, "disk full") {
		t.Fatalf("status error = %q, want persistence failure", statusErr)
	}
	afterSystem, _ := agent.PromptConfig()
	if afterSystem != beforeSystem {
		t.Fatalf("system prompt changed after persistence failure: %q -> %q", beforeSystem, afterSystem)
	}
	afterTools := agent.ToolsSnapshot()
	if len(afterTools) != len(beforeTools) {
		t.Fatalf("tool registry size = %d, want unchanged %d", len(afterTools), len(beforeTools))
	}
	for name, before := range beforeTools {
		if afterTools[name] != before {
			t.Fatalf("tool %q changed after persistence failure", name)
		}
	}
	item := findSettingsItem(interactive.settingsDialog.items, "auto_subagents_enabled")
	if item == nil || !item.value {
		t.Fatalf("settings row = %#v, want restored enabled value", item)
	}
}

func TestOrchestratorSlashTogglesAutoSubagents(t *testing.T) {
	allowed := true
	enabled := false
	supervisor := subagents.New(subagents.Config{Root: t.TempDir(), RepoRoot: t.TempDir()})
	t.Cleanup(supervisor.StopAll)
	store := &failingAutoSubagentsSettingsStore{}
	agent := core.NewAgent(nil, "test-model", "base system", core.Registry{})
	interactive := NewInteractive(InteractiveConfig{
		Agent:                       agent,
		Supervisor:                  supervisor,
		AutoSubagentsEnabled:        &enabled,
		AutoSubagentsToolAllowed:    &allowed,
		AutoSubagentsSystemAddendum: "orchestrator guidance",
		SettingsStore:               store,
	})
	interactive.rend = nil

	if interactive.runSlash(context.Background(), "/ORCHESTRATOR") {
		t.Fatal("/orchestrator requested exit")
	}
	if interactive.cfg.AutoSubagentsEnabled == nil || !*interactive.cfg.AutoSubagentsEnabled {
		t.Fatal("/orchestrator did not enable orchestration")
	}
	if store.calls != 1 || interactive.statusOK != "subagent orchestrator enabled" {
		t.Fatalf("enable persistence/status = calls=%d status=%q", store.calls, interactive.statusOK)
	}
	if system, _ := agent.PromptConfig(); !strings.Contains(system, "orchestrator guidance") {
		t.Fatalf("enabled system prompt = %q, want orchestrator guidance", system)
	}

	interactive.runSlash(context.Background(), "/orchestrator")
	if interactive.cfg.AutoSubagentsEnabled == nil || *interactive.cfg.AutoSubagentsEnabled {
		t.Fatal("/orchestrator did not disable orchestration")
	}
	if store.calls != 2 || interactive.statusOK != "subagent orchestrator disabled" {
		t.Fatalf("disable persistence/status = calls=%d status=%q", store.calls, interactive.statusOK)
	}
	if system, _ := agent.PromptConfig(); strings.Contains(system, "orchestrator guidance") {
		t.Fatalf("disabled system prompt still contains orchestrator guidance: %q", system)
	}
	if _, ok := agent.ToolsSnapshot()["subagent_spawn"]; !ok {
		t.Fatal("disabling orchestration removed subagent_spawn")
	}
}

func TestAutoSubagentsSettingsDisableUnavailablePolicy(t *testing.T) {
	trueValue := true
	falseValue := false

	tests := []struct {
		name       string
		supervisor *subagents.Supervisor
		allowed    *bool
		wantHint   string
	}{
		{
			name:     "supervisor unavailable",
			allowed:  &trueValue,
			wantHint: "subagent supervisor not available in this mode",
		},
		{
			name:       "launch policy excludes tool",
			supervisor: subagents.New(subagents.Config{Root: t.TempDir(), RepoRoot: t.TempDir()}),
			allowed:    &falseValue,
			wantHint:   "launch-time tool policy excludes subagent manager tools",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.supervisor != nil {
				t.Cleanup(tt.supervisor.StopAll)
			}
			iv := &Interactive{
				cfg: InteractiveConfig{
					AutoSubagentsEnabled:     &trueValue,
					AutoSubagentsToolAllowed: tt.allowed,
					Supervisor:               tt.supervisor,
				},
				settingsDialog: newSettingsDialog(),
			}
			iv.openSettingsDialog()

			var item settingsItem
			for _, candidate := range iv.settingsDialog.items {
				if candidate.key == "auto_subagents_enabled" {
					item = candidate
					break
				}
			}
			if !item.disabled {
				t.Fatal("auto-subagents setting is not disabled")
			}
			if item.value {
				t.Fatal("disabled auto-subagents setting is shown as enabled")
			}
			if !strings.Contains(item.hint, tt.wantHint) {
				t.Fatalf("hint = %q; want %q", item.hint, tt.wantHint)
			}
		})
	}
}

func TestAutoSubagentsToolRegistrationHonorsLaunchPolicy(t *testing.T) {
	allowed := false
	enabled := false
	supervisor := subagents.New(subagents.Config{Root: t.TempDir(), RepoRoot: t.TempDir()})
	t.Cleanup(supervisor.StopAll)
	iv := &Interactive{
		agent: &core.Agent{Tools: core.Registry{}},
		cfg: InteractiveConfig{
			AutoSubagentsEnabled:     &enabled,
			AutoSubagentsToolAllowed: &allowed,
			Supervisor:               supervisor,
		},
		dirty: make(chan struct{}, 1),
	}

	iv.applyAutoSubagentsTool()
	if _, ok := iv.agent.Tools["subagent_spawn"]; ok {
		t.Fatal("subagent_spawn registered despite launch-time policy")
	}
	if _, ok := iv.agent.Tools["subagent_status"]; ok {
		t.Fatal("subagent_status registered despite launch-time policy")
	}
	if _, ok := iv.agent.Tools["subagent_stop"]; ok {
		t.Fatal("subagent_stop registered despite launch-time policy")
	}
	if _, ok := iv.agent.Tools["subagent_resume"]; ok {
		t.Fatal("subagent_resume registered despite launch-time policy")
	}
	iv.applySettingToggle("auto_subagents_enabled", true)
	iv.mu.Lock()
	statusErr := iv.statusErr
	value := iv.cfg.AutoSubagentsEnabled != nil && *iv.cfg.AutoSubagentsEnabled
	iv.mu.Unlock()
	if value {
		t.Fatal("auto-subagents toggle enabled despite launch-time policy")
	}
	if !strings.Contains(statusErr, "launch-time tool policy") {
		t.Fatalf("toggle error = %q; want launch-time policy hint", statusErr)
	}
}

func TestAutoSubagentsToolRegistrationHonorsSeparateLaunchPolicies(t *testing.T) {
	for _, tc := range []struct {
		name          string
		spawnAllowed  bool
		statusAllowed bool
		stopAllowed   bool
		resumeAllowed bool
		wantSpawn     bool
		wantStatus    bool
		wantStop      bool
		wantResume    bool
	}{
		{name: "status only", statusAllowed: true, wantStatus: true},
		{name: "spawn only", spawnAllowed: true, wantSpawn: true},
		{name: "stop only", stopAllowed: true, wantStop: true},
		{name: "resume only", resumeAllowed: true, wantResume: true},
		{name: "all", spawnAllowed: true, statusAllowed: true, stopAllowed: true, resumeAllowed: true, wantSpawn: true, wantStatus: true, wantStop: true, wantResume: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enabled := true
			spawnAllowed := tc.spawnAllowed
			statusAllowed := tc.statusAllowed
			stopAllowed := tc.stopAllowed
			resumeAllowed := tc.resumeAllowed
			supervisor := subagents.New(subagents.Config{Root: t.TempDir(), RepoRoot: t.TempDir()})
			t.Cleanup(supervisor.StopAll)
			iv := &Interactive{
				agent: &core.Agent{Tools: core.Registry{}},
				cfg: InteractiveConfig{
					AutoSubagentsEnabled:           &enabled,
					AutoSubagentsToolAllowed:       &spawnAllowed,
					AutoSubagentsStatusToolAllowed: &statusAllowed,
					AutoSubagentsStopToolAllowed:   &stopAllowed,
					AutoSubagentsResumeToolAllowed: &resumeAllowed,
					Supervisor:                     supervisor,
				},
				dirty: make(chan struct{}, 1),
			}

			iv.applyAutoSubagentsTool()
			_, gotSpawn := iv.agent.Tools["subagent_spawn"]
			_, gotStatus := iv.agent.Tools["subagent_status"]
			_, gotStop := iv.agent.Tools["subagent_stop"]
			_, gotResume := iv.agent.Tools["subagent_resume"]
			if gotSpawn != tc.wantSpawn || gotStatus != tc.wantStatus || gotStop != tc.wantStop || gotResume != tc.wantResume {
				t.Fatalf("registered tools spawn=%v status=%v stop=%v resume=%v, want spawn=%v status=%v stop=%v resume=%v", gotSpawn, gotStatus, gotStop, gotResume, tc.wantSpawn, tc.wantStatus, tc.wantStop, tc.wantResume)
			}
		})
	}
}

func TestAutoSubagentsDisabledKeepsToolsForExplicitUserRequests(t *testing.T) {
	enabled := false
	allowed := true
	supervisor := subagents.New(subagents.Config{Root: t.TempDir(), RepoRoot: t.TempDir()})
	t.Cleanup(supervisor.StopAll)
	iv := NewInteractive(InteractiveConfig{
		Agent:                       &core.Agent{Tools: core.Registry{}},
		AutoSubagentsEnabled:        &enabled,
		AutoSubagentsToolAllowed:    &allowed,
		AutoSubagentsSystemAddendum: "primary-agent orchestrator",
		Supervisor:                  supervisor,
	})

	spawn, ok := iv.agent.ToolsSnapshot()["subagent_spawn"].(*tools.SubagentSpawnTool)
	if !ok {
		t.Fatal("subagent_spawn is unavailable when automatic subagents are disabled")
	}
	if spawn.Enabled == nil || !spawn.Enabled() {
		t.Fatal("subagent_spawn is disabled when a user explicitly requests delegation")
	}
	system, _ := iv.agent.PromptConfig()
	if strings.Contains(system, "primary-agent orchestrator") {
		t.Fatalf("disabled automatic subagents retained the orchestrator contract: %q", system)
	}
}

type serializedAutoSubagentsClient struct {
	started  chan struct{}
	request  chan provider.Request
	release  chan struct{}
	finished chan struct{}
}

func (c *serializedAutoSubagentsClient) Name() string { return "serialized-auto-subagents-test" }

func (c *serializedAutoSubagentsClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.request <- req
	c.started <- struct{}{}
	out := make(chan provider.Event, 1)
	go func() {
		defer close(out)
		select {
		case <-c.release:
			out <- provider.EventDone{Stop: provider.StopEnd}
		case <-ctx.Done():
			out <- provider.EventDone{Stop: provider.StopAborted, Err: ctx.Err()}
		}
		close(c.finished)
	}()
	return out, nil
}

func TestAutoSubagentsPromptConfigurationIsSynchronized(t *testing.T) {
	client := &serializedAutoSubagentsClient{
		started:  make(chan struct{}, 1),
		request:  make(chan provider.Request, 1),
		release:  make(chan struct{}),
		finished: make(chan struct{}),
	}
	agent := core.NewAgent(client, "test-model", "base system", nil)
	supervisor := subagents.New(subagents.Config{Root: t.TempDir(), RepoRoot: t.TempDir()})
	t.Cleanup(supervisor.StopAll)
	allowed := true
	iv := NewInteractive(InteractiveConfig{
		Agent:                       agent,
		Supervisor:                  supervisor,
		AutoSubagentsToolAllowed:    &allowed,
		AutoSubagentsSystemAddendum: "delegation guidance",
	})

	iv.startTurn(context.Background(), "hello")
	select {
	case <-client.started:
	case <-time.After(time.Second):
		t.Fatal("prompt did not reach provider")
	}

	request := <-client.request
	if request.System != "base system" {
		t.Fatalf("provider request system = %q; want base system snapshot", request.System)
	}
	applied := make(chan struct{})
	go func() {
		iv.applyAutoSubagentsSystemPrompt(true)
		close(applied)
	}()
	select {
	case <-applied:
	case <-time.After(time.Second):
		t.Fatal("system-prompt update did not apply while provider was running")
	}
	if system, _ := agent.PromptConfig(); !strings.Contains(system, "delegation guidance") {
		t.Fatalf("system prompt missing applied guidance: %q", system)
	}

	close(client.release)
	select {
	case <-client.finished:
	case <-time.After(time.Second):
		t.Fatal("prompt did not finish after provider release")
	}
}
