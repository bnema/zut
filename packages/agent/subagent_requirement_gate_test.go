package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestRequiredWorkerGateWaitsBeforeParentModelTurn(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	cfg := newRuntimeTestConfig(t.TempDir(), t.TempDir(), func(*subagents.Agent) subagents.Runner {
		return subagents.RunnerFunc(func(ctx context.Context, _ subagents.Sink) error {
			close(started)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	})
	rt := newSubagentRuntime(cfg)
	defer func() { _ = rt.Close(context.Background()) }()
	rt.SetActiveSession("parent-session")
	a, err := rt.Supervisor().SpawnReq(context.Background(), subagents.SpawnRequest{
		Task:          "required review",
		RootSessionID: "parent-session",
		Required:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started

	parent := &core.Agent{}
	rt.WireRequiredWorkerGate(parent)
	if parent.BeforeTurnContext == nil {
		t.Fatal("required worker gate did not install a before-turn hook")
	}

	type turnResult struct {
		allowed bool
		reason  string
		context string
	}
	result := make(chan turnResult, 1)
	go func() {
		allowed, reason, turnContext := parent.BeforeTurnContext(context.Background(), 1)
		result <- turnResult{allowed: allowed, reason: reason, context: turnContext}
	}()
	select {
	case got := <-result:
		t.Fatalf("parent gate returned while required worker was running: %+v", got)
	default:
	}

	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	select {
	case got := <-result:
		if !got.allowed || got.reason != "" || !strings.Contains(got.context, "required") || !strings.Contains(got.context, a.ID) {
			t.Fatalf("gate result = %+v, want required completion context", got)
		}
	case <-ctx.Done():
		t.Fatal("parent gate did not wake when required worker finished")
	}
}

func TestRequiredWorkerUpdateIsAcknowledgedOnlyAfterParentCompletion(t *testing.T) {
	cfg := newRuntimeTestConfig(t.TempDir(), t.TempDir(), func(*subagents.Agent) subagents.Runner {
		return subagents.RunnerFunc(func(context.Context, subagents.Sink) error { return nil })
	})
	rt := newSubagentRuntime(cfg)
	defer func() { _ = rt.Close(context.Background()) }()
	rt.SetActiveSession("parent-session")
	worker, err := rt.Supervisor().SpawnReq(context.Background(), subagents.SpawnRequest{
		Task: "required validation", RootSessionID: "parent-session", Required: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker.Wait()
	parent := &core.Agent{}
	rt.WireRequiredWorkerGate(parent)

	allowed, reason, update := parent.BeforeTurnContext(context.Background(), 1)
	if !allowed || reason != "" || !strings.Contains(update, worker.ID) {
		t.Fatalf("first required context allowed=%v reason=%q update=%q", allowed, reason, update)
	}
	if worker.Snapshot().Requirement.Notified {
		t.Fatal("before-turn context was acknowledged before parent completion")
	}
	allowed, reason, update = parent.BeforeTurnContext(context.Background(), 1)
	if !allowed || reason != "" || !strings.Contains(update, worker.ID) {
		t.Fatalf("replayed required context allowed=%v reason=%q update=%q", allowed, reason, update)
	}
	if allowed, reason, _ := parent.BeforeAssistantMessage("consumed the required result"); !allowed || reason != "" {
		t.Fatalf("parent completion allowed=%v reason=%q", allowed, reason)
	}
	if !worker.Snapshot().Requirement.Notified {
		t.Fatal("successful parent completion did not acknowledge required context")
	}
}

func TestRequiredWorkerGateFailsClosedOnReloadError(t *testing.T) {
	cfg := newRuntimeTestConfig(t.TempDir(), t.TempDir(), func(*subagents.Agent) subagents.Runner {
		return subagents.RunnerFunc(func(context.Context, subagents.Sink) error { return nil })
	})
	rt := newSubagentRuntime(cfg)
	defer func() { _ = rt.Close(context.Background()) }()
	worker, err := rt.Supervisor().SpawnReq(context.Background(), subagents.SpawnRequest{Task: "persisted requirement", Required: true})
	if err != nil {
		t.Fatal(err)
	}
	worker.Wait()
	ready := make(chan struct{})
	close(ready)
	rt.SetRequiredWorkerReady(ready, func() error { return errors.New("malformed persisted worker") })
	parent := &core.Agent{}
	rt.WireRequiredWorkerGate(parent)
	allowed, reason, _ := parent.BeforeTurnContext(context.Background(), 1)
	if allowed || reason != "loading persisted subagents failed: malformed persisted worker; resolve the supervisor reload error before continuing" {
		t.Fatalf("reload gate allowed=%v reason=%q, want fail-closed reload error", allowed, reason)
	}
}

func TestRequiredWorkerGateTreatsManualTerminalRemovalAsWaiver(t *testing.T) {
	cfg := newRuntimeTestConfig(t.TempDir(), t.TempDir(), func(*subagents.Agent) subagents.Runner {
		return subagents.RunnerFunc(func(context.Context, subagents.Sink) error {
			return errors.New("worker failed")
		})
	})
	rt := newSubagentRuntime(cfg)
	defer func() { _ = rt.Close(context.Background()) }()
	rt.SetActiveSession("parent-session")
	worker, err := rt.Supervisor().SpawnReq(context.Background(), subagents.SpawnRequest{
		Task: "required validation", RootSessionID: "parent-session", Required: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker.Wait()
	parent := &core.Agent{}
	rt.WireRequiredWorkerGate(parent)
	if allowed, _, _ := parent.BeforeToolExecute(provider.ToolCallBlock{Name: "bash"}); allowed {
		t.Fatal("unmet requirement did not block tools")
	}
	if err := rt.Supervisor().Remove(worker.ID); err != nil {
		t.Fatalf("explicit user removal waiver: %v", err)
	}
	if allowed, reason, _ := parent.BeforeToolExecute(provider.ToolCallBlock{Name: "bash"}); !allowed || reason != "" {
		t.Fatalf("tool after explicit waiver allowed=%v reason=%q", allowed, reason)
	}
}

func TestRequiredWorkerGateRejectsTerminalParentResponseUntilRetrySucceeds(t *testing.T) {
	attempt := 0
	cfg := newRuntimeTestConfig(t.TempDir(), t.TempDir(), func(*subagents.Agent) subagents.Runner {
		attempt++
		current := attempt
		return subagents.RunnerFunc(func(context.Context, subagents.Sink) error {
			if current == 1 {
				return errors.New("review crashed")
			}
			return nil
		})
	})
	rt := newSubagentRuntime(cfg)
	defer func() { _ = rt.Close(context.Background()) }()
	rt.SetActiveSession("parent-session")
	worker, err := rt.Supervisor().SpawnReq(context.Background(), subagents.SpawnRequest{
		Task:          "required review",
		RootSessionID: "parent-session",
		Required:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker.Wait()

	parent := &core.Agent{}
	rt.WireRequiredWorkerGate(parent)
	toolAllowed, toolReason, _ := parent.BeforeToolExecute(provider.ToolCallBlock{Name: "bash"})
	if toolAllowed || !strings.Contains(toolReason, worker.ID) {
		t.Fatalf("finalization tool allowed=%v reason=%q, want failed required worker gate", toolAllowed, toolReason)
	}
	toolAllowed, toolReason, _ = parent.BeforeToolExecute(provider.ToolCallBlock{Name: "subagent_resume"})
	if !toolAllowed || toolReason != "" {
		t.Fatalf("recovery tool allowed=%v reason=%q, want allowed", toolAllowed, toolReason)
	}
	allowed, reason, _ := parent.BeforeAssistantMessage("I opened the pull request")
	if allowed || !strings.Contains(reason, worker.ID) {
		t.Fatalf("terminal response allowed=%v reason=%q, want failed required worker gate", allowed, reason)
	}

	retried, err := rt.Supervisor().ResumeRequiredWithPrompt(context.Background(), worker.ID, "retry review")
	if err != nil {
		t.Fatal(err)
	}
	retried.Wait()
	allowed, reason, _ = parent.BeforeAssistantMessage("The required review passed")
	if !allowed || reason != "" {
		t.Fatalf("terminal response after retry allowed=%v reason=%q, want allowed", allowed, reason)
	}
}
