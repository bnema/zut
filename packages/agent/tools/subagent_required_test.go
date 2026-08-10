package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/subagents"
)

func TestRequiredSubagentSpawnReturnsWhileWorkerRuns(t *testing.T) {
	root := t.TempDir()
	started := make(chan struct{})
	release := make(chan struct{})
	manager := subagents.New(subagents.Config{
		Root:     filepath.Join(root, "subagents"),
		RepoRoot: root,
		NewRunner: func(*subagents.Agent) subagents.Runner {
			return subagents.RunnerFunc(func(ctx context.Context, _ subagents.Sink) error {
				close(started)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		},
	})
	t.Cleanup(manager.StopAll)

	spawned := make(chan bool, 1)
	tool := &SubagentSpawnTool{
		Supervisor: manager,
		Enabled:    func() bool { return true },
		OnSpawned: func(_ *subagents.Agent, _ string, required bool) {
			spawned <- required
		},
	}
	got, err := tool.Execute(context.Background(), json.RawMessage(`{"task":"finish the release","required":true}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	text := textResult(got.Content)
	if got.IsError || !strings.Contains(text, "required: pending") || !strings.Contains(text, "manager remains free") {
		t.Fatalf("required spawn result = %#v\n%s", got, text)
	}
	if details, ok := got.Details.(map[string]any); !ok || details["required"] != true || details["requirement_state"] != "pending" {
		t.Fatalf("required details = %#v", got.Details)
	}
	select {
	case required := <-spawned:
		if !required {
			t.Fatal("required callback was reported as optional")
		}
	default:
		t.Fatal("required spawn callback was not delivered")
	}

	close(release)
	manager.List()[0].Wait()
}

func TestRequiredSubagentResumeRetriesUnmetWorkWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	attempt := 0
	retryStarted := make(chan struct{})
	releaseRetry := make(chan struct{})
	manager := subagents.New(subagents.Config{
		Root:     filepath.Join(root, "subagents"),
		RepoRoot: root,
		NewRunner: func(*subagents.Agent) subagents.Runner {
			attempt++
			current := attempt
			return subagents.RunnerFunc(func(ctx context.Context, _ subagents.Sink) error {
				if current == 1 {
					return errors.New("first review failed")
				}
				close(retryStarted)
				select {
				case <-releaseRetry:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		},
	})
	t.Cleanup(manager.StopAll)
	spawn := &SubagentSpawnTool{Supervisor: manager, Enabled: func() bool { return true }}
	first, err := spawn.Execute(context.Background(), json.RawMessage(`{"task":"review","required":true}`), nil)
	if err != nil || first.IsError {
		t.Fatalf("required spawn = (%+v, %v), want asynchronous success", first, err)
	}
	worker := manager.List()[0]
	worker.Wait()
	if !worker.Snapshot().Requirement.Unmet() {
		t.Fatal("failed required worker was not retained as unmet")
	}

	var callbackRequired bool
	resume := &SubagentResumeTool{
		Supervisor: manager,
		Enabled:    func() bool { return true },
		BeforeResumed: func(_ *subagents.Agent, _ string, required bool) func() {
			callbackRequired = required
			return nil
		},
	}
	got, err := resume.Execute(context.Background(), json.RawMessage(fmt.Sprintf(`{"agent_id":%q,"prompt":"retry"}`, worker.ID)), nil)
	if err != nil {
		t.Fatal(err)
	}
	<-retryStarted
	if got.IsError || !callbackRequired || !strings.Contains(textResult(got.Content), `"requirement":{"state":"pending"`) {
		t.Fatalf("required retry = %#v callback_required=%v\n%s", got, callbackRequired, textResult(got.Content))
	}
	close(releaseRetry)
	manager.List()[0].Wait()
	if state := manager.List()[0].Snapshot().Requirement.State; state != subagents.RequirementSatisfied {
		t.Fatalf("required retry state = %s, want satisfied", state)
	}
}

func TestIndeterminateRequiredWorkNeedsExplicitUserReconciliation(t *testing.T) {
	root := t.TempDir()
	started := make(chan struct{})
	first := subagents.New(subagents.Config{
		Root: filepath.Join(root, "subagents"), RepoRoot: root,
		NewRunner: func(*subagents.Agent) subagents.Runner {
			return subagents.RunnerFunc(func(ctx context.Context, _ subagents.Sink) error {
				close(started)
				<-ctx.Done()
				return ctx.Err()
			})
		},
	})
	a, err := first.SpawnReq(context.Background(), subagents.SpawnRequest{Task: "non-idempotent release action", Required: true})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	t.Cleanup(first.StopAll)

	runs := 0
	second := subagents.New(subagents.Config{
		Root: filepath.Join(root, "subagents"), RepoRoot: root,
		NewRunner: func(*subagents.Agent) subagents.Runner {
			runs++
			return subagents.RunnerFunc(func(context.Context, subagents.Sink) error { return nil })
		},
	})
	t.Cleanup(second.StopAll)
	if _, errs := second.Reload(); len(errs) != 0 {
		t.Fatalf("reload errors: %v", errs)
	}
	resume := &SubagentResumeTool{Supervisor: second, Enabled: func() bool { return true }}
	got, err := resume.Execute(context.Background(), json.RawMessage(fmt.Sprintf(`{"agent_id":%q,"prompt":"retry"}`, a.ID)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsError || runs != 0 || !strings.Contains(textResult(got.Content), "user must inspect") {
		t.Fatalf("automatic indeterminate retry = %#v runs=%d\n%s", got, runs, textResult(got.Content))
	}
	first.StopAll()

	confirmed, err := second.RestartTask(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("explicit user restart: %v", err)
	}
	confirmed.Wait()
	if runs != 1 || confirmed.Snapshot().Requirement.State != subagents.RequirementSatisfied {
		t.Fatalf("explicit reconciliation runs=%d requirement=%+v", runs, confirmed.Snapshot().Requirement)
	}
}

func TestRequiredSubagentFailureStaysUnmet(t *testing.T) {
	root := t.TempDir()
	manager := subagents.New(subagents.Config{
		Root:     filepath.Join(root, "subagents"),
		RepoRoot: root,
		NewRunner: func(*subagents.Agent) subagents.Runner {
			return subagents.RunnerFunc(func(context.Context, subagents.Sink) error {
				return errors.New("finalizer failed")
			})
		},
	})
	t.Cleanup(manager.StopAll)
	tool := &SubagentSpawnTool{Supervisor: manager, Enabled: func() bool { return true }}

	got, err := tool.Execute(context.Background(), json.RawMessage(`{"task":"finish the release","required":true}`), nil)
	if err != nil || got.IsError || !strings.Contains(textResult(got.Content), "required: pending") {
		t.Fatalf("required spawn = (%+v, %v), want asynchronous pending result", got, err)
	}
	agents := manager.List()
	if len(agents) != 1 {
		t.Fatalf("spawned agents = %d, want 1", len(agents))
	}
	agents[0].Wait()
	if !agents[0].Snapshot().Requirement.Unmet() {
		t.Fatalf("failed required worker was not retained as unmet: %#v", agents)
	}
	status := &SubagentStatusTool{Supervisor: manager, Enabled: func() bool { return true }}
	public, err := status.Execute(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	publicText := textResult(public.Content)
	if !strings.Contains(publicText, `"error_code":"failed"`) || strings.Contains(publicText, "finalizer failed") {
		t.Fatalf("public requirement status exposed raw diagnostics: %s", publicText)
	}
}
