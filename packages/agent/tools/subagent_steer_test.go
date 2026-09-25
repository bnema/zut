package tools

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
)

// steerRuntime blocks its turn until the first steer arrives, then finishes.
type steerRuntime struct {
	started chan struct{}
	steered chan string

	mu      sync.Mutex
	running bool
}

func (r *steerRuntime) Run(ctx context.Context, _ string) error {
	r.mu.Lock()
	r.running = true
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.running = false; r.mu.Unlock() }()
	r.started <- struct{}{}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.steered:
		return nil
	}
}

func (r *steerRuntime) Steer(prompt string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return false
	}
	r.steered <- prompt
	return true
}

func (r *steerRuntime) PendingSteers() int    { return 0 }
func (r *steerRuntime) DrainSteers() []string { return nil }

// Resume steers a running child by default, and its wait resolves on the
// running turn that received the follow-up.
func TestResidentResumeSteersRunningChildAndWaitsOnItsTurn(t *testing.T) {
	runtime := &steerRuntime{started: make(chan struct{}, 1), steered: make(chan string, 1)}
	manager := subagents.NewResidentManager(t.TempDir(), func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentRuntime, error) {
		return runtime, nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	if _, err := manager.Spawn(t.Context(), subagents.ResidentChildSpec{ID: "steer-tool", InitialTurnID: "initial", SessionID: "session", Provider: "openai", Model: "test"}, "investigate"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runtime.started:
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not start")
	}
	facade := &SubagentTool{Resume: &SubagentResumeTool{ResidentManager: manager, Enabled: func() bool { return true }}}
	result, err := facade.Execute(t.Context(), json.RawMessage(`{"action":"resume","agent_id":"steer-tool","prompt":"report now","wait":5}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("resume = (%#v, %v)", result, err)
	}
	response, ok := result.Details.(subagentResumeResponse)
	if !ok || response.Delivery != "steered" || response.Wait == nil || response.Wait.TimedOut || response.Wait.Status != string(subagents.ResidentCompleted) {
		t.Fatalf("response = %#v", result.Details)
	}
}

func TestResidentResumeRejectsUnknownMode(t *testing.T) {
	manager := subagents.NewResidentManager(t.TempDir(), func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentRuntime, error) {
		return subagents.ResidentTurnRunner(func(context.Context, string) error { return nil }), nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	resume := &SubagentResumeTool{ResidentManager: manager, Enabled: func() bool { return true }}
	result, err := resume.Execute(t.Context(), json.RawMessage(`{"agent_id":"x","prompt":"p","mode":"now"}`), nil)
	if err != nil || !result.IsError {
		t.Fatalf("result = (%#v, %v), want mode error", result, err)
	}
}

func TestResidentInterruptToolRequiresRunningTurn(t *testing.T) {
	manager := subagents.NewResidentManager(t.TempDir(), func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentRuntime, error) {
		return subagents.ResidentTurnRunner(func(context.Context, string) error { return nil }), nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	done, cancel := manager.WatchCompletion("idle-child", "initial")
	defer cancel()
	if _, err := manager.Spawn(t.Context(), subagents.ResidentChildSpec{ID: "idle-child", InitialTurnID: "initial", SessionID: "session", Provider: "openai", Model: "test"}, "task"); err != nil {
		t.Fatal(err)
	}
	<-done
	interrupt := &SubagentInterruptTool{ResidentManager: manager, Enabled: func() bool { return true }}
	result, err := interrupt.Execute(t.Context(), json.RawMessage(`{"agent_id":"idle-child"}`), nil)
	if err != nil || !result.IsError {
		t.Fatalf("result = (%#v, %v), want no-running-turn error", result, err)
	}
}

// The facade dispatches interrupt to a running child and keeps it live.
func TestSubagentFacadeInterruptsRunningTurn(t *testing.T) {
	started := make(chan struct{}, 1)
	manager := subagents.NewResidentManager(t.TempDir(), func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentRuntime, error) {
		return subagents.ResidentTurnRunner(func(ctx context.Context, _ string) error {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}), nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	done, cancel := manager.WatchCompletion("running-child", "initial")
	defer cancel()
	if _, err := manager.Spawn(t.Context(), subagents.ResidentChildSpec{ID: "running-child", InitialTurnID: "initial", SessionID: "session", Provider: "openai", Model: "test"}, "task"); err != nil {
		t.Fatal(err)
	}
	<-started
	facade := &SubagentTool{Interrupt: &SubagentInterruptTool{ResidentManager: manager, Enabled: func() bool { return true }}}
	result, err := facade.Execute(t.Context(), json.RawMessage(`{"action":"interrupt","agent_id":"running-child"}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("interrupt = (%#v, %v)", result, err)
	}
	if response, ok := result.Details.(subagentActionResponse); !ok || response.Action != "interrupt_requested" {
		t.Fatalf("response = %#v", result.Details)
	}
	select {
	case completion := <-done:
		if completion.Completion().Status != string(subagents.ResidentInterrupted) {
			t.Fatalf("completion = %#v", completion)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupted turn did not complete")
	}
	if manager.Get("running-child") == nil {
		t.Fatal("interrupt removed the live child")
	}
	result, err = facade.Execute(t.Context(), json.RawMessage(`{"action":"interrupt","agent_id":"running-child","mode":"steer"}`), nil)
	if err != nil || !result.IsError {
		t.Fatalf("mode on interrupt = (%#v, %v), want foreign-field error", result, err)
	}
}
