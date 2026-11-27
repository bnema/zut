package tools

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
	spec := subagents.ResidentChildSpec{ParentSessionID: "parent-session", ID: "watched", InitialTurnID: "initial", SessionID: "session", Provider: "openai", Model: "test"}
	if _, err := manager.Spawn(t.Context(), spec, "investigate"); err != nil {
		t.Fatal(err)
	}
	<-ready
	tool := &SubagentStatusTool{SessionScope: testResidentSessionScope, ResidentManager: manager, Enabled: func() bool { return true }}

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
	tool := &SubagentStatusTool{SessionScope: testResidentSessionScope, ResidentManager: manager, Enabled: func() bool { return true }}
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
			spec := subagents.ResidentChildSpec{ParentSessionID: "parent-session", ID: "status-error", InitialTurnID: "initial", SessionID: "session", Provider: "openai", Model: "test"}
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
			tool := &SubagentStatusTool{SessionScope: testResidentSessionScope, ResidentManager: statusManager, Enabled: func() bool { return true }}
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

func testResidentSessionScope() []string { return []string{"parent-session"} }

func TestResidentStatusSessionScope(t *testing.T) {
	root := t.TempDir()
	owner := subagents.NewResidentManager(root, func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentRuntime, error) {
		return subagents.ResidentTurnRunner(func(context.Context, string) error { return nil }), nil
	})
	t.Cleanup(func() { _ = owner.Close(context.Background()) })
	const outsideID = "12345678-2222-4222-8222-222222222222"
	for _, spec := range []subagents.ResidentChildSpec{
		{ID: "12345678-1111-4111-8111-111111111111", ParentSessionID: "current"},
		{ID: "ancestor-child", ParentSessionID: "ancestor"},
		{ID: outsideID, ParentSessionID: "unrelated"},
		{ID: "unknown-owner"},
	} {
		spec.SessionID, spec.InitialTurnID = spec.ID, "initial"
		spec.Provider, spec.Model = "openai", "test"
		if _, err := owner.Spawn(t.Context(), spec, "task"); err != nil {
			t.Fatal(err)
		}
	}

	manager := subagents.NewResidentManager(root, nil)
	if errs := manager.Reconcile(); len(errs) != 0 {
		t.Fatal(errs)
	}

	scope := []string{"current", "ancestor", ""}
	tool := &SubagentStatusTool{ResidentManager: manager, Enabled: func() bool { return true }, SessionScope: func() []string { return scope }}
	status := func(args string) subagentStatusResponse {
		t.Helper()
		result, err := tool.Execute(t.Context(), json.RawMessage(args), nil)
		if err != nil || result.IsError {
			t.Fatalf("status(%s) = %#v, %v", args, result, err)
		}
		var response subagentStatusResponse
		if err := json.Unmarshal([]byte(toolResultText(t, result)), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	listed := status(`{}`)
	if listed.Total == nil || *listed.Total != 2 || listed.Truncated || len(listed.Agents) != 2 ||
		!slices.ContainsFunc(listed.Agents, func(e subagentStatusEntry) bool { return e.ID == "12345678-1111-4111-8111-111111111111" }) ||
		!slices.ContainsFunc(listed.Agents, func(e subagentStatusEntry) bool { return e.ID == "ancestor-child" }) {
		t.Fatalf("in-scope list = %#v", listed)
	}
	if got := status(`{"agent_id":"12345678"}`).Agent; got == nil || got.ID != "12345678-1111-4111-8111-111111111111" {
		t.Fatalf("scoped prefix = %#v", got)
	}
	result, err := tool.Execute(t.Context(), json.RawMessage(`{"agent_id":"`+outsideID+`"}`), nil)
	if err != nil || !result.IsError || !strings.Contains(toolResultText(t, result), "no such agent") {
		t.Fatalf("out-of-scope exact ID = %#v, %v", result, err)
	}
	facade := &SubagentTool{
		Stop:      &SubagentStopTool{ResidentManager: manager, Enabled: tool.Enabled, SessionScope: tool.SessionScope},
		Resume:    &SubagentResumeTool{ResidentManager: manager, Enabled: tool.Enabled, SessionScope: tool.SessionScope},
		Interrupt: &SubagentInterruptTool{ResidentManager: manager, Enabled: tool.Enabled, SessionScope: tool.SessionScope},
	}
	for _, action := range []string{"stop", "resume", "interrupt"} {
		args := map[string]string{"action": action, "agent_id": outsideID}
		if action == "resume" {
			args["prompt"] = "continue"
		}
		raw, _ := json.Marshal(args)
		result, err := facade.Execute(t.Context(), raw, nil)
		if err != nil || !result.IsError || !strings.Contains(toolResultText(t, result), "no such agent") {
			t.Fatalf("%s: %#v, %v", action, result, err)
		}
	}
	scope = nil
	if got := status(`{}`).Agents; got == nil || len(got) != 0 {
		t.Fatalf("empty scope list = %#v, want []", got)
	}
	tool.SessionScope = nil
	if got := status(`{}`).Agents; got == nil || len(got) != 0 {
		t.Fatalf("missing scope list = %#v, want []", got)
	}
}

func TestResidentStatusListLimit(t *testing.T) {
	oldest := time.Unix(1, 0)
	snapshots := []subagents.ResidentSnapshot{
		{ID: "running", State: subagents.ResidentRunning, UpdatedAt: oldest},
		{ID: "queued", State: subagents.ResidentQueued, UpdatedAt: oldest},
	}
	for i := 0; i < 22; i++ {
		snapshots = append(snapshots, subagents.ResidentSnapshot{
			ID: fmt.Sprintf("idle-%02d", i), State: subagents.ResidentIdle,
			UpdatedAt: oldest.Add(time.Duration(i/2+1) * time.Second),
		})
	}
	for _, tc := range []struct {
		name         string
		total, count int
	}{
		{"over limit", 24, 20},
		{"at limit", 20, 20},
		{"empty", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := renderSubagentStatus(listResidentStatus(slices.Clone(snapshots[:tc.total])))
			if err != nil {
				t.Fatal(err)
			}
			var response struct {
				Agents    []subagentStatusEntry `json:"agents"`
				Total     *int                  `json:"total"`
				Truncated *bool                 `json:"truncated"`
			}
			if err := json.Unmarshal([]byte(toolResultText(t, result)), &response); err != nil {
				t.Fatal(err)
			}
			if response.Total == nil || *response.Total != tc.total || response.Agents == nil || len(response.Agents) != tc.count {
				t.Fatalf("incorrect list size or total: %#v", response)
			}
			if tc.total > tc.count {
				if response.Truncated == nil || !*response.Truncated {
					t.Fatal("expected truncated: true")
				}
				want := []string{"queued", "running"}
				for i := 20; i >= 4; i -= 2 {
					want = append(want, fmt.Sprintf("idle-%02d", i), fmt.Sprintf("idle-%02d", i+1))
				}
				for i, entry := range response.Agents {
					if entry.ID != want[i] {
						t.Fatalf("entry %d = %q, want %q", i, entry.ID, want[i])
					}
				}
			} else if response.Truncated != nil {
				t.Fatal("truncated should be absent")
			}
		})
	}
}
