package subagents

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestPathWithinResolvesMissingDescendantThroughSymlink(t *testing.T) {
	realRoot := t.TempDir()
	aliasParent := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realRoot, aliasParent); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	stateDir := filepath.Join(aliasParent, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	missingFile := filepath.Join(stateDir, "events.jsonl")
	if !pathWithin(missingFile, stateDir) {
		t.Fatalf("pathWithin(%q, %q) = false; want true", missingFile, stateDir)
	}
}

func TestSchedulerBoundsConcurrencyAndReleasesCapacity(t *testing.T) {
	root := t.TempDir()
	started := make(chan string, 3)
	var active, maxActive int32
	f := New(Config{
		Root: root, RepoRoot: root,
		Policy: SubagentPolicy{MaxConcurrent: 1, MaxConcurrentPerParent: 4, QueueTimeout: time.Minute, DefaultTimeout: 0, IdleTimeout: time.Hour},
		NewRunner: func(a *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, _ Sink) error {
				current := atomic.AddInt32(&active, 1)
				for {
					old := atomic.LoadInt32(&maxActive)
					if current <= old || atomic.CompareAndSwapInt32(&maxActive, old, current) {
						break
					}
				}
				started <- a.ID
				<-ctx.Done()
				atomic.AddInt32(&active, -1)
				return ctx.Err()
			})
		},
	})

	first, err := f.Spawn(context.Background(), "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.Spawn(context.Background(), "second")
	if err != nil {
		t.Fatal(err)
	}
	third, err := f.Spawn(context.Background(), "third")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-started:
		if got != first.ID {
			t.Fatalf("first started = %q, want %q", got, first.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("first worker did not start")
	}
	if second.Status() != StatusPending || third.Status() != StatusPending {
		t.Fatalf("queued statuses = %s/%s", second.Status(), third.Status())
	}
	if got := atomic.LoadInt32(&maxActive); got > 1 {
		t.Fatalf("max active = %d, want <= 1", got)
	}

	if err := f.Stop(first.ID); err != nil {
		t.Fatal(err)
	}
	first.Wait()
	select {
	case got := <-started:
		if got != second.ID {
			t.Fatalf("second started = %q, want %q", got, second.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not release capacity")
	}
	f.StopAll()
	if got := atomic.LoadInt32(&maxActive); got > 1 {
		t.Fatalf("max active = %d after release, want <= 1", got)
	}
}

func TestSchedulerRejectsChildSpawning(t *testing.T) {
	f := New(Config{Root: t.TempDir(), RepoRoot: t.TempDir(), NewRunner: func(*Agent) Runner {
		return RunnerFunc(func(context.Context, Sink) error { return nil })
	}})
	if _, err := f.SpawnReq(context.Background(), SpawnRequest{Task: "child", RequesterAgentID: "worker-1"}); err == nil {
		t.Fatal("child-originated spawn was accepted")
	}
}

func TestSchedulerAllowsWorkersAfterManyCompletions(t *testing.T) {
	f := New(Config{Root: t.TempDir(), RepoRoot: t.TempDir(), Policy: SubagentPolicy{MaxConcurrent: 1}, NewRunner: func(*Agent) Runner {
		return RunnerFunc(func(context.Context, Sink) error { return nil })
	}})
	t.Cleanup(f.StopAll)

	const workersBeyondFormerLifetimeLimit = 33
	for worker := 0; worker < workersBeyondFormerLifetimeLimit; worker++ {
		a, err := f.Spawn(context.Background(), fmt.Sprintf("worker-%d", worker))
		if err != nil {
			t.Fatalf("Spawn(worker-%d): %v", worker, err)
		}
		a.Wait()
		if got := a.Status(); got != StatusDone {
			t.Fatalf("worker-%d status = %s, want %s", worker, got, StatusDone)
		}
	}
}

func TestStopQueuedAgentBeforeRunnerAdmission(t *testing.T) {
	root := t.TempDir()
	started := make(chan string, 2)
	f := New(Config{
		Root: root, RepoRoot: root,
		Policy: SubagentPolicy{MaxConcurrent: 1, IdleTimeout: time.Hour},
		NewRunner: func(a *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, _ Sink) error {
				started <- a.ID
				<-ctx.Done()
				return ctx.Err()
			})
		},
	})
	first, err := f.Spawn(context.Background(), "first")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-started:
		if got != first.ID {
			t.Fatalf("first runner = %q, want %q", got, first.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("first runner did not start")
	}
	queued, err := f.Spawn(context.Background(), "queued")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Stop(queued.ID); err != nil {
		t.Fatal(err)
	}
	queued.Wait()
	if queued.Status() != StatusKilled {
		t.Fatalf("queued status = %s, want killed", queued.Status())
	}
	select {
	case got := <-started:
		t.Fatalf("stopped queued agent still reached runner: %q", got)
	case <-time.After(50 * time.Millisecond):
	}
	if err := f.Stop(first.ID); err != nil {
		t.Fatal(err)
	}
	first.Wait()
}

func TestCanceledQueuedAgentIsRemovedAndGetsResult(t *testing.T) {
	root := t.TempDir()
	// The runner may start before the test reaches its receive. Buffer the
	// one-shot readiness signal so scheduling cannot turn it into a lost wakeup.
	started := make(chan struct{}, 1)
	f := New(Config{
		Root: root, RepoRoot: root,
		Policy: SubagentPolicy{MaxConcurrent: 1, IdleTimeout: time.Hour},
		NewRunner: func(*Agent) Runner {
			return RunnerFunc(func(ctx context.Context, _ Sink) error {
				select {
				case started <- struct{}{}:
				default:
				}
				<-ctx.Done()
				return ctx.Err()
			})
		},
	})
	first, err := f.Spawn(context.Background(), "first")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	queued, err := f.Spawn(context.Background(), "queued")
	if err != nil {
		t.Fatal(err)
	}
	if queued.Status() != StatusPending {
		t.Fatalf("queued status = %s, want pending", queued.Status())
	}
	if err := f.Stop(queued.ID); err != nil {
		t.Fatal(err)
	}
	queued.Wait()
	f.mu.Lock()
	queueLen := len(f.queue)
	f.mu.Unlock()
	if queueLen != 0 {
		t.Fatalf("canceled queue entries = %d, want 0", queueLen)
	}
	result, err := f.ReadResult(queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != ResultCanceled {
		t.Fatalf("queued cancellation result = %s, want canceled", result.Status)
	}
	if err := f.Stop(first.ID); err != nil {
		t.Fatal(err)
	}
	first.Wait()
}

func TestSpawnIDsRemainUniqueWithSameClock(t *testing.T) {
	now := time.Unix(123, 0)
	f := New(Config{
		Root: t.TempDir(), RepoRoot: t.TempDir(), Now: func() time.Time { return now },
		NewRunner: func(*Agent) Runner {
			return RunnerFunc(func(context.Context, Sink) error { return nil })
		},
	})
	first, err := f.Spawn(context.Background(), "same task")
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.Spawn(context.Background(), "same task")
	if err != nil {
		t.Fatal(err)
	}
	first.Wait()
	second.Wait()
	if first.ID == second.ID {
		t.Fatalf("duplicate agent ids: %q", first.ID)
	}
}

func TestStructuredFallbackResultIsDurable(t *testing.T) {
	root := t.TempDir()
	f := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(*Agent) Runner {
			return RunnerFunc(func(context.Context, Sink) error {
				return nil
			})
		},
	})
	a, err := f.Spawn(context.Background(), "result fallback")
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	result, err := f.ReadResult(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != ResultSucceeded || result.AgentID != a.ID || result.TurnID == "" {
		t.Fatalf("result = %+v", result)
	}
	if _, err := os.Stat(filepath.Join(root, "agents", a.ID, "result.json")); err != nil {
		t.Fatalf("result file missing: %v", err)
	}
}
