package subagents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestReconcileIgnoresPruneStaging(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{".prune-stale", ".private"} {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, residentTranscriptName), []byte("invalid\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	m := NewResidentManager(root, nil)
	if errs := m.Reconcile(); len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, name := range []string{".prune-stale", ".private"} {
		entries, err := os.ReadDir(filepath.Join(root, name))
		if err != nil || len(entries) != 1 {
			t.Fatalf("staging touched: %v %v", entries, err)
		}
	}
}

func oldResidentForPrune(t *testing.T, root string) string {
	t.Helper()
	id := uuid.NewString()
	j, err := OpenResidentJournal(root, id)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	at := time.Now().Add(-8 * 24 * time.Hour)
	spec := ResidentChildSpec{ID: id, SessionID: "child", ParentSessionID: "parent", InitialTurnID: "turn", Provider: "test", Model: "test"}
	for _, r := range []residentRecord{
		{Version: residentJournalVersion, Type: residentRecordAccepted, Time: at, Spec: &spec},
		{Version: residentJournalVersion, Type: residentRecordTurnStarted, Time: at, TurnID: "turn"},
		{Version: residentJournalVersion, Type: residentRecordTurnFinished, Time: at, TurnID: "turn", Outcome: residentOutcomeCompleted},
	} {
		if err := j.appendSync(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeResidentMetadata(j.Dir(), j.metadata(spec, ResidentIdle, at)); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestResumeDoesNotRecreateRemovedResident(t *testing.T) {
	root := t.TempDir()
	id := oldResidentForPrune(t, root)
	m := NewResidentManager(root, func(ResidentChildSpec, *ResidentJournal) (ResidentRuntime, error) {
		t.Fatal("factory called for removed child")
		return nil, nil
	})
	if errs := m.Reconcile(); len(errs) != 0 {
		t.Fatal(errs)
	}
	if _, ok := m.SnapshotFor(id); !ok {
		t.Fatal("child not discovered")
	}
	path := filepath.Join(root, id)
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := m.Resume(t.Context(), id, "resume"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resume error = %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("child directory recreated: %v", err)
	}
}

func TestPruneRechecksParentBeforeStaging(t *testing.T) {
	root := t.TempDir()
	id := oldResidentForPrune(t, root)
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	checks := 0
	p := ResidentPruner{Root: root, Cutoff: cutoff, ParentActivity: func(string) (time.Time, error) {
		checks++
		if checks == 3 {
			return cutoff.Add(time.Hour), nil
		}
		return cutoff.Add(-time.Hour), nil
	}}
	candidates, notices, err := p.Scan()
	if err != nil || len(notices) != 0 || len(candidates) != 1 {
		t.Fatalf("scan = %v, %v, %v", candidates, notices, err)
	}
	if err := p.Delete(t.Context(), candidates[0]); err == nil || !strings.Contains(err.Error(), "parent became recently active") {
		t.Fatalf("delete error = %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != id {
		t.Fatalf("child not preserved: %v, %v", entries, err)
	}
}

func TestPruneCancellationBeforeNextStaging(t *testing.T) {
	root := t.TempDir()
	oldResidentForPrune(t, root)
	oldResidentForPrune(t, root)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	checks := 0
	pruner := ResidentPruner{Root: root, Cutoff: time.Now().Add(-7 * 24 * time.Hour), RecheckParentActivity: func(string) (time.Time, error) {
		checks++
		if checks == 2 {
			cancel()
		}
		return time.Time{}, nil
	}}
	candidates, notices, err := pruner.Scan()
	if err != nil || len(notices) != 0 || len(candidates) != 2 {
		t.Fatalf("scan: %v, %v, %v", candidates, notices, err)
	}
	if err := pruner.Delete(ctx, candidates[0]); err != nil {
		if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(32)) {
			t.Skip("Windows does not allow staging with the lease open")
		}
		t.Fatal(err)
	}
	if err := pruner.Delete(ctx, candidates[1]); !errors.Is(err, context.Canceled) {
		t.Fatalf("second deletion: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != candidates[1].ID {
		t.Fatalf("remaining entries: %v, %v", entries, err)
	}
}

func TestResidentPrunerEligibility(t *testing.T) {
	for _, tc := range []struct {
		name    string
		preview bool
	}{
		{"eligible", true}, {"recent", false}, {"running", false}, {"busy", true}, {"worktree", false},
		{"patch file", false}, {"patch result", false}, {"patch transcript", false}, {"failed required", false},
		{"partial", false}, {"pending", false}, {"recent parent", false}, {"non-uuid", false}, {"legacy", false},
		{"quarantined", false}, {"explicit parent", false}, {"invalid runtime parent", false}, {"conflicting result", false},
		{"unknown file", false}, {"orphan tool result", false}, {"dangling tool call", false}, {"recent usage", false},
		{"symlink", false}, {"replaced parent", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			old := time.Now().Add(-8 * 24 * time.Hour).UTC()
			kind := tc.name
			id := uuid.NewString()
			if kind == "non-uuid" {
				id = "not-a-uuid"
			}
			if kind == "legacy" {
				id = "agents"
			}
			dir := filepath.Join(root, id)
			spec := ResidentChildSpec{ID: id, SessionID: uuid.NewString(), InitialTurnID: "initial", Provider: "test", Model: "test", Profile: "review", ParentSessionID: "runtime:" + uuid.NewString(), Required: kind == "failed required" || kind == "eligible"}
			if kind == "worktree" {
				spec.WorkspaceMode = WorkspaceWorktree
			}
			if kind == "invalid runtime parent" {
				spec.ParentSessionID = "runtime:not-a-uuid"
			}
			j, err := OpenResidentJournal(root, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := j.Close(); err != nil {
				t.Fatal(err)
			}
			at := old
			if kind == "recent" {
				at = time.Now().UTC()
			}
			state, outcome := "idle", "completed"
			if kind == "failed required" {
				state, outcome = "failed", "failed"
			}
			if kind == "running" {
				state = "running"
			}
			write := func(name string, v any) {
				t.Helper()
				b, err := json.Marshal(v)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			write("metadata.json", ResidentMetadata{Version: 3, ID: id, SessionID: spec.SessionID, ParentSessionID: spec.ParentSessionID, State: ResidentState(state), UpdatedAt: at})
			finish := map[string]any{"version": 3, "type": "turn.finished", "time": at, "turn_id": "initial", "outcome": outcome, "changed_files": []string{"shared.go"}}
			if kind == "patch transcript" {
				finish["patch_ref"] = "patch"
			}
			var transcript bytes.Buffer
			enc := json.NewEncoder(&transcript)
			records := []map[string]any{{"version": 3, "type": "child.accepted", "time": at, "spec": spec}, {"version": 3, "type": "turn.started", "time": at, "turn_id": "initial"}}
			if kind != "running" {
				records = append(records, finish)
			}
			if kind == "pending" {
				records = append(records, map[string]any{"version": 3, "type": "turn.accepted", "time": at, "turn_id": "follow-up"})
			}
			if kind == "orphan tool result" {
				records = append(records, map[string]any{"version": 3, "type": "tool.result", "time": at, "tool_id": "missing", "tool_result": map[string]any{"content": []any{}}})
			}
			if kind == "dangling tool call" {
				records = append(records, map[string]any{"version": 3, "type": "tool.call", "time": at, "tool_id": "unfinished", "tool_name": "read", "tool_args": map[string]any{}})
			}
			if kind == "recent usage" {
				records = append(records, map[string]any{"version": 3, "type": "usage", "time": time.Now().UTC(), "usage": map[string]any{}})
			}
			for _, r := range records {
				if err := enc.Encode(r); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "partial" {
				transcript.WriteString("{")
			}
			if err := os.WriteFile(filepath.Join(dir, "transcript.jsonl"), transcript.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "patch file":
				write("patch.diff", "artifact")
			case "patch result":
				write("result.json", ResidentResult{Version: 3, ID: id, TurnID: "initial", State: ResidentIdle, PatchRef: "patch"})
			case "quarantined":
				write("quarantined.json", true)
			case "conflicting result":
				write("result.json", ResidentResult{Version: 3, ID: id, TurnID: "initial", State: ResidentFailed})
			case "unknown file":
				write("unknown.json", true)
			case "busy":
				lease, err := acquireResidentLease(dir, false)
				if err != nil {
					t.Fatal(err)
				}
				defer lease.Close()
			}

			if kind == "symlink" {
				target := filepath.Join(t.TempDir(), "target")
				if err := os.Rename(dir, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, dir); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
			p := ResidentPruner{Root: root, Cutoff: old.Add(24 * time.Hour), ParentActivity: func(string) (time.Time, error) {
				switch kind {
				case "recent parent":
					return time.Now(), nil
				case "explicit parent", "invalid runtime parent":
					return time.Time{}, ErrPruneParentUnresolved
				}
				return old, nil
			}}
			if kind == "replaced parent" {
				p.RecheckParentActivity = func(string) (time.Time, error) { return time.Time{}, ErrPruneParentUnresolved }
			}
			candidates, _, err := p.Scan()
			if err != nil || (len(candidates) == 1) != tc.preview {
				t.Fatalf("preview = %v, error = %v", candidates, err)
			}
			deleted := false
			if tc.preview {
				err := p.Delete(t.Context(), candidates[0])
				switch kind {
				case "busy":
					if !errors.Is(err, ErrResidentLeaseBusy) {
						t.Fatalf("delete = %v", err)
					}
				case "replaced parent":
					if !errors.Is(err, ErrPruneParentUnresolved) {
						t.Fatalf("delete = %v", err)
					}
				default:
					if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(32)) {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					deleted = true
				}
			}
			_, err = os.Lstat(dir)
			if deleted && !os.IsNotExist(err) || !deleted && err != nil {
				t.Fatalf("deleted = %t, stat = %v", deleted, err)
			}
		})
	}
}
