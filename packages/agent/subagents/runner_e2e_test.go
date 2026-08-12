package subagents

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type notifyingAgentSink struct {
	agentSink
	deltas chan<- struct{}
}

func (s notifyingAgentSink) assistantDelta(text string) {
	s.agentSink.assistantDelta(text)
	select {
	case s.deltas <- struct{}{}:
	default:
	}
}

const runnerProcessTimeout = 10 * time.Second

type controlledDeadlineContext struct{ done chan struct{} }

func (c *controlledDeadlineContext) Deadline() (time.Time, bool) {
	return time.Now().Add(time.Hour), true
}
func (c *controlledDeadlineContext) Done() <-chan struct{} { return c.done }
func (c *controlledDeadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}
func (c *controlledDeadlineContext) Value(any) any { return nil }
func (c *controlledDeadlineContext) expire() {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
}

// TestExecRunnerDeadlineGracefullyStopsWorkerAndPreservesStreamedOutput
// verifies that deadline expiry shuts a worker down through its inbox rather
// than killing it before its terminal events can be recorded.
func TestExecRunnerDeadlineGracefullyStopsWorkerAndPreservesStreamedOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets not supported")
	}
	if testing.Short() {
		t.Skip("skip end-to-end runner test in -short mode")
	}

	exe := buildStubChild(t)
	t.Setenv("ZUT_STUB_PROTOCOL", "1")
	t.Setenv("ZUT_STUB_BLOCK_INITIAL", "1")

	root := t.TempDir()
	inboxPath, err := inboxSocketPath(root, "deadline-test")
	if err != nil {
		t.Fatalf("inboxSocketPath: %v", err)
	}
	a := &Agent{
		ID:           "deadline-test",
		Task:         "long-running task",
		Dir:          root,
		SessionPath:  filepath.Join(root, "session.jsonl"),
		InboxPath:    inboxPath,
		EventLogPath: filepath.Join(root, "events.jsonl"),
	}
	a.inbox = NewInbox(a.InboxPath)
	t.Cleanup(func() { _ = a.inbox.Close() })

	r := &execRunner{
		agent:       a,
		Command:     subagentWorkerArgs(subagentWorkerArgsOpts{Exe: exe, Dir: a.Dir, SessionPath: a.SessionPath, InboxPath: a.InboxPath, Task: a.Task}),
		GracePeriod: time.Second,
	}
	ctx := &controlledDeadlineContext{done: make(chan struct{})}
	defer ctx.expire()
	deltas := make(chan struct{}, 1)
	runDone := make(chan error, 1)
	go func() { runDone <- r.Run(ctx, notifyingAgentSink{agentSink: agentSink{a: a}, deltas: deltas}) }()
	select {
	case <-deltas:
	case <-time.After(runnerProcessTimeout):
		t.Fatal("worker did not emit its initial delta")
	}
	ctx.expire()
	select {
	case err := <-runDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Run error = %v, want context deadline exceeded", err)
		}
	case <-time.After(runnerProcessTimeout):
		t.Fatal("Run did not return after deadline expiry")
	}

	snapshot := a.Snapshot()
	if snapshot.LastAssistant != "partial answer" {
		t.Fatalf("live partial output = %q, want %q", snapshot.LastAssistant, "partial answer")
	}
}

func TestExecRunnerCancelsBlockedWorker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets not supported")
	}
	if testing.Short() {
		t.Skip("skip end-to-end runner test in -short mode")
	}

	exe := buildStubChild(t)
	t.Setenv("ZUT_STUB_PROTOCOL", "1")
	t.Setenv("ZUT_STUB_BLOCK_INITIAL", "1")

	root := t.TempDir()
	inboxPath, err := inboxSocketPath(root, "cancel-test")
	if err != nil {
		t.Fatalf("inboxSocketPath: %v", err)
	}
	a := &Agent{
		ID:           "cancel-test",
		Task:         "blocked task",
		Dir:          root,
		SessionPath:  filepath.Join(root, "session.jsonl"),
		InboxPath:    inboxPath,
		EventLogPath: filepath.Join(root, "events.jsonl"),
	}
	a.inbox = NewInbox(a.InboxPath)
	t.Cleanup(func() { _ = a.inbox.Close() })

	r := &execRunner{
		agent:   a,
		Command: subagentWorkerArgs(subagentWorkerArgsOpts{Exe: exe, Dir: a.Dir, SessionPath: a.SessionPath, InboxPath: a.InboxPath, Task: a.Task}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deltas := make(chan struct{}, 1)
	runDone := make(chan error, 1)
	go func() { runDone <- r.Run(ctx, notifyingAgentSink{agentSink: agentSink{a: a}, deltas: deltas}) }()

	select {
	case <-deltas:
	case <-time.After(runnerProcessTimeout):
		t.Fatal("worker did not emit its initial delta")
	}
	cancel()
	select {
	case err := <-runDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context canceled", err)
		}
	case <-time.After(runnerProcessTimeout):
		t.Fatal("Run did not return after context cancellation")
	}
}

