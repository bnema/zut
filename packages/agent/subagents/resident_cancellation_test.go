package subagents

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestResidentCancellationPreservesInterruptedState(t *testing.T) {
	for _, turnErr := range []error{context.Canceled, errors.Join(context.Canceled, ErrBudgetExceeded)} {
		t.Run(turnErr.Error(), func(t *testing.T) {
			root := t.TempDir()
			manager := NewResidentManager(root, func(ResidentChildSpec, *ResidentJournal) (ResidentTurnRunner, error) {
				return func(context.Context, string) error { return turnErr }, nil
			})
			t.Cleanup(func() { _ = manager.Close(context.Background()) })
			spec := ResidentChildSpec{ID: "canceled", InitialTurnID: "initial", SessionID: "session", Provider: "openai", Model: "test", Required: true}
			completions := make(chan ResidentCompletion, 1)
			manager.SetCompletionObserver(func(c ResidentCompletion) { completions <- c })
			spawnCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if _, err := manager.Spawn(spawnCtx, spec, "investigate"); err != nil {
				t.Fatal(err)
			}
			cancel() // Accepted children outlive the caller's spawn context.
			completion := awaitBudgetCompletion(t, completions)
			snapshot, ok := manager.SnapshotFor(spec.ID)
			if !ok || snapshot.State != ResidentInterrupted || completion.Completion().Status != string(ResidentInterrupted) {
				t.Fatalf("snapshot = %#v, completion = %#v", snapshot, completion)
			}
			result, err := manager.Result(spec.ID)
			if err != nil || result.State != ResidentInterrupted || result.Handoff != "" || result.ErrorCode == residentErrorBudgetExhausted {
				t.Fatalf("result = %#v, error = %v", result, err)
			}
			if len(manager.UnmetRequired()) != 1 {
				t.Fatal("cancellation satisfied required work")
			}
			if err := manager.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			metadata, err := ReconcileResidentJournal(filepath.Join(root, spec.ID))
			if err != nil || metadata.State != ResidentInterrupted {
				t.Fatalf("reconciled metadata = %#v, error = %v", metadata, err)
			}
		})
	}
}
