package subagents

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRequiredWaitWakesOnlyAfterOutcomePersistence(t *testing.T) {
	persisting := make(chan struct{})
	release := make(chan struct{})
	a := &Agent{
		ID:                 "required-agent",
		requirement:        newRequirement(true, 1, time.Now()),
		requirementChanged: make(chan struct{}),
		persistFn: func(*Agent) error {
			close(persisting)
			<-release
			return nil
		},
	}
	resolved := make(chan struct{})
	go func() {
		a.resolveRequirement(1, nil, "", false)
		close(resolved)
	}()
	<-persisting
	a.lifecycleMu.Lock()
	visible := a.visibleRequirementLocked()
	a.lifecycleMu.Unlock()
	if visible.State != RequirementPending {
		t.Fatalf("visible requirement while persistence is blocked = %+v, want pending", visible)
	}
	waited := make(chan RequirementSnapshot, 1)
	go func() {
		got, _ := a.waitRequirement(context.Background())
		waited <- got
	}()
	close(release)
	<-resolved
	if got := <-waited; got.State != RequirementSatisfied {
		t.Fatalf("durable outcome = %+v", got)
	}
}

func TestRequiredOutcomeUsesDelegatedTurnOrdinal(t *testing.T) {
	a := &Agent{
		requirement:        newRequirement(true, 1, time.Now()),
		requirementChanged: make(chan struct{}),
	}
	if err := updateAgentFromEvent(a, NewEvent(EventTurnStarted, map[string]any{
		"step": float64(1), "lifetime_turns": float64(1), "current_run_turns": float64(1),
	})); err != nil {
		t.Fatal(err)
	}
	if err := updateAgentFromEvent(a, NewEvent("turn_end", map[string]any{"step": float64(1)})); err != nil {
		t.Fatal(err)
	}
	if got := a.requirementSnapshot(); got.State != RequirementSatisfied || got.TargetTurn != 1 {
		t.Fatalf("canonical first-turn outcome = %+v", got)
	}

	a.prepareRequired(0)
	if err := updateAgentFromEvent(a, NewEvent(EventTurnStarted, map[string]any{
		"step": float64(2), "lifetime_turns": float64(2), "current_run_turns": float64(1),
	})); err != nil {
		t.Fatal(err)
	}
	if err := updateAgentFromEvent(a, NewEvent("turn_end", map[string]any{"step": float64(2)})); err != nil {
		t.Fatal(err)
	}
	if got := a.requirementSnapshot(); got.State != RequirementSatisfied || got.TargetTurn != 2 {
		t.Fatalf("canonical retry outcome = %+v", got)
	}

	for _, step := range []int{0, 1} {
		legacy := &Agent{
			requirement:        newRequirement(true, 1, time.Now()),
			requirementChanged: make(chan struct{}),
		}
		if err := updateAgentFromEvent(legacy, NewEvent("turn_end", map[string]any{"step": float64(step)})); err != nil {
			t.Fatal(err)
		}
		if got := legacy.requirementSnapshot(); got.State != RequirementSatisfied {
			t.Fatalf("lifetime-free step %d outcome = %+v", step, got)
		}
	}
}

