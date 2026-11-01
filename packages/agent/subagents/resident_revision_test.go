package subagents

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func requireRevisionAdvance(t *testing.T, m *ResidentManager, mutate func()) {
	t.Helper()
	before := m.Revision()
	mutate()
	if m.Revision() <= before {
		t.Fatal("snapshot mutation did not advance revision")
	}
}

func TestResidentRevisionChildMutations(t *testing.T) {
	m := NewResidentManager(t.TempDir(), nil)
	j, err := OpenResidentJournal(m.root, "child")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "child", SessionID: "session", Provider: "test", Model: "test"}
	if err := j.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	child := newJournaledResidentChild(spec, j, nil, nil)
	child.setUpdateObserver(func(history bool) { m.notifyUpdate("child", history) })
	t.Cleanup(func() { _ = child.Close(context.Background()) })
	for _, tt := range []struct {
		name   string
		mutate func()
	}{
		{"state", func() { child.setState(ResidentIdle) }},
		{"turn start", func() { child.startTurn("turn") }},
		{"activity", child.recordActivity},
		{"waiting model", func() {
			if err := j.RecordAgentEvent(core.EvRequestStarted{}); err != nil {
				t.Fatal(err)
			}
		}},
		{"stream activity", func() {
			if err := j.RecordAgentEvent(core.EvTextDelta{Delta: "text"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"usage", func() {
			if err := j.RecordAgentEvent(core.EvUsage{Cumulative: provider.Usage{InputTokens: 42}, Usage: provider.Usage{InputTokens: 7}}); err != nil {
				t.Fatal(err)
			}
		}},
		{"history", func() {
			if err := j.RecordAgentEvent(core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant}}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := m.Revision()
			tt.mutate()
			if delta := m.Revision() - before; delta != 1 {
				t.Fatalf("snapshot mutation advanced revision by %d, want one notification", delta)
			}
		})
	}
	before := m.Revision()
	_ = m.Snapshot()
	if m.Revision() != before {
		t.Fatal("read advanced revision")
	}
}

func TestResidentRevisionQueuedTurns(t *testing.T) {
	m := NewResidentManager(t.TempDir(), nil)
	// No control loop is needed to exercise this setter.
	child := &ResidentChild{}
	child.setUpdateObserver(func(history bool) { m.notifyUpdate("child", history) })
	before := m.Revision()
	child.setQueuedTurns(3)
	if delta := m.Revision() - before; delta != 1 {
		t.Fatalf("queued turns advanced revision by %d, want 1", delta)
	}
	child.setQueuedTurns(3)
	if m.Revision() != before+1 {
		t.Fatal("unchanged queued count notified again")
	}
}

func TestResidentRevisionLifecycleAndRecovery(t *testing.T) {
	root := t.TempDir()
	m := NewResidentManager(root, func(ResidentChildSpec, *ResidentJournal) (ResidentRuntime, error) {
		return ResidentTurnRunner(func(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() }), nil
	})
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	spec := ResidentChildSpec{ID: "child", SessionID: "session", Provider: "test", Model: "test"}
	requireRevisionAdvance(t, m, func() {
		if _, err := m.Spawn(context.Background(), spec, "task"); err != nil {
			t.Fatal(err)
		}
	})
	requireRevisionAdvance(t, m, func() {
		if err := m.Stop(context.Background(), "child"); err != nil {
			t.Fatal(err)
		}
	})
	requireRevisionAdvance(t, m, func() {
		if err := m.Resume(context.Background(), "child", "again"); err != nil {
			t.Fatal(err)
		}
	})
	requireRevisionAdvance(t, m, func() {
		if err := m.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	recovered := NewResidentManager(root, nil)
	requireRevisionAdvance(t, recovered, func() {
		if errs := recovered.Reconcile(); len(errs) != 0 {
			t.Fatal(errs)
		}
	})
	if len(recovered.Snapshot()) != 1 {
		t.Fatal("missing recovered child")
	}
	// A lease changes the recovered projection even without a live child.
	lease, err := OpenResidentJournal(root, "child")
	if err != nil {
		t.Fatal(err)
	}
	requireRevisionAdvance(t, recovered, func() {
		if errs := recovered.Reconcile(); len(errs) != 0 {
			t.Fatal(errs)
		}
	})
	if !recovered.Snapshot()[0].OwnedElsewhere {
		t.Fatal("missing foreign ownership")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	requireRevisionAdvance(t, recovered, func() {
		if errs := recovered.Reconcile(); len(errs) != 0 {
			t.Fatal(errs)
		}
	})
	if recovered.Snapshot()[0].OwnedElsewhere {
		t.Fatal("stale ownership")
	}
	if err := os.Rename(filepath.Join(root, "child"), filepath.Join(t.TempDir(), "child")); err != nil {
		t.Fatal(err)
	}
	requireRevisionAdvance(t, recovered, func() {
		if errs := recovered.Reconcile(); len(errs) != 0 {
			t.Fatal(errs)
		}
	})
	if len(recovered.Snapshot()) != 0 {
		t.Fatal("stale recovered set")
	}
}

func TestResidentRevisionSpawnRollback(t *testing.T) {
	m := NewResidentManager(t.TempDir(), func(ResidentChildSpec, *ResidentJournal) (ResidentRuntime, error) {
		return ResidentTurnRunner(func(context.Context, string) error { return nil }), nil
	})
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	// Empty input fails before publication; canceled admission rolls back a
	// published child and must invalidate even if a reader saw that child.
	spec := ResidentChildSpec{ID: "child", SessionID: "session", Provider: "test", Model: "test"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	requireRevisionAdvance(t, m, func() {
		if _, err := m.Spawn(ctx, spec, "task"); err == nil {
			t.Fatal("canceled spawn succeeded")
		}
	})
	if len(m.Snapshot()) != 0 {
		t.Fatal("rolled-back child still visible")
	}
}

func TestResidentRevisionSteerAndInterrupt(t *testing.T) {
	runtime := newSteerableRuntime()
	m, completions := newSteerTestManager(t, runtime)
	spec := ResidentChildSpec{ID: "child", SessionID: "session", Provider: "test", Model: "test"}
	if _, err := m.Spawn(t.Context(), spec, "task"); err != nil {
		t.Fatal(err)
	}
	runtime.awaitStart(t)
	requireRevisionAdvance(t, m, func() {
		outcome, err := m.ResumeFollowUp(t.Context(), "child", "follow up", ResumeSteer, "unused", nil)
		if err != nil || !outcome.Steered {
			t.Fatalf("steer = %#v, %v", outcome, err)
		}
	})
	requireRevisionAdvance(t, m, func() {
		interrupted, err := m.Interrupt(t.Context(), "child")
		if err != nil || !interrupted {
			t.Fatalf("interrupt = %t, %v", interrupted, err)
		}
	})
	select {
	case <-completions:
	case <-time.After(5 * time.Second):
		t.Fatal("missing interrupted completion")
	}
	snapshot, _ := m.SnapshotFor("child")
	if snapshot.PendingFollowUps != 0 || snapshot.State != ResidentInterrupted {
		t.Fatalf("stale terminal projection: %#v", snapshot)
	}
}
