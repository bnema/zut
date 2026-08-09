package subagents

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestSupervisor builds a Supervisor rooted in t.TempDir and configured
// with the Runner factory controlled by the test.
func newTestSupervisor(t *testing.T, mk func(a *Agent) Runner) *Supervisor {
	t.Helper()
	root := t.TempDir()
	return New(Config{
		Root:      root,
		RepoRoot:  root,
		NewRunner: mk,
	})
}

func TestUnqualifiedModelID(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		model    string
		want     string
	}{
		{name: "matching provider", provider: "openai-codex", model: "openai-codex/gpt-5.6-sol", want: "gpt-5.6-sol"},
		{name: "matching provider with whitespace", provider: "openai-codex", model: "  openai-codex/gpt-5.6-sol  ", want: "gpt-5.6-sol"},
		{name: "other slash-containing model ID", provider: "openrouter", model: "anthropic/claude-sonnet-4-5", want: "anthropic/claude-sonnet-4-5"},
		{name: "no provider", model: "/custom-model", want: "/custom-model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unqualifiedModelID(tc.provider, tc.model); got != tc.want {
				t.Fatalf("unqualifiedModelID(%q, %q) = %q, want %q", tc.provider, tc.model, got, tc.want)
			}
		})
	}
}

func TestSpawnTimeoutDoesNotLimitResumableWorkerLifetime(t *testing.T) {
	observedDeadline := make(chan bool, 1)
	f := newTestSupervisor(t, func(a *Agent) Runner {
		return RunnerFunc(func(ctx context.Context, _ Sink) error {
			_, hasDeadline := ctx.Deadline()
			observedDeadline <- hasDeadline
			<-ctx.Done()
			return ctx.Err()
		})
	})
	a, err := f.SpawnReq(context.Background(), SpawnRequest{
		Task:    "remain resumable",
		Timeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if hasDeadline := <-observedDeadline; hasDeadline {
		t.Fatal("worker lifetime context has a deadline; turn timeout must not expire an idle resumable worker")
	}
	if a.Timeout != time.Minute {
		t.Fatalf("agent timeout = %s, want %s", a.Timeout, time.Minute)
	}
	f.StopAll()
	a.Wait()
}

func TestSpawnRunsAndCompletes(t *testing.T) {
	ran := make(chan string, 1)
	f := newTestSupervisor(t, func(a *Agent) Runner {
		return RunnerFunc(func(ctx context.Context, sink Sink) error {
			ran <- a.Task
			sink.Activity("hello")
			sink.Transcript("line one")
			sink.Transcript("line two")
			return nil
		})
	})
	a, err := f.Spawn(context.Background(), "do a thing")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-ran:
		if got != "do a thing" {
			t.Fatalf("runner got task %q; want %q", got, "do a thing")
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}
	a.Wait()
	if a.Status() != StatusDone {
		t.Fatalf("status %s; want done", a.Status())
	}
	if got := a.Transcript(); len(got) != 2 || got[0] != "line one" || got[1] != "line two" {
		t.Fatalf("transcript = %q", got)
	}
	if !strings.Contains(a.ID, "do-a-thing") {
		t.Fatalf("id %q missing slug", a.ID)
	}
	// Every agent shares the host's RepoRoot.
	if a.Dir != f.cfg.RepoRoot {
		t.Fatalf("dir = %q; want repo root %q", a.Dir, f.cfg.RepoRoot)
	}
}

// TestSpawnAgentSharesRepoRoot verifies the only-mode-we-support:
// every spawned agent points its cwd at the parent zut's RepoRoot.
func TestSpawnAgentSharesRepoRoot(t *testing.T) {
	f := newTestSupervisor(t, func(a *Agent) Runner {
		return RunnerFunc(func(ctx context.Context, sink Sink) error { return nil })
	})
	a, err := f.Spawn(context.Background(), "share me")
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if a.Dir != f.cfg.RepoRoot {
		t.Fatalf("Dir = %q; want RepoRoot %q", a.Dir, f.cfg.RepoRoot)
	}
}

func TestSetRepoRootAffectsSubsequentSpawns(t *testing.T) {
	f := newTestSupervisor(t, func(a *Agent) Runner {
		return RunnerFunc(func(ctx context.Context, sink Sink) error { return nil })
	})
	want := t.TempDir()
	f.SetRepoRoot(want)
	a, err := f.Spawn(context.Background(), "use new root")
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if a.Dir != want {
		t.Fatalf("Dir = %q; want updated RepoRoot %q", a.Dir, want)
	}
}

func TestSpawnEmptyTaskFails(t *testing.T) {
	f := newTestSupervisor(t, func(a *Agent) Runner {
		return RunnerFunc(func(ctx context.Context, sink Sink) error { return nil })
	})
	if _, err := f.Spawn(context.Background(), "   "); err == nil {
		t.Fatal("expected error on empty task")
	}
}

func TestRunnerErrorMarksFailed(t *testing.T) {
	wantErr := errors.New("boom")
	f := newTestSupervisor(t, func(a *Agent) Runner {
		return RunnerFunc(func(ctx context.Context, sink Sink) error { return wantErr })
	})
	a, _ := f.Spawn(context.Background(), "explode")
	a.Wait()
	if a.Status() != StatusFailed {
		t.Fatalf("status %s; want failed", a.Status())
	}
	if !errors.Is(a.Err(), wantErr) {
		t.Fatalf("err = %v", a.Err())
	}
	if !strings.Contains(a.Activity(), "boom") {
		t.Fatalf("activity = %q; want it to mention the error", a.Activity())
	}
}

func TestStopCancelsRunningAgent(t *testing.T) {
	started := make(chan struct{})
	f := newTestSupervisor(t, func(a *Agent) Runner {
		return RunnerFunc(func(ctx context.Context, sink Sink) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
	})
	a, _ := f.Spawn(context.Background(), "long")
	<-started
	if err := f.Stop(a.ID); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if a.Status() != StatusKilled {
		t.Fatalf("status = %s; want killed", a.Status())
	}
}

func TestStopByRootSessionLeavesOtherSessionsRunning(t *testing.T) {
	started := make(chan string, 2)
	stopped := make(chan string, 2)
	f := newTestSupervisor(t, func(a *Agent) Runner {
		return RunnerFunc(func(ctx context.Context, _ Sink) error {
			started <- a.ID
			<-ctx.Done()
			stopped <- a.ID
			return ctx.Err()
		})
	})
	t.Cleanup(f.StopAll)

	f.SetActiveSession("session-a")
	a, err := f.Spawn(context.Background(), "session A task")
	if err != nil {
		t.Fatal(err)
	}
	f.SetActiveSession("session-b")
	b, err := f.Spawn(context.Background(), "session B task")
	if err != nil {
		t.Fatal(err)
	}

	for range 2 {
		<-started
	}
	if err := f.StopByRootSession("session-a"); err != nil {
		t.Fatal(err)
	}
	if got := <-stopped; got != a.ID {
		t.Fatalf("stopped agent = %q, want session A agent %q", got, a.ID)
	}
	select {
	case got := <-stopped:
		t.Fatalf("session cleanup also stopped %q", got)
	default:
	}
	if got := b.Status(); got != StatusRunning {
		t.Fatalf("session B status = %s, want running", got)
	}
}

func TestSupervisorShutdownCommandsIdentifyOrigin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subagent inbox transport uses Unix-domain sockets")
	}
	for _, tc := range []struct {
		name   string
		origin ShutdownOrigin
		stop   func(*Supervisor, *Agent) error
	}{
		{name: "targeted", origin: ShutdownOriginTargeted, stop: func(f *Supervisor, a *Agent) error { return f.Stop(a.ID) }},
		{name: "session", origin: ShutdownOriginSession, stop: func(f *Supervisor, _ *Agent) error { return f.StopByRootSession("session-a") }},
		{name: "process", origin: ShutdownOriginProcess, stop: func(f *Supervisor, _ *Agent) error { return f.StopAllContext(context.Background()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := make(chan struct{})
			commands := make(chan Envelope, 1)
			f := newTestSupervisor(t, func(a *Agent) Runner {
				return RunnerFunc(func(ctx context.Context, _ Sink) error {
					listener, err := Listen(a.InboxPath)
					if err != nil {
						return err
					}
					defer listener.Close()
					close(started)
					select {
					case line := <-listener.Lines():
						command, err := ParseCommand(line)
						if err != nil {
							return err
						}
						commands <- command
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			})
			f.SetActiveSession("session-a")
			a, err := f.Spawn(context.Background(), "origin test")
			if err != nil {
				t.Fatal(err)
			}
			<-started
			if err := tc.stop(f, a); err != nil {
				t.Fatal(err)
			}
			command := <-commands
			var payload AgentShutdownPayload
			if err := command.DecodePayload(&payload); err != nil {
				t.Fatal(err)
			}
			if payload.Origin != tc.origin {
				t.Fatalf("shutdown origin = %q, want %q", payload.Origin, tc.origin)
			}
			a.Wait()
		})
	}
}

func TestStopContextCallerCancellationDoesNotEndGracePeriod(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subagent inbox transport uses Unix-domain sockets")
	}
	const grace = 250 * time.Millisecond
	started := make(chan struct{})
	canceled := make(chan struct{})
	f := New(Config{
		Root:     t.TempDir(),
		RepoRoot: t.TempDir(),
		Policy: SubagentPolicy{
			CancelGracePeriod: grace,
		},
		NewRunner: func(a *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, sink Sink) error {
				listener, err := Listen(a.InboxPath)
				if err != nil {
					return err
				}
				defer listener.Close()
				close(started)
				select {
				case <-listener.Lines():
				case <-ctx.Done():
					close(canceled)
					return ctx.Err()
				}
				<-ctx.Done()
				close(canceled)
				return ctx.Err()
			})
		},
	})
	a, err := f.Spawn(context.Background(), "graceful stop")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := f.StopContext(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	cancel()

	early := time.NewTimer(grace / 2)
	defer early.Stop()
	select {
	case <-canceled:
		t.Fatal("agent canceled when StopContext caller context was canceled")
	case <-early.C:
	}

	deadline := time.NewTimer(grace * 2)
	defer deadline.Stop()
	select {
	case <-canceled:
	case <-deadline.C:
		t.Fatal("agent was not canceled after the grace period")
	}
	a.Wait()
}

func TestStopContextCancelsDetachedWaitWithoutHoldingOperationLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subagent inbox transport uses Unix-domain sockets")
	}
	path := filepath.Join(shortSocketDir(t), "agent.sock")
	listener, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	f := New(Config{
		Root: t.TempDir(),
		Policy: SubagentPolicy{
			CancelGracePeriod: time.Hour,
		},
	})
	a := &Agent{
		ID:        "detached-agent",
		InboxPath: path,
		inbox:     NewInbox(path),
		status:    StatusDetached,
		done:      make(chan struct{}),
	}
	defer a.inbox.Close()
	close(a.done)
	f.mu.Lock()
	f.agents[a.ID] = a
	f.order = append(f.order, a.ID)
	f.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopDone := make(chan error, 1)
	go func() { stopDone <- f.StopContext(ctx, a.ID) }()

	// The listener notification proves Stop reached its shutdown wait. The
	// operation lock must already be available while that wait is in progress.
	if msg := <-listener.Lines(); msg == "" {
		t.Fatal("shutdown command was empty")
	}
	operationReleased := make(chan struct{})
	go func() {
		f.operationMu.Lock()
		close(operationReleased)
		f.operationMu.Unlock()
	}()
	<-operationReleased

	cancel()
	if err := <-stopDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("StopContext error = %v; want context.Canceled", err)
	}
	if got := a.Status(); got != StatusDetached {
		t.Fatalf("status after canceled stop = %s; want detached", got)
	}
}

