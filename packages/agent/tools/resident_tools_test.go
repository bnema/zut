package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
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
	if len(result.Content) == 0 {
		t.Fatal("spawn returned no result")
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

func TestFindResidentStatusSnapshotRejectsAmbiguousPrefix(t *testing.T) {
	_, ok := findResidentStatusSnapshot([]subagents.ResidentSnapshot{{ID: "resident-abcd"}, {ID: "resident-abef"}}, "resident-ab")
	if ok {
		t.Fatal("ambiguous resident prefix resolved")
	}
}