func TestRequiredWaitCompletesOnDelegatedTurnWhileWorkerStaysAlive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets not supported")
	}
	if testing.Short() {
		t.Skip("skip end-to-end runner test in -short mode")
	}
	exe := buildStubChild(t)
	t.Setenv("ZUT_STUB_PROTOCOL", "1")
	root := t.TempDir()
	f := New(Config{
		Root:     filepath.Join(root, "subagents"),
		RepoRoot: root,
		NewRunner: func(a *Agent) Runner {
			return &execRunner{
				agent: a,
				Command: subagentWorkerArgs(subagentWorkerArgsOpts{
					Exe: exe, Dir: a.Dir, SessionPath: a.SessionPath, InboxPath: a.InboxPath, Task: a.Task,
				}),
				GracePeriod: time.Second,
			}
		},
	})
	t.Cleanup(f.StopAll)
	a, err := f.SpawnReq(context.Background(), SpawnRequest{Task: "required review", Required: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), runnerProcessTimeout)
	defer cancel()
	got, err := a.waitRequirement(ctx)
	if err != nil {
		events, _ := ReadEventLog(a.EventLogPath)
		t.Fatalf("wait for delegated turn: %v\nsnapshot: %+v\nevents: %#v\nformatted: %s", err, a.Snapshot(), events, formatEvents(events))
	}
	if got.State != RequirementSatisfied {
		t.Fatalf("requirement = %+v, want satisfied", got)
	}
	if snapshot := a.Snapshot(); snapshot.Status != StatusRunning {
		t.Fatalf("worker lifecycle = (%s, %s), want live daemon", snapshot.Status, snapshot.TurnState)
	}
}

func TestReloadFailsClosedWhenRequiredOutcomeWasNotObserved(t *testing.T) {
	root := t.TempDir()
	started := make(chan struct{})
	first := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(*Agent) Runner {
			return RunnerFunc(func(ctx context.Context, _ Sink) error {
				close(started)
				<-ctx.Done()
				return ctx.Err()
			})
		},
	})
	a, err := first.SpawnReq(context.Background(), SpawnRequest{Task: "required review", Required: true})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	t.Cleanup(first.StopAll)

	second := New(Config{Root: root, RepoRoot: root})
	loaded, errs := second.Reload()
	if len(errs) != 0 || loaded != 1 {
		t.Fatalf("reload loaded=%d errors=%v", loaded, errs)
	}
	got := second.Get(a.ID).Snapshot().Requirement
	if got.State != RequirementIndeterminate || !got.Unmet() || got.ErrorCode != "required_outcome_unobserved" {
		t.Fatalf("reconciled requirement = %+v, want durable indeterminate outcome", got)
	}
	meta, err := readAgentMeta(filepath.Join(root, "agents", a.ID))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Requirement.State != RequirementIndeterminate {
		t.Fatalf("persisted reconciled requirement = %+v", meta.Requirement)
	}
}

func TestRequiredOutcomePersistsAcrossReload(t *testing.T) {
	root := t.TempDir()
	first := New(Config{
		Root:     root,
		RepoRoot: root,
		NewRunner: func(*Agent) Runner {
			return RunnerFunc(func(context.Context, Sink) error { return errors.New("review failed") })
		},
	})
	t.Cleanup(first.StopAll)

	a, err := first.SpawnReq(context.Background(), SpawnRequest{Task: "required review", Required: true})
	if err != nil {
		t.Fatalf("spawn required worker: %v", err)
	}
	a.Wait()
	got := a.Snapshot().Requirement
	if !got.Required || got.State != RequirementFailed {
		t.Fatalf("live requirement = %+v, want required failed", got)
	}

	second := New(Config{Root: root, RepoRoot: root})
	loaded, errs := second.Reload()
	if len(errs) != 0 {
		t.Fatalf("reload errors: %v", errs)
	}
	if loaded != 1 {
		t.Fatalf("loaded = %d, want 1", loaded)
	}
	reloaded := second.Get(a.ID)
	if reloaded == nil {
		t.Fatalf("reloaded worker %q not found", a.ID)
	}
	got = reloaded.Snapshot().Requirement
	if !got.Required || got.State != RequirementFailed || got.TargetTurn != 1 {
		t.Fatalf("reloaded requirement = %+v, want durable failed turn 1", got)
	}

	meta, err := readAgentMeta(filepath.Join(root, "agents", a.ID))
	if err != nil {
		t.Fatalf("read agent metadata: %v", err)
	}
	if !meta.Requirement.Required || meta.Requirement.State != RequirementFailed {
		t.Fatalf("persisted requirement = %+v", meta.Requirement)
	}
}

