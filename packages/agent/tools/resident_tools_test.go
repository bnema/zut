package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestResidentStatusRetrievesFailedResultWithoutExecution(t *testing.T) {
	failure := errors.New("worker failed")
	manager := subagents.NewResidentManager(t.TempDir(), func(_ subagents.ResidentChildSpec, journal *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
		return func(context.Context, string) error {
			if err := journal.RecordAgentEvent(core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "partial finding"}}}}); err != nil {
				return err
			}
			return failure
		}, nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	spec := subagents.ResidentChildSpec{ID: "failed-status", InitialTurnID: "initial", SessionID: "session", Provider: "openai", Model: "test", Required: true}
	completed, cancel := manager.WatchCompletion(spec.ID, spec.InitialTurnID)
	defer cancel()
	if _, err := manager.Spawn(t.Context(), spec, "investigate"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("no completion")
	}
	status := &SubagentStatusTool{ResidentManager: manager, Enabled: func() bool { return true }}
	for _, include := range []bool{false, true} {
		raw := json.RawMessage(`{"agent_id":"failed-status"}`)
		if include {
			raw = json.RawMessage(`{"agent_id":"failed-status","include_result":true}`)
		}
		result, err := status.Execute(t.Context(), raw, nil)
		if err != nil || result.IsError {
			t.Fatalf("status: %#v, %v", result, err)
		}
		response := result.Details.(subagentStatusResponse)
		if response.Agent.State != subagents.ResidentFailed || (response.Result != nil) != include {
			t.Fatalf("response = %#v", response)
		}
		if include && (response.Result.Handoff != "" || response.Result.ErrorCode != "turn_failed" || response.Result.Summary != "partial finding") {
			t.Fatalf("result = %#v", response.Result)
		}
	}
	result, err := status.Execute(t.Context(), json.RawMessage(`{"include_result":true}`), nil)
	if err != nil || !result.IsError {
		t.Fatalf("missing child ID must fail: %#v, %v", result, err)
	}
	if len(manager.UnmetRequired()) != 1 {
		t.Fatal("reading a result satisfied required work")
	}
}

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
	facade := &SubagentTool{}
	for _, want := range []string{"independent sidecar", "keep immediate blockers local", "never duplicate it in the parent", "end or yield the parent turn"} {
		if got := facade.Description(); !strings.Contains(got, want) {
			t.Fatalf("description missing %q: %s", want, got)
		}
	}
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(facade.Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	task := schema.Properties["task"].Description
	for _, want := range []string{"concrete, bounded scope", "explicit ownership", "does not overlap", "Shared isolation uses this working directory", "worktree isolation captures a patch without merging it"} {
		if !strings.Contains(task, want) {
			t.Fatalf("task schema missing %q: %s", want, task)
		}
	}
}

func TestResidentSpawnWaitReturnsInitialCompletion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status string
	}{
		{"success", nil, "completed"},
		{"failure", errors.New("worker failed"), "failed"},
		{"cancellation", context.Canceled, "interrupted"},
		{"cancellation with failure", errors.Join(context.Canceled, errors.New("worker failed")), "interrupted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := subagents.NewResidentManager(t.TempDir(), func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
				return func(context.Context, string) error { return tc.err }, nil
			})
			t.Cleanup(func() { _ = manager.Close(context.Background()) })
			spawn := &SubagentSpawnTool{
				ResidentManager: manager,
				Enabled:         func() bool { return true },
				DefaultProvider: func() string { return "openai" },
				DefaultModel:    func() string { return "gpt-5" },
				BuildResidentSpec: func(_ context.Context, request ResidentSpawnRequest) (subagents.ResidentChildSpec, error) {
					return subagents.ResidentChildSpec{ID: "wait-for-completion", SessionID: "child-session", Provider: request.Provider, Model: request.Model, Required: request.Required}, nil
				},
			}
			result, err := spawn.Execute(t.Context(), json.RawMessage(`{"task":"finish now","wait":1,"required":true}`), nil)
			if err != nil || result.IsError {
				t.Fatalf("Execute = (%#v, %v)", result, err)
			}
			if got := toolResultText(t, result); !strings.Contains(got, "\nstate: "+tc.status+"\n") || !strings.Contains(got, "\nrequired: "+tc.status+"\n") {
				t.Fatalf("wait result = %q, want %s state", got, tc.status)
			}
			if unmet := len(manager.UnmetRequired()); (unmet != 0) != (tc.err != nil) {
				t.Fatalf("unmet required = %d, error = %v", unmet, tc.err)
			}
		})
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
		`{"task":"x","budget_ratio":0.5}`,
		`{"task":"x","budget_tokens":1000}`,
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

