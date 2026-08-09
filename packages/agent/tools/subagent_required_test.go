package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
)

func TestRequiredSubagentSpawnBlocksUntilTerminalOutcome(t *testing.T) {
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
	result := make(chan core.ToolResult, 1)
	errCh := make(chan error, 1)
	go func() {
		got, err := tool.Execute(context.Background(), json.RawMessage(`{"task":"finish the release","required":true}`), nil)
		if err != nil {
			errCh <- err
			return
		}
		result <- got
	}()

	<-started
	select {
	case got := <-result:
		t.Fatalf("required spawn returned while worker was running: %s", textResult(got.Content))
	case err := <-errCh:
		t.Fatalf("required spawn failed while worker was running: %v", err)
	default:
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	select {
	case err := <-errCh:
		t.Fatalf("required spawn failed: %v", err)
	case got := <-result:
		text := textResult(got.Content)
		if got.IsError || !strings.Contains(text, "required") || !strings.Contains(text, "satisfied") {
			t.Fatalf("required result = %#v\n%s", got, text)
		}
		if details, ok := got.Details.(map[string]any); !ok || details["required"] != true || details["requirement_state"] != "satisfied" {
			t.Fatalf("required details = %#v", got.Details)
		}
	case <-ctx.Done():
		t.Fatal("required spawn did not return after worker completion")
	}
}

func TestRequiredSubagentResumeRetriesUnmetWorkAsRequired(t *testing.T) {
	root := t.TempDir()
	attempt := 0
	manager := subagents.New(subagents.Config{
		Root:     filepath.Join(root, "subagents"),
		RepoRoot: root,
		NewRunner: func(*subagents.Agent) subagents.Runner {
			attempt++
			current := attempt
			return subagents.RunnerFunc(func(context.Context, subagents.Sink) error {
				if current == 1 {
					return errors.New("first review failed")
				}
				return nil
			})
		},
	})
	t.Cleanup(manager.StopAll)
	spawn := &SubagentSpawnTool{Supervisor: manager, Enabled: func() bool { return true }}
	first, err := spawn.Execute(context.Background(), json.RawMessage(`{"task":"review","required":true}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !first.IsError {
		t.Fatalf("first required result should remain unmet: %s", textResult(first.Content))
	}
	worker := manager.List()[0]
	worker.Wait()

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
	if got.IsError || !callbackRequired || !strings.Contains(textResult(got.Content), "satisfied") {
		t.Fatalf("required retry = %#v callback_required=%v\n%s", got, callbackRequired, textResult(got.Content))
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
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(textResult(got.Content), "failed") {
		t.Fatalf("required failure was not surfaced: %s", textResult(got.Content))
	}
	agents := manager.List()
	if len(agents) != 1 || !agents[0].Snapshot().Requirement.Unmet() {
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