func TestRequirementNotificationRecordsDeliveryExactlyOnce(t *testing.T) {
	trace := NewMemoryTraceWriter()
	t.Cleanup(func() { _ = trace.Close() })
	a := &Agent{ID: "agent-1", trace: trace, result: &TurnResult{AgentID: "agent-1", TurnID: "turn-1", Status: ResultSucceeded}, resultRef: ResultRef("agent-1"), requirement: RequirementSnapshot{Required: true, State: RequirementSatisfied, TargetTurn: 1, ResultTurnID: "turn-1"}}
	s := &Supervisor{agents: map[string]*Agent{a.ID: a}}
	if err := s.MarkRequirementNotified(a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRequirementNotified(a.ID); err != nil {
		t.Fatal(err)
	}
	if err := trace.Flush(); err != nil {
		t.Fatal(err)
	}
	var delivered int
	for _, event := range trace.Events() {
		if event.Type == "result.delivered" {
			delivered++
		}
	}
	if delivered != 1 {
		t.Fatalf("result.delivered count = %d, want 1", delivered)
	}
}

func TestRequirementNotificationPersistenceFailureIsReported(t *testing.T) {
	trace := NewMemoryTraceWriter()
	t.Cleanup(func() { _ = trace.Close() })
	a := &Agent{
		ID:        "agent-1",
		trace:     trace,
		result:    &TurnResult{AgentID: "agent-1", TurnID: "turn-1", Status: ResultSucceeded},
		resultRef: ResultRef("agent-1"),
		requirement: RequirementSnapshot{
			Required: true, State: RequirementSatisfied, TargetTurn: 1, ResultTurnID: "turn-1",
		},
		persistFn: func(*Agent) error { return errors.New("disk full") },
	}
	s := &Supervisor{agents: map[string]*Agent{a.ID: a}}
	if err := s.MarkRequirementNotified(a.ID); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("MarkRequirementNotified error = %v, want persistence failure", err)
	}
	if a.Snapshot().Requirement.Notified {
		t.Fatal("requirement remained notified after persistence failure")
	}
	if err := trace.Flush(); err != nil {
		t.Fatal(err)
	}
	view := trace.Views()[a.ID]
	if view.Result == nil || !view.Result.Failed {
		t.Fatalf("delivery failure trace = %#v", view.Result)
	}
}

func TestRequirementNotificationRejectsMissingDurableResult(t *testing.T) {
	trace := NewMemoryTraceWriter()
	t.Cleanup(func() { _ = trace.Close() })
	a := &Agent{ID: "agent-1", trace: trace, requirement: RequirementSnapshot{Required: true, State: RequirementSatisfied, TargetTurn: 1}}
	s := &Supervisor{agents: map[string]*Agent{a.ID: a}}
	if err := s.MarkRequirementNotified(a.ID); err == nil {
		t.Fatal("MarkRequirementNotified succeeded without durable result")
	}
	if a.Snapshot().Requirement.Notified {
		t.Fatal("requirement marked notified without durable result")
	}
	for _, event := range trace.Events() {
		if event.Type == "result.delivered" {
			t.Fatalf("unexpected delivery trace: %#v", event)
		}
	}
}

