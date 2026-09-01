package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionLogProjectionsRejectInvalidRowsConsistently(t *testing.T) {
	meta := SessionMeta{ID: "session-1", CWD: "/workspace", Started: time.Now().UTC()}
	metaLine, err := json.Marshal(sessionLine{Type: "meta", Meta: &meta})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		contents string
		want     string
	}{
		{name: "missing meta", contents: `{"type":"message","message":{"role":"user","content":[{"text":"hello"}]}}` + "\n", want: "first row is not meta"},
		{name: "invalid message", contents: string(metaLine) + "\n" + `{"type":"message","message":{"role":"user"}}` + "\n", want: "invalid message row"},
		{name: "invalid usage", contents: string(metaLine) + "\n" + `{"type":"usage","cumulative":null}` + "\n", want: "usage row has no cumulative usage"},
		{name: "invalid rename", contents: string(metaLine) + "\n" + `{"type":"rename"}` + "\n", want: "rename row has no title"},
		{name: "unknown type", contents: string(metaLine) + "\n" + `{"type":"future-row"}` + "\n", want: `unknown row type "future-row"`},
		{name: "changed session id", contents: string(metaLine) + "\n" + `{"type":"meta","meta":{"id":"session-2","cwd":"/workspace"}}` + "\n", want: `session id changed from "session-1" to "session-2"`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			if err := os.WriteFile(path, []byte(test.contents), 0o644); err != nil {
				t.Fatal(err)
			}

			_, snapshotErr := ReadSessionSnapshot(path)
			_, historyErr := ReadSessionHistory(path)
			_, branchErr := readExtensionStateAtFork(context.Background(), path, 0)
			for name, err := range map[string]error{
				"snapshot": snapshotErr,
				"history":  historyErr,
				"branch":   branchErr,
			} {
				if err == nil || !strings.Contains(err.Error(), test.want) {
					t.Errorf("%s error = %v, want substring %q", name, err, test.want)
				}
			}
		})
	}
}

func TestBranchSessionHonorsCancellationBeforeSnapshotRead(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	missingParent := filepath.Join(root, "missing.jsonl")
	if _, err := branchSession(ctx, missingParent, root, "/workspace", "test", 0, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("branchSession error = %v, want context.Canceled", err)
	}
}

func TestBranchSessionThreadsCancellationToExtensionStateRead(t *testing.T) {
	root := t.TempDir()
	session, err := NewSession(root, "/workspace", "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendExtensionState("extension", json.RawMessage(`{"version":1}`)); err != nil {
		t.Fatal(err)
	}
	path := session.Path
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := branchSession(ctx, path, root, "/workspace", "test", 0, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("branchSession error = %v, want context.Canceled", err)
	}
}