// TestRunnerEndToEndWithStubChild drives the daemon-mode runner through an
// initial turn, a follow-up, and a graceful stop using the stub child. It is
// skipped on platforms without Unix sockets.
func TestRunnerEndToEndWithStubChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets not supported")
	}
	if testing.Short() {
		t.Skip("skip end-to-end runner test in -short mode")
	}

	exe := buildStubChild(t)
	t.Setenv("ZUT_STUB_PROTOCOL", "1")

	root := t.TempDir()
	repo := t.TempDir()
	f := New(Config{
		Root:     root,
		RepoRoot: repo,
		NewRunner: func(a *Agent) Runner {
			return &execRunner{
				agent: a,
				Command: subagentWorkerArgs(subagentWorkerArgsOpts{
					Exe:         exe,
					Dir:         a.Dir,
					SessionPath: a.SessionPath,
					InboxPath:   a.InboxPath,
					Task:        a.Task,
					Model:       a.Model,
					Provider:    a.Provider,
				}),
			}
		},
	})
	defer f.StopAll()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a, err := f.Spawn(ctx, "first task")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Wait for the supervisor-owned in-memory transcript. Worker protocol
	// stdout is no longer duplicated into a per-agent durable event log.
	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(runnerProcessTimeout)
		for time.Now().Before(deadline) {
			if strings.Contains(strings.Join(a.Transcript(), "\n"), want) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %q in transcript: %q", want, a.Transcript())
	}
	waitFor("echo: first task")

	// Send a follow-up over the inbox. The stub echoes the text into
	// an assistant_message we can poll for in the log.
	if err := retrySend(f, a.ID, "follow up", time.Second); err != nil {
		t.Fatalf("SendUserTurn: %v", err)
	}
	waitFor("echo: follow up")

	// Shut the agent down gracefully via the inbox.
	if err := a.inbox.SendCommand(NewCommand(CommandAgentShutdown, a.ID, a.CurrentTurnID(), AgentShutdownPayload{})); err != nil && !errors.Is(err, ErrNotReady) {
		t.Fatalf("shutdown send: %v", err)
	}
	a.Wait()
	if got := a.Status(); got != StatusDone && got != StatusKilled {
		t.Fatalf("final status = %s; want done/killed", got)
	}
}

// retrySend exists because the inbox dial races against the child
// opening the socket. Production callers handle ErrNotReady with a
// status message; tests retry within a small window.
func retrySend(f *Supervisor, id, msg string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		err := f.SendUserTurn(id, msg)
		if err == nil {
			return nil
		}
		lastErr = err
		if !errors.Is(err, ErrNotReady) {
			return err
		}
		time.Sleep(30 * time.Millisecond)
	}
	return lastErr
}

func formatEvents(evs []Event) string {
	var sb strings.Builder
	for _, ev := range evs {
		sb.WriteString(ev.Type)
		sb.WriteString(" ")
		sb.WriteString(ev.Time.Format(time.RFC3339Nano))
		sb.WriteString("\n")
	}
	return sb.String()
}

func buildStubChild(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "stubchild")
	cmd := exec.Command("go", "build", "-o", out, "./testdata/cmd/stubchild")
	// Pass through the test runner's env so `go build` can find
	// HOME, PATH, GOCACHE, etc. CGO is disabled to keep the build
	// hermetic across machines.
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build stubchild: %v\n%s", err, b)
	}
	return out
}
