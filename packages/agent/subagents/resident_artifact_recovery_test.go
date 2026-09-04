package subagents

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReconcileResidentJournalRestoresCapturedArtifacts(t *testing.T) {
	for _, damage := range []string{"missing", "corrupt"} {
		for _, captured := range []bool{false, true} {
			name := damage + "/without-capture"
			if captured {
				name = damage + "/with-capture"
			}
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				spec := ResidentChildSpec{ID: "artifact-recovery", SessionID: "session", InitialTurnID: "initial", Provider: "openai", Model: "test"}
				journal, err := OpenResidentJournal(root, spec.ID)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = journal.Close() })
				if err := journal.Accept(spec, "inspect and change files"); err != nil {
					t.Fatal(err)
				}
				if err := journal.RecordTurnStarted(spec, spec.InitialTurnID); err != nil {
					t.Fatal(err)
				}
				var capture *WorkspaceCapture
				if captured {
					capture = &WorkspaceCapture{Patch: []byte("partial patch\n"), ChangedFiles: []string{"README.md", "file with spaces.go"}}
				} else if err := writeResidentPatch(journal.Dir(), []byte("stale patch from an earlier turn\n")); err != nil {
					t.Fatal(err)
				}
				if err := journal.RecordTurnFinishedWithCapture(spec, spec.InitialTurnID, ErrBudgetExceeded, capture); err != nil {
					t.Fatal(err)
				}
				original, err := journal.Result()
				if err != nil {
					t.Fatal(err)
				}
				if captured && (original.PatchRef != PatchRef(spec.ID) || !reflect.DeepEqual(original.ChangedFiles, capture.ChangedFiles)) {
					t.Fatalf("capture was not published: %#v", original)
				}
				if err := journal.Close(); err != nil {
					t.Fatal(err)
				}
				dir := filepath.Join(root, spec.ID)
				transcriptPath := filepath.Join(dir, residentTranscriptName)
				before, err := os.ReadFile(transcriptPath)
				if err != nil {
					t.Fatal(err)
				}
				resultPath := filepath.Join(dir, residentResultName)
				if damage == "missing" {
					err = os.Remove(resultPath)
				} else {
					err = os.WriteFile(resultPath, []byte("invalid JSON"), 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
				for range 2 {
					if _, err := ReconcileResidentJournal(dir); err != nil {
						t.Fatal(err)
					}
					result, err := ReadResidentResult(resultPath)
					if err != nil {
						t.Fatal(err)
					}
					if result.State != ResidentBudgetExhausted || result.TurnID != spec.InitialTurnID || result.ErrorCode != residentErrorBudgetExhausted || result.Handoff == "" || result.PatchRef != original.PatchRef || !reflect.DeepEqual(result.ChangedFiles, original.ChangedFiles) {
						t.Fatalf("reconstructed result = %#v, original = %#v", result, original)
					}
				}
				after, err := os.ReadFile(transcriptPath)
				if err != nil || string(after) != string(before) {
					t.Fatalf("reconciliation modified transcript: %v", err)
				}
				if captured {
					patch, err := os.ReadFile(filepath.Join(dir, residentPatchName))
					if err != nil || string(patch) != string(capture.Patch) {
						t.Fatalf("patch = %q, error = %v", patch, err)
					}
				}
			})
		}
	}
}