func TestRequirementNotificationPersistsAndRetryClearsIt(t *testing.T) {
	root := t.TempDir()
	first := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(*Agent) Runner {
			return RunnerFunc(func(context.Context, Sink) error { return nil })
		},
	})
	t.Cleanup(first.StopAll)
	a, err := first.SpawnReq(context.Background(), SpawnRequest{Task: "required validation", Required: true})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if err := first.MarkRequirementNotified(a.ID); err != nil {
		t.Fatal(err)
	}

	second := New(Config{Root: root, RepoRoot: root})
	if _, errs := second.Reload(); len(errs) != 0 {
		t.Fatalf("reload errors: %v", errs)
	}
	reloaded := second.Get(a.ID)
	if got := reloaded.requirementSnapshot(); !got.Notified || got.State != RequirementSatisfied {
		t.Fatalf("reloaded notified requirement = %+v", got)
	}
	previous := reloaded.prepareRequired(0)
	if previous.TargetTurn != 1 {
		t.Fatalf("previous target = %d, want 1", previous.TargetTurn)
	}
	pending := reloaded.requirementSnapshot()
	if pending.Notified || pending.TargetTurn != 2 {
		t.Fatalf("retried requirement = %+v, want unnotified turn 2", pending)
	}
	reloaded.prepareRequired(0)
	if got := reloaded.requirementSnapshot(); got.TargetTurn != 2 {
		t.Fatalf("idempotent prepare target = %d, want 2", got.TargetTurn)
	}
	reloaded.resolveRequirement(2, nil, "", false)
	if got := reloaded.requirementSnapshot(); got.Notified || got.State != RequirementSatisfied {
		t.Fatalf("resolved retry = %+v, want unnotified satisfaction", got)
	}
}

func TestRequiredTerminalOutcomesRemainUnmet(t *testing.T) {
	tests := []struct {
		name string
		run  func(context.Context) error
		want RequirementState
	}{
		{name: "failed", run: func(context.Context) error { return errors.New("boom") }, want: RequirementFailed},
		{name: "timed out", run: func(context.Context) error { return context.DeadlineExceeded }, want: RequirementTimedOut},
		{name: "canceled", run: func(context.Context) error { return context.Canceled }, want: RequirementCanceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTestSupervisor(t, func(*Agent) Runner {
				return RunnerFunc(func(ctx context.Context, _ Sink) error { return tt.run(ctx) })
			})
			a, err := f.SpawnReq(context.Background(), SpawnRequest{Task: tt.name, Required: true})
			if err != nil {
				t.Fatal(err)
			}
			a.Wait()
			got := a.Snapshot().Requirement
			if got.State != tt.want || !got.Unmet() {
				t.Fatalf("requirement = %+v, want unmet %s", got, tt.want)
			}
		})
	}
}

func TestWaitRequiredIsEventDrivenAndRetryCanSatisfy(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	attempt := 0
	f := newTestSupervisor(t, func(*Agent) Runner {
		attempt++
		current := attempt
		return RunnerFunc(func(ctx context.Context, _ Sink) error {
			if current == 1 {
				close(started)
				select {
				case <-release:
					return errors.New("first attempt failed")
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		})
	})
	a, err := f.SpawnReq(context.Background(), SpawnRequest{Task: "required finalizer", Required: true})
	if err != nil {
		t.Fatal(err)
	}
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	waited := make(chan RequirementSnapshot, 1)
	waitErr := make(chan error, 1)
	go func() {
		result, err := a.waitRequirement(ctx)
		if err != nil {
			waitErr <- err
			return
		}
		waited <- result
	}()
	select {
	case got := <-waited:
		t.Fatalf("wait returned before the worker finished: %+v", got)
	case err := <-waitErr:
		t.Fatalf("wait failed before the worker finished: %v", err)
	default:
	}

	close(release)
	select {
	case err := <-waitErr:
		t.Fatalf("wait failed: %v", err)
	case got := <-waited:
		if got.State != RequirementFailed || !got.Unmet() {
			t.Fatalf("first outcome = %+v, want unmet failure", got)
		}
	case <-ctx.Done():
		t.Fatal("event-driven requirement wait did not wake")
	}
	a.Wait()

	resumed, err := f.ResumeRequiredWithPrompt(context.Background(), a.ID, "retry the finalization")
	if err != nil {
		t.Fatalf("required retry: %v", err)
	}
	resumed.Wait()
	got, err := resumed.waitRequirement(context.Background())
	if err != nil {
		t.Fatalf("wait for retry: %v", err)
	}
	if got.State != RequirementSatisfied || got.Unmet() || got.TargetTurn != 2 {
		t.Fatalf("retry outcome = %+v, want satisfied turn 2", got)
	}
}
