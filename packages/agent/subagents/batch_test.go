package subagents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBatchIDsRemainUniqueWithFixedClock(t *testing.T) {
	now := time.Unix(123, 0)
	f := New(Config{
		Root: t.TempDir(), RepoRoot: t.TempDir(), Now: func() time.Time { return now },
		Policy: SubagentPolicy{MaxConcurrent: 2},
		NewRunner: func(*Agent) Runner {
			return RunnerFunc(func(context.Context, Sink) error { return nil })
		},
	})
	t.Cleanup(f.StopAll)
	first, err := f.SpawnBatch(context.Background(), BatchRequest{Tasks: []string{"one"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.SpawnBatch(context.Background(), BatchRequest{Tasks: []string{"two"}})
	if err != nil {
		t.Fatal(err)
	}
	first.Wait()
	second.Wait()
	if first.ID == second.ID {
		t.Fatalf("duplicate batch ids: %q", first.ID)
	}
}

func TestBatchReloadsDurableResult(t *testing.T) {
	root := t.TempDir()
	newSupervisor := func() *Supervisor {
		return New(Config{
			Root: root, RepoRoot: root,
			NewRunner: func(*Agent) Runner {
				return RunnerFunc(func(context.Context, Sink) error { return nil })
			},
		})
	}
	first := newSupervisor()
	t.Cleanup(first.StopAll)
	batch, err := first.SpawnBatch(context.Background(), BatchRequest{Context: "review", Tasks: []string{"one", "two"}})
	if err != nil {
		t.Fatal(err)
	}
	want, err := first.WaitBatch(batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want.Status != BatchSucceeded {
		t.Fatalf("first batch status = %s", want.Status)
	}
	// WaitBatch returns once the aggregate result is available, which can
	// precede each child's final metadata write. Wait for that finalisation
	// before another supervisor reloads the same durable state.
	for _, childID := range batch.ChildIDs {
		child := first.Get(childID)
		if child == nil {
			t.Fatalf("first supervisor is missing child %q", childID)
		}
		child.Wait()
	}

	second := newSupervisor()
	t.Cleanup(second.StopAll)
	loaded, errs := second.Reload()
	if len(errs) != 0 {
		t.Fatalf("reload errors: %v", errs)
	}
	if loaded != 2 {
		t.Fatalf("reload loaded %d agents, want 2", loaded)
	}
	got, err := second.WaitBatch(batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != BatchSucceeded || len(got.Results) != 2 {
		t.Fatalf("reloaded batch = %#v", got)
	}
}

func TestBatchReloadRejectsUnsafeIDs(t *testing.T) {
	root := t.TempDir()
	batchDir := filepath.Join(root, "batches")
	if err := os.MkdirAll(batchDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(BatchResult{
		Version: ProtocolVersion, BatchID: "../escape", TaskCount: 0, Status: BatchRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(batchDir, "queued.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	f := New(Config{Root: root, RepoRoot: root})
	if _, errs := f.Reload(); len(errs) == 0 {
		t.Fatal("unsafe batch metadata was accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "escape.json")); !os.IsNotExist(err) {
		t.Fatalf("unsafe batch path was written: %v", err)
	}
}

func TestBatchSpawnWaitAndCollect(t *testing.T) {
	root := t.TempDir()
	f := New(Config{
		Root: root, RepoRoot: root,
		Policy: SubagentPolicy{MaxConcurrent: 2, MaxConcurrentPerParent: 2, DefaultTimeout: 0, IdleTimeout: 0},
		NewRunner: func(*Agent) Runner {
			return RunnerFunc(func(context.Context, Sink) error { return nil })
		},
	})
	t.Cleanup(f.StopAll)
	batch, err := f.SpawnBatch(context.Background(), BatchRequest{Context: "review", Tasks: []string{"one", "two"}, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.WaitBatch(batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != BatchSucceeded || len(result.ChildIDs) != 2 || len(result.Results) != 2 {
		t.Fatalf("batch result = %#v", result)
	}
	if batch.Status() != BatchSucceeded {
		t.Fatalf("batch status = %s", batch.Status())
	}
}

func TestBatchResultWaitCancellationDoesNotCancelWorker(t *testing.T) {
	f := New(Config{Root: t.TempDir(), RepoRoot: t.TempDir()})
	a := &Agent{
		ID:     "worker-1",
		status: StatusRunning,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := f.waitForBatchResult(ctx, a)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error = %v, want context.Canceled", err)
	}
	if result != nil {
		t.Fatalf("wait result = %#v, want nil", result)
	}
	if got := a.Status(); got != StatusRunning {
		t.Fatalf("worker status = %s, want running", got)
	}
}

func TestBatchAllowsMoreThanFormerLifetimeLimit(t *testing.T) {
	const workersBeyondFormerLifetimeLimit = 33
	tasks := make([]string, workersBeyondFormerLifetimeLimit)
	for i := range tasks {
		tasks[i] = fmt.Sprintf("worker-%d", i)
	}
	f := New(Config{
		Root: t.TempDir(), RepoRoot: t.TempDir(),
		Policy: SubagentPolicy{MaxConcurrent: 1, MaxConcurrentPerParent: 1},
		NewRunner: func(*Agent) Runner {
			return RunnerFunc(func(context.Context, Sink) error { return nil })
		},
	})
	t.Cleanup(f.StopAll)

	batch, err := f.SpawnBatch(context.Background(), BatchRequest{Tasks: tasks, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.WaitBatch(batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != BatchSucceeded || len(result.ChildIDs) != len(tasks) || len(result.Results) != len(tasks) {
		t.Fatalf("batch result = %#v", result)
	}
}
