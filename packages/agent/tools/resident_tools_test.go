package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestResidentToolsUseManagerOnly(t *testing.T) {
	runs := make(chan string, 2)
	manager := subagents.NewResidentManager(t.TempDir(), func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
		return func(_ context.Context, prompt string) error { runs <- prompt; return nil }, nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	spawn := &SubagentSpawnTool{
		ResidentManager: manager,
		Enabled:         func() bool { return true },
		DefaultProvider: func() string { return "openai" },
		DefaultModel:    func() string { return "gpt-5" },
		BuildResidentSpec: func(_ context.Context, request ResidentSpawnRequest) (subagents.ResidentChildSpec, error) {
			return subagents.ResidentChildSpec{ID: "resident-tool", SessionID: "child-session", Provider: request.Provider, Model: request.Model, Required: request.Required}, nil
		},
	}
	result, err := spawn.Execute(context.Background(), json.RawMessage(`{"task":"initial"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := toolResultText(t, result); !strings.Contains(got, "owns the delegated scope") || !strings.Contains(got, "Do not repeat that work in the parent") {
		t.Fatalf("spawn result lacks ownership guidance: %q", got)
	}
	if got := <-runs; got != "initial" {
		t.Fatalf("initial prompt = %q", got)
	}
	status := &SubagentStatusTool{ResidentManager: manager, Enabled: func() bool { return true }}
	result, err = status.Execute(context.Background(), json.RawMessage(`{"agent_id":"resident-tool"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if details, ok := result.Details.(subagentStatusResponse); !ok || details.Agent == nil || details.Agent.State == "" {
		t.Fatalf("status details = %#v", result.Details)
	}
	resume := &SubagentResumeTool{ResidentManager: manager, Enabled: func() bool { return true }}
	if _, err := resume.Execute(context.Background(), json.RawMessage(`{"agent_id":"resident-tool","prompt":"follow up"}`), nil); err != nil {
		t.Fatal(err)
	}
	if got := <-runs; got != "follow up" {
		t.Fatalf("follow-up prompt = %q", got)
	}
}

func TestSubagentSpawnGuidanceRequiresIndependentOwnership(t *testing.T) {
	spawn := &SubagentSpawnTool{}
	for _, want := range []string{"independent sidecar", "keep immediate blockers local", "never duplicate it in the parent", "end or yield the parent turn"} {
		if got := spawn.Description(); !strings.Contains(got, want) {
			t.Fatalf("description missing %q: %s", want, got)
		}
	}
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(spawn.Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	task := schema.Properties["task"].Description
	for _, want := range []string{"concrete, bounded scope", "explicit ownership", "does not overlap"} {
		if !strings.Contains(task, want) {
			t.Fatalf("task schema missing %q: %s", want, task)
		}
	}
}

func TestResidentSpawnWaitReturnsInitialCompletion(t *testing.T) {
	manager := subagents.NewResidentManager(t.TempDir(), func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
		return func(context.Context, string) error { return nil }, nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	spawn := &SubagentSpawnTool{
		ResidentManager: manager,
		Enabled:         func() bool { return true },
		DefaultProvider: func() string { return "openai" },
		DefaultModel:    func() string { return "gpt-5" },
		BuildResidentSpec: func(_ context.Context, request ResidentSpawnRequest) (subagents.ResidentChildSpec, error) {
			return subagents.ResidentChildSpec{ID: "wait-for-completion", SessionID: "child-session", Provider: request.Provider, Model: request.Model}, nil
		},
	}
	result, err := spawn.Execute(context.Background(), json.RawMessage(`{"task":"finish now","wait":1}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("Execute = (%#v, %v)", result, err)
	}
	if got := toolResultText(t, result); !strings.Contains(got, "state: completed") {
		t.Fatalf("wait result = %q, want completed state", got)
	}
}

func TestResidentSpawnWaitExpiryReportsQueuedChild(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseBlocker := func() { releaseOnce.Do(func() { close(release) }) }
	finished := make(chan string, 1)
	manager := subagents.NewResidentManagerWithLimit(t.TempDir(), 1, func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
		return func(ctx context.Context, prompt string) error {
			switch prompt {
			case "blocker":
				started <- struct{}{}
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			case "waiter":
				finished <- prompt
			}
			return nil
		}, nil
	})
	t.Cleanup(func() {
		releaseBlocker()
		_ = manager.Close(context.Background())
	})
	spawn := &SubagentSpawnTool{
		ResidentManager: manager,
		Enabled:         func() bool { return true },
		DefaultProvider: func() string { return "openai" },
		DefaultModel:    func() string { return "gpt-5" },
		BuildResidentSpec: func(_ context.Context, request ResidentSpawnRequest) (subagents.ResidentChildSpec, error) {
			return subagents.ResidentChildSpec{ID: request.Task, SessionID: request.Task, Provider: request.Provider, Model: request.Model}, nil
		},
	}
	if _, err := spawn.Execute(context.Background(), json.RawMessage(`{"task":"blocker"}`), nil); err != nil {
		t.Fatalf("spawn blocker: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("blocker did not start")
	}
	result, err := spawn.Execute(context.Background(), json.RawMessage(`{"task":"waiter","wait":1}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("spawn waiter = (%#v, %v)", result, err)
	}
	if got := toolResultText(t, result); !strings.Contains(got, "state: queued") || !strings.Contains(got, "wait: timed out after 1 seconds") {
		t.Fatalf("wait expiry result = %q, want queued timeout", got)
	}
	if state, ok := manager.State("waiter"); !ok || state != subagents.ResidentQueued {
		t.Fatalf("waiter state = %q (found=%t), want queued", state, ok)
	}
	releaseBlocker()
	select {
	case got := <-finished:
		if got != "waiter" {
			t.Fatalf("finished task = %q, want waiter", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued waiter did not execute")
	}
}

func TestResidentSpawnRejectsInvalidInputBeforeCreatingChild(t *testing.T) {
	manager := subagents.NewResidentManager(t.TempDir(), func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
		return func(context.Context, string) error { return nil }, nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	built := 0
	spawn := &SubagentSpawnTool{
		ResidentManager: manager,
		Enabled:         func() bool { return true },
		BuildResidentSpec: func(context.Context, ResidentSpawnRequest) (subagents.ResidentChildSpec, error) {
			built++
			return subagents.ResidentChildSpec{ID: "should-not-exist", SessionID: "child"}, nil
		},
	}
	for _, raw := range []string{
		`{"task":"x","unexpected":true}`,
		`{"task":"x"}{}`,
		`{"task":"x","model":"gpt-5"}`,
		`{"task":"x","wait":0}`,
		`{"task":"x","wait":301}`,
		`{"task":"x","isolation":"outside"}`,
	} {
		result, err := spawn.Execute(context.Background(), json.RawMessage(raw), nil)
		if err != nil && !strings.Contains(err.Error(), "invalid args") {
			t.Fatalf("Execute(%s) error = %v", raw, err)
		}
		if err == nil && !result.IsError {
			t.Fatalf("Execute(%s) = %#v, want protocol error", raw, result)
		}
	}
	if built != 0 || len(manager.Snapshot()) != 0 {
		t.Fatalf("invalid input reached resident manager: built=%d snapshots=%#v", built, manager.Snapshot())
	}
}

func TestResidentSpawnProfileAndExplicitOverridesReachFactory(t *testing.T) {
	manager := subagents.NewResidentManager(t.TempDir(), func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
		return func(context.Context, string) error { return nil }, nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	profileFast := false
	var got ResidentSpawnRequest
	spawn := &SubagentSpawnTool{
		ResidentManager:  manager,
		Enabled:          func() bool { return true },
		DefaultModel:     func() string { return "host-model" },
		DefaultProvider:  func() string { return "host-provider" },
		DefaultReasoning: func() string { return "medium" },
		ResolveSubagent: func(name string) (*subagents.Profile, error) {
			return &subagents.Profile{Name: name, Model: "openai/gpt-5.6-sol", Thinking: "low", FastMode: &profileFast}, nil
		},
		BuildResidentSpec: func(_ context.Context, request ResidentSpawnRequest) (subagents.ResidentChildSpec, error) {
			got = request
			return subagents.ResidentChildSpec{ID: "profiled", SessionID: "child", Provider: request.Provider, Model: request.Model, Profile: request.Profile.Name}, nil
		},
	}
	result, err := spawn.Execute(context.Background(), json.RawMessage(`{"task":"review","agent":"reviewer","reasoning":"high","fast_mode":true}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("Execute = (%#v, %v)", result, err)
	}
	if got.Model != "gpt-5.6-sol" || got.Provider != "openai" || got.Reasoning != "high" || got.FastMode == nil || !*got.FastMode {
		t.Fatalf("factory request = %#v", got)
	}
}

func TestResidentToolsRejectUnknownFields(t *testing.T) {
	for _, tool := range []interface {
		Execute(context.Context, json.RawMessage, func(string)) (core.ToolResult, error)
	}{
		&SubagentStatusTool{ResidentManager: subagents.NewResidentManager(t.TempDir(), nil), Enabled: func() bool { return true }},
		&SubagentStopTool{ResidentManager: subagents.NewResidentManager(t.TempDir(), nil), Enabled: func() bool { return true }},
		&SubagentResumeTool{ResidentManager: subagents.NewResidentManager(t.TempDir(), nil), Enabled: func() bool { return true }},
	} {
		_, err := tool.Execute(context.Background(), json.RawMessage(`{"unknown":true}`), nil)
		if err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("unknown field error = %v", err)
		}
	}
}

func toolResultText(t *testing.T, result core.ToolResult) string {
	t.Helper()
	if len(result.Content) != 1 {
		t.Fatalf("content = %#v, want one text block", result.Content)
	}
	block, ok := result.Content[0].(provider.TextBlock)
	if !ok {
		t.Fatalf("content = %#v, want text block", result.Content)
	}
	return block.Text
}

func TestFindResidentStatusSnapshotRejectsAmbiguousPrefix(t *testing.T) {
	_, ok := findResidentStatusSnapshot([]subagents.ResidentSnapshot{{ID: "resident-abcd"}, {ID: "resident-abef"}}, "resident-ab")
	if ok {
		t.Fatal("ambiguous resident prefix resolved")
	}
}
