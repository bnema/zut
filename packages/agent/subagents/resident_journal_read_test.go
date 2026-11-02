package subagents

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReadResidentJournalReusesBufferWithoutAliasing(t *testing.T) {
	first := residentRecord{Version: residentJournalVersion, Type: residentRecordAccepted, Prompt: strings.Repeat("a", 100000)}
	second := residentRecord{Version: residentJournalVersion, Type: residentRecordInterrupted}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	data := append(append(append(append([]byte{}, a...), '\n'), b...), '\n')
	// An incomplete final record is not committed, even when it is invalid JSON.
	data = append(data, []byte(`{"unfinished`)...)
	path := filepath.Join(t.TempDir(), residentTranscriptName)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	records, err := ReadResidentJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Prompt != first.Prompt || records[1].Type != second.Type {
		t.Fatalf("unexpected decoded records: count=%d", len(records))
	}
	if !bytes.Equal(records[0].raw, append(a, '\n')) || !bytes.Equal(records[1].raw, append(b, '\n')) {
		t.Fatal("raw records were aliased or changed")
	}
}

func TestReadResidentJournalRecordSizeBoundary(t *testing.T) {
	for _, extra := range []int{0, 1} {
		t.Run(string(rune('0'+extra)), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), residentTranscriptName)
			data := []byte(`{"prompt":"` + strings.Repeat("x", residentMaxRecordBytes-len(`{"prompt":""}`)-1+extra) + "\"}\n")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			records, err := ReadResidentJournal(path)
			if extra == 0 {
				if err != nil || len(records) != 1 {
					t.Fatalf("boundary: count=%d err=%v", len(records), err)
				}
			} else if !errors.Is(err, ErrCorruptResidentJournal) {
				t.Fatalf("oversize error = %v", err)
			}
		})
	}
}

func TestReconcileResidentJournalRetainsAcceptedSpec(t *testing.T) {
	root := t.TempDir()
	spec := ResidentChildSpec{ID: "child", SessionID: "session", Provider: "test", Model: "test", Profile: "reviewer", Tools: []string{"read"}}
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
	_, got, err := reconcileResidentJournalWithSpec(filepath.Join(root, spec.ID))
	if err != nil {
		t.Fatal(err)
	}
	records, err := ReadResidentJournal(filepath.Join(root, spec.ID, residentTranscriptName))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, *records[0].Spec) {
		t.Fatalf("spec differs: got %#v want %#v", got, *records[0].Spec)
	}
}
