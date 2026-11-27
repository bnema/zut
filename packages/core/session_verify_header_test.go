package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifySessionHeaderID(t *testing.T) {
	meta := func(id string) string {
		t.Helper()
		line, err := json.Marshal(sessionLine{Type: "meta", Meta: &SessionMeta{ID: id, CWD: "/work"}})
		if err != nil {
			t.Fatal(err)
		}
		return string(line) + "\n"
	}
	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{"matching ID", meta("parent-id") + "{\"type\":\"message\"}\n", false},
		{"matching ID without newline", meta("parent-id")[:len(meta("parent-id"))-1], false},
		{"different ID", meta("other-id"), true},
		{"malformed JSON", "{not json\n", true},
		{"non-meta first row", "{\"type\":\"message\"}\n" + meta("parent-id"), true},
		{"meta without ID", meta(""), true},
		{"empty file", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			err = VerifySessionHeaderID(path, info, "parent-id")
			if (err != nil) != tt.wantErr {
				t.Fatalf("VerifySessionHeaderID error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestVerifySessionHeaderIDRejectsReplacedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	line, err := json.Marshal(sessionLine{Type: "meta", Meta: &SessionMeta{ID: "parent-id"}})
	if err != nil {
		t.Fatal(err)
	}
	content := append(line, '\n')
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	original, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Same header, different file: identity must come from the stat taken
	// at lookup time, not from the header alone.
	replacement := filepath.Join(dir, "replacement.jsonl")
	if err := os.WriteFile(replacement, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, original.ModTime(), original.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if err := VerifySessionHeaderID(path, original, "parent-id"); err == nil {
		t.Fatal("VerifySessionHeaderID accepted a replaced file")
	}
}