// A resume wait subscribes to the accepted follow-up turn before it can finish,
// so it returns that turn's completion instead of timing out.
func TestResidentResumeWaitReturnsFollowUpCompletion(t *testing.T) {
	manager := subagents.NewResidentManager(t.TempDir(), func(_ subagents.ResidentChildSpec, journal *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
		return func(_ context.Context, prompt string) error {
			return journal.RecordAgentEvent(core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "answer for " + prompt}}}})
		}, nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	initial, cancelInitial := manager.WatchCompletion("waited-resume", "initial-turn")
	defer cancelInitial()
	if _, err := manager.Spawn(context.Background(), subagents.ResidentChildSpec{ID: "waited-resume", InitialTurnID: "initial-turn", SessionID: "child-session", Provider: "openai", Model: "gpt-5"}, "start"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-initial:
	case <-time.After(5 * time.Second):
		t.Fatal("initial turn did not complete")
	}
	resume := &SubagentResumeTool{ResidentManager: manager, Enabled: func() bool { return true }}
	result, err := resume.Execute(context.Background(), json.RawMessage(`{"agent_id":"waited-resume","prompt":"confirm","wait":5}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("resume = (%#v, %v)", result, err)
	}
	response, ok := result.Details.(subagentActionResponse)
	if !ok || response.Wait == nil || response.Wait.TimedOut {
		t.Fatalf("wait outcome = %#v", result.Details)
	}
	if response.Wait.Status != string(subagents.ResidentCompleted) || response.Wait.Summary != "answer for confirm" {
		t.Fatalf("wait outcome = %#v", response.Wait)
	}
	if response.Action != "resumed" || response.Agent.ID != "waited-resume" {
		t.Fatalf("response = %#v", response)
	}
}

// An expired resume wait reports the timeout and leaves the child active.
func TestResidentResumeWaitExpiryLeavesChildActive(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseBlocker := func() { releaseOnce.Do(func() { close(release) }) }
	manager := subagents.NewResidentManager(t.TempDir(), func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
		return func(ctx context.Context, _ string) error {
			started <- struct{}{}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}, nil
	})
	t.Cleanup(func() {
		releaseBlocker()
		_ = manager.Close(context.Background())
	})
	if _, err := manager.Spawn(context.Background(), subagents.ResidentChildSpec{ID: "blocked-resume", InitialTurnID: "initial-turn", SessionID: "child-session", Provider: "openai", Model: "gpt-5"}, "blocker"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking turn did not start")
	}
	resume := &SubagentResumeTool{ResidentManager: manager, Enabled: func() bool { return true }}
	result, err := resume.Execute(context.Background(), json.RawMessage(`{"agent_id":"blocked-resume","prompt":"follow up","wait":1}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("resume = (%#v, %v)", result, err)
	}
	response, ok := result.Details.(subagentActionResponse)
	if !ok || response.Wait == nil || !response.Wait.TimedOut || response.Wait.Seconds != 1 {
		t.Fatalf("wait outcome = %#v", result.Details)
	}
	if state, found := manager.State("blocked-resume"); !found || state != subagents.ResidentRunning {
		t.Fatalf("child state = %q (found=%t), want running", state, found)
	}
	releaseBlocker()
}

// A cancelled parent context wins over an expiring wait: the tool returns the
// host context error rather than a timeout result.
func TestResidentResumeWaitCancellationBeatsTimeout(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseBlocker := func() { releaseOnce.Do(func() { close(release) }) }
	var calls atomic.Int32
	followUpStarted := make(chan struct{}, 1)
	manager := subagents.NewResidentManager(t.TempDir(), func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
		return func(ctx context.Context, _ string) error {
			if calls.Add(1) == 1 {
				return nil
			}
			followUpStarted <- struct{}{}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}, nil
	})
	t.Cleanup(func() {
		releaseBlocker()
		_ = manager.Close(context.Background())
	})
	initial, cancelInitial := manager.WatchCompletion("cancel-resume", "initial-turn")
	defer cancelInitial()
	if _, err := manager.Spawn(context.Background(), subagents.ResidentChildSpec{ID: "cancel-resume", InitialTurnID: "initial-turn", SessionID: "child-session", Provider: "openai", Model: "gpt-5"}, "start"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-initial:
	case <-time.After(5 * time.Second):
		t.Fatal("initial turn did not complete")
	}

	resume := &SubagentResumeTool{ResidentManager: manager, Enabled: func() bool { return true }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		result core.ToolResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := resume.Execute(ctx, json.RawMessage(`{"agent_id":"cancel-resume","prompt":"follow up","wait":300}`), nil)
		done <- outcome{result: result, err: err}
	}()
	select {
	case <-followUpStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("follow-up turn did not start")
	}
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("Execute error = %v, want context.Canceled (result=%#v)", got.err, got.result)
		}
		if got.result.IsError || len(got.result.Content) != 0 {
			t.Fatalf("result = %#v, want an empty non-error result on host cancellation", got.result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resume did not return after cancellation")
	}
}

func TestResidentResumeRejectsWaitOutsideBounds(t *testing.T) {
	manager := subagents.NewResidentManager(t.TempDir(), nil)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	resume := &SubagentResumeTool{ResidentManager: manager, Enabled: func() bool { return true }}
	for _, raw := range []string{
		`{"agent_id":"child","prompt":"x","wait":0}`,
		`{"agent_id":"child","prompt":"x","wait":301}`,
	} {
		result, err := resume.Execute(context.Background(), json.RawMessage(raw), nil)
		if err != nil || !result.IsError || !strings.Contains(toolResultText(t, result), "wait must be between 1 and 300 seconds") {
			t.Fatalf("Execute(%s) = (%#v, %v)", raw, result, err)
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

func TestPublicResidentStatusOmitsBudget(t *testing.T) {
	entry := publicResidentStatus(subagents.ResidentSnapshot{
		ID: "resident", State: subagents.ResidentIdle, Provider: "openai", Model: "test",
	})
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "budget") {
		t.Fatalf("status entry carries budget = %s", encoded)
	}
}

func TestFindResidentStatusSnapshotRejectsAmbiguousPrefix(t *testing.T) {
	_, ok := findResidentStatusSnapshot([]subagents.ResidentSnapshot{{ID: "resident-abcd"}, {ID: "resident-abef"}}, "resident-ab")
	if ok {
		t.Fatal("ambiguous resident prefix resolved")
	}
}