func TestStopDetachedWorkerClosesWaiters(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subagent inbox transport uses Unix-domain sockets")
	}
	path := filepath.Join(shortSocketDir(t), "agent.sock")
	listener, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	root := t.TempDir()
	f := New(Config{Root: root, RepoRoot: root})
	stateDir := f.agentStateDir("detached-agent")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	a := &Agent{
		ID:        "detached-agent",
		stateDir:  stateDir,
		InboxPath: path,
		inbox:     NewInbox(path),
		status:    StatusDetached,
		done:      make(chan struct{}),
	}
	defer a.inbox.Close()
	f.mu.Lock()
	f.agents[a.ID] = a
	f.order = append(f.order, a.ID)
	f.mu.Unlock()

	stopped := make(chan error, 1)
	go func() { stopped <- f.StopContext(context.Background(), a.ID) }()
	select {
	case msg := <-listener.Lines():
		if msg == "" {
			t.Fatal("shutdown command was empty")
		}
	case <-time.After(time.Second):
		t.Fatal("detached worker did not receive shutdown")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("stop detached worker: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("detached worker stop did not finish")
	}
	select {
	case <-a.done:
	case <-time.After(time.Second):
		t.Fatal("detached worker stop did not close waiters")
	}
	result, err := readTurnResult(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if result.AgentID != a.ID || result.Status != ResultCanceled {
		t.Fatalf("detached worker result = agent %q status %q, want agent %q status %q", result.AgentID, result.Status, a.ID, ResultCanceled)
	}
}

func TestStopDetachedWorkerHonorsCanceledShutdownSend(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subagent inbox transport uses Unix-domain sockets")
	}
	path := filepath.Join(shortSocketDir(t), "agent.sock")
	listener, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	local, remote := net.Pipe()
	defer remote.Close()
	conn := &blockingWriteConn{
		Conn:    local,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	inbox := NewInbox(path)
	inbox.conn = conn
	defer inbox.Close()

	f := New(Config{Root: t.TempDir()})
	a := &Agent{
		ID:        "detached-cancel",
		InboxPath: path,
		inbox:     inbox,
		status:    StatusDetached,
		done:      make(chan struct{}),
	}
	f.mu.Lock()
	f.agents[a.ID] = a
	f.order = append(f.order, a.ID)
	f.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- f.StopContext(ctx, a.ID) }()
	select {
	case <-conn.started:
	case <-time.After(time.Second):
		t.Fatal("detached worker shutdown send did not start")
	}
	cancel()
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stop detached worker error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("detached worker shutdown send did not honor cancellation")
	}
}

