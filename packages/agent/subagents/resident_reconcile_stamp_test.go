package subagents

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func stampedResident(t *testing.T) (string, ResidentMetadata, ResidentChildSpec) {
	t.Helper()
	root := t.TempDir()
	spec := ResidentChildSpec{ID: "child", SessionID: "session", InitialTurnID: "turn", Provider: "test", Model: "test", Profile: "reviewer"}
	j, err := OpenResidentJournal(root, spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, spec.ID)
	metadata, accepted, err := reconcileResidentJournalWithSpec(dir)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.State != ResidentInterrupted {
		t.Fatalf("repair state = %s", metadata.State)
	}
	if _, _, ok := readResidentReconcileStamp(dir); !ok {
		t.Fatal("successful repair did not produce a valid stamp")
	}
	return dir, metadata, accepted
}

// Deliberately preserve the fingerprint while making the transcript undecodable
// to prove the hit path does not read it. Normal writers change size or mtime;
// the stamp is not intended to detect out-of-band timestamp-preserving tampering.
func hideStampedTranscript(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, residentTranscriptName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("x"), int(info.Size()))
	data[len(data)-1] = '\n'
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
}

func TestResidentReconcileStampSkipsJournal(t *testing.T) {
	dir, wantMetadata, wantSpec := stampedResident(t)
	hideStampedTranscript(t, dir)
	gotMetadata, gotSpec, err := reconcileResidentJournalWithSpec(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotMetadata, wantMetadata) || !reflect.DeepEqual(gotSpec, wantSpec) {
		t.Fatal("stamp changed reconciliation results")
	}
}

func TestResidentReconcileStampInvalidation(t *testing.T) {
	for _, kind := range []string{"size", "mtime", "version", "corrupt", "missing", "metadata", "result"} {
		t.Run(kind, func(t *testing.T) {
			dir, _, _ := stampedResident(t)
			hideStampedTranscript(t, dir)
			path := filepath.Join(dir, residentReconcileStampName)
			switch kind {
			case "size":
				f, err := os.OpenFile(filepath.Join(dir, residentTranscriptName), os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.WriteString("\n"); err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			case "mtime":
				info, err := os.Stat(filepath.Join(dir, residentTranscriptName))
				if err != nil {
					t.Fatal(err)
				}
				changed := info.ModTime().Add(1000000000)
				if err := os.Chtimes(filepath.Join(dir, residentTranscriptName), changed, changed); err != nil {
					t.Fatal(err)
				}
			case "version":
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var stamp residentReconcileStamp
				if err := json.Unmarshal(data, &stamp); err != nil {
					t.Fatal(err)
				}
				stamp.Version++
				if err := writeResidentProjection(dir, residentReconcileStampName, stamp); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "metadata":
				if err := os.Remove(filepath.Join(dir, residentMetadataName)); err != nil {
					t.Fatal(err)
				}
			case "result":
				// This fixture has no result. Adding a stale one must invalidate too.
				if err := os.WriteFile(filepath.Join(dir, residentResultName), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, ok := readResidentReconcileStamp(dir); ok {
				t.Fatal("stale stamp accepted")
			}
			_, _, err := reconcileResidentJournalWithSpec(dir)
			if !errors.Is(err, ErrCorruptResidentJournal) {
				t.Fatalf("expected full journal validation, got %v", err)
			}
		})
	}
}

func TestResidentReconcileStampWriteFailureIsNonfatal(t *testing.T) {
	dir, _, _ := stampedResident(t)
	path := filepath.Join(dir, residentReconcileStampName)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reconcileResidentJournalWithSpec(dir); err != nil {
		t.Fatalf("stamp write failure escaped: %v", err)
	}
}
