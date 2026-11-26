package core

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func TestSessionFilesListableValidatesBuckets(t *testing.T) {
	root := t.TempDir()
	cwdA := filepath.Join(t.TempDir(), "workspace-a")
	cwdB := filepath.Join(t.TempDir(), "workspace-b")

	newSession := func(cwd, text string) *Session {
		t.Helper()
		s, err := NewSession(root, cwd, "provider", "model", "test")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: text}}}); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		return s
	}

	a := newSession(cwdA, "from a")
	b := newSession(cwdB, "from b")

	listable := func() []string {
		var paths []string
		files, err := SessionFiles(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if SessionListable(context.Background(), root, file.Path) {
				paths = append(paths, file.Path)
			}
		}
		return paths
	}
	got := listable()
	if len(got) != 2 || !containsPath(got, a.Path) || !containsPath(got, b.Path) {
		t.Fatalf("all scope = %q, want both valid sessions", got)
	}

	invalidDir := filepath.Join(root, "sessions", "not-a-cwd-bucket")
	if err := os.MkdirAll(invalidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(invalidDir, "invalid.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wrongBucket := filepath.Join(root, "sessions", "0123456789abcdef")
	if err := os.MkdirAll(wrongBucket, 0o755); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wrongBucket, "wrong.jsonl"), contents, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := listable(); len(got) != 2 {
		t.Fatalf("all scope included invalid session paths: %q", got)
	}
}

func TestSessionFilesReportsUnreadableBucket(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	root := t.TempDir()
	if files, err := SessionFiles(context.Background(), root); err != nil || files != nil {
		t.Fatalf("missing sessions dir = %v, %v", files, err)
	}
	s, err := NewSession(root, t.TempDir(), "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	bucket := filepath.Dir(s.Path)
	if err := os.Chmod(bucket, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(bucket, 0o755) })
	if _, err := SessionFiles(context.Background(), root); err == nil {
		t.Fatal("unreadable bucket not reported")
	}
}

func TestManagedSessionMetaRejectsWrongBucket(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	session, err := NewSession(root, cwd, "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "keep this session"}}}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ManagedSessionMeta(context.Background(), root, session.Path); err != nil {
		t.Fatalf("managed session metadata: %v", err)
	}
	wrongPath := filepath.Join(root, "sessions", "0123456789abcdef", "moved.jsonl")
	if err := os.MkdirAll(filepath.Dir(wrongPath), 0o755); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(session.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrongPath, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ManagedSessionMeta(context.Background(), root, wrongPath); err == nil {
		t.Fatal("wrong-bucket session was accepted")
	}
}

func containsPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}