func TestStopAfterDoneIsNoop(t *testing.T) {
	f := newTestSupervisor(t, func(a *Agent) Runner {
		return RunnerFunc(func(ctx context.Context, sink Sink) error { return nil })
	})
	a, _ := f.Spawn(context.Background(), "quick")
	a.Wait()
	if err := f.Stop(a.ID); err != nil {
		t.Fatalf("stop after done: %v", err)
	}
	if a.Status() != StatusDone {
		t.Fatalf("status flipped to %s", a.Status())
	}
}

func TestGetPrefixMatch(t *testing.T) {
	f := newTestSupervisor(t, func(a *Agent) Runner {
		return RunnerFunc(func(ctx context.Context, sink Sink) error { return nil })
	})
	a, _ := f.Spawn(context.Background(), "alpha task")
	a.Wait()
	// Full id works.
	if got := f.Get(a.ID); got != a {
		t.Fatal("get by full id failed")
	}
	// Slug prefix works as long as it's unique.
	if got := f.Get("alpha"); got != a {
		t.Fatal("get by prefix failed")
	}
	// Bogus id returns nil.
	if got := f.Get("zzz-nope"); got != nil {
		t.Fatalf("expected nil; got %#v", got)
	}
}

func TestRemoveRequiresTerminalState(t *testing.T) {
	hold := make(chan struct{})
	f := newTestSupervisor(t, func(a *Agent) Runner {
		return RunnerFunc(func(ctx context.Context, sink Sink) error {
			<-hold
			return nil
		})
	})
	a, _ := f.Spawn(context.Background(), "still going")
	// Wait for run goroutine to flip to running.
	for i := 0; i < 100 && a.Status() != StatusRunning; i++ {
		time.Sleep(time.Millisecond)
	}
	if err := f.Remove(a.ID); err == nil {
		t.Fatal("remove of running agent should fail")
	}
	close(hold)
	a.Wait()
	if err := f.Remove(a.ID); err != nil {
		t.Fatalf("remove after done: %v", err)
	}
	if got := f.Get(a.ID); got != nil {
		t.Fatal("agent still present after remove")
	}
}

