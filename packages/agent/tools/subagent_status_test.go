package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
)

func TestResidentStatusWatchReportsLiveActivity(t *testing.T) {
	ready := make(chan struct{})
	release := make(chan struct{})
	manager := subagents.NewResidentManager(t.TempDir(), func(_ subagents.ResidentChildSpec, journal *subagents.ResidentJournal) (subagents.ResidentRuntime, error) {
		return subagents.ResidentTurnRunner(func(ctx context.Context, _ string) error {
			_ = journal.RecordAgentEvent(core.EvTextDelta{Delta: "Reading the parser"})
			_ = journal.RecordAgentEvent(core.EvToolExecutionStarted{ID: "call-1", Name: "read"})
			close(ready)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}), nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	spec := subagents.ResidentChildSpec{ID: "watched", InitialTurnID: "initial", SessionID: "session", Provider: "openai", Model: "test"}
	if _, err := manager.Spawn(t.Context(), spec, "investigate"); err != nil {
		t.Fatal(err)
	}
	<-ready
	tool := &SubagentStatusTool{ResidentManager: manager, Enabled: func() bool { return true }}

	result, err := tool.Execute(t.Context(), json.RawMessage(`{"agent_id":"watched","watch":1}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
	var response subagentStatusResponse
	if err := json.Unmarshal([]byte(toolResultText(t, result)), &response); err != nil {
		t.Fatal(err)
	}
	if response.Watch == nil || len(response.Watch.Timeline) == 0 || response.Watch.EndState != subagents.ResidentRunning {
		t.Fatalf("watch = %#v", response.Watch)
	}
	activity := response.Activity
	if activity == nil || activity.Phase != "tools" || activity.Text != "Reading the parser" || len(activity.Tools) != 1 || activity.Tools[0].Name != "read" {
		t.Fatalf("activity = %#v", activity)
	}

	close(release)
	result, err = tool.Execute(t.Context(), json.RawMessage(`{"agent_id":"watched","watch":60}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
	response = subagentStatusResponse{}
	if err := json.Unmarshal([]byte(toolResultText(t, result)), &response); err != nil {
		t.Fatal(err)
	}
	if response.Watch == nil || response.Watch.EndState == subagents.ResidentRunning || response.Watch.Seconds >= 30 || response.Activity != nil {
		t.Fatalf("watch did not end with the turn: %#v", response)
	}
}

func TestResidentStatusWatchValidation(t *testing.T) {
	manager := subagents.NewResidentManager(t.TempDir(), nil)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	tool := &SubagentStatusTool{ResidentManager: manager, Enabled: func() bool { return true }}
	result, err := tool.Execute(t.Context(), json.RawMessage(`{"watch":5}`), nil)
	if err != nil || !result.IsError || !strings.Contains(toolResultText(t, result), "watch requires agent_id") {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
}

func TestResidentStatusResultReadErrors(t *testing.T) {
	for _, scenario := range []struct {
		name string
		want string
	}{
		{"missing", "no saved result exists yet"},
		{"foreign", "owned by another zut process"},
		{"corrupt", "could not read or decode the saved result"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := t.TempDir()
			manager := subagents.NewResidentManager(root, func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentRuntime, error) {
				return subagents.ResidentTurnRunner(func(ctx context.Context, _ string) error {
					<-ctx.Done()
					return ctx.Err()
				}), nil
			})
			t.Cleanup(func() { _ = manager.Close(context.Background()) })
			spec := subagents.ResidentChildSpec{ID: "status-error", InitialTurnID: "initial", SessionID: "session", Provider: "openai", Model: "test"}
			if _, err := manager.Spawn(t.Context(), spec, "investigate"); err != nil {
				t.Fatal(err)
			}
			statusManager := manager
			if scenario.name == "foreign" {
				statusManager = subagents.NewResidentManager(root, nil)
				t.Cleanup(func() { _ = statusManager.Close(context.Background()) })
				if errs := statusManager.Reconcile(); len(errs) != 0 {
					t.Fatal(errs)
				}
			}
			if scenario.name == "corrupt" {
				if err := os.WriteFile(filepath.Join(root, spec.ID, "result.json"), []byte("private malformed payload"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			tool := &SubagentStatusTool{ResidentManager: statusManager, Enabled: func() bool { return true }}
			result, err := tool.Execute(t.Context(), json.RawMessage(`{"agent_id":"status-error","include_result":true}`), nil)
			if err != nil || !result.IsError {
				t.Fatalf("result = %#v, error = %v", result, err)
			}
			text := toolResultText(t, result)
			if !strings.Contains(text, scenario.want) || strings.Contains(text, root) || strings.Contains(text, "private") {
				t.Fatalf("unexpected error text: %q", text)
			}
		})
	}
}