func TestSnapshotIsStableAcrossAccess(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	f := newTestSupervisor(t, func(a *Agent) Runner {
		return RunnerFunc(func(ctx context.Context, sink Sink) error {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				sink.Transcript("chunk")
				sink.Activity("step")
			}
			return nil
		})
	})
	a, _ := f.Spawn(context.Background(), "race")
	// Hammer Snapshot while the runner is writing; the -race detector
	// is the real assertion here.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				_ = a.Snapshot()
			}
		}
	}()
	wg.Wait()
	a.Wait()
	close(stop)
	if a.Status() != StatusDone {
		t.Fatalf("status = %s", a.Status())
	}
}

func TestTruncateIsRuneAware(t *testing.T) {
	input := "αβγδεζη"
	if got := truncate(input, 6); got != "αβγ..." {
		t.Fatalf("truncate(%q, 6) = %q; want %q", input, got, "αβγ...")
	}
	if got := truncate(input, 3); got != "..." {
		t.Fatalf("truncate(%q, 3) = %q; want %q", input, got, "...")
	}
	if got := truncate(input, 2); got != ".." {
		t.Fatalf("truncate(%q, 2) = %q; want %q", input, got, "..")
	}
}

func TestTaskSlug(t *testing.T) {
	cases := map[string]string{
		"fix the login form":                   "fix-the-login-form",
		"  weird --- spaces!!  ":               "weird-spaces",
		"":                                     "agent",
		"a-very-long-task-name-that-overflows": "a-very-long-task-name-th",
	}
	for in, want := range cases {
		if got := taskSlug(in); got != want {
			t.Errorf("taskSlug(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestSnapshotAllSorted(t *testing.T) {
	f := newTestSupervisor(t, func(a *Agent) Runner {
		return RunnerFunc(func(ctx context.Context, sink Sink) error { return nil })
	})
	a1, _ := f.Spawn(context.Background(), "first")
	// Force second spawn into a later nanosecond bucket.
	time.Sleep(2 * time.Millisecond)
	a2, _ := f.Spawn(context.Background(), "second")
	a1.Wait()
	a2.Wait()
	snaps := f.SnapshotAll()
	if len(snaps) != 2 {
		t.Fatalf("want 2 snapshots; got %d", len(snaps))
	}
	if !snaps[0].Started.Before(snaps[1].Started) && !snaps[0].Started.Equal(snaps[1].Started) {
		t.Fatal("snapshots not in spawn order")
	}
}
