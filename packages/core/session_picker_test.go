package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func TestListSessionPathsContextScopesAndValidatesBuckets(t *testing.T) {
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

	if got := ListSessionPathsContext(context.Background(), root, cwdA, false); len(got) != 1 || got[0] != a.Path {
		t.Fatalf("current scope = %q, want %q", got, a.Path)
	}
	got := ListSessionPathsContext(context.Background(), root, cwdA, true)
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
	if got := ListSessionPathsContext(context.Background(), root, cwdA, true); len(got) != 2 {
		t.Fatalf("all scope included invalid session paths: %q", got)
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

func TestReadSessionSearchSegmentsIncludesOnlyUserAssistantText(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	s, err := NewSession(root, cwd, "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "find this user sentence"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "find this assistant sentence"}, provider.ToolCallBlock{ID: "call", Name: "secret-tool"}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: "call", Content: []provider.Content{provider.TextBlock{Text: "never expose tool result"}}}}},
		{Role: provider.RoleDeveloper, Content: []provider.Content{provider.TextBlock{Text: "never expose developer text"}}},
	} {
		if err := s.AppendMessage(msg); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	segments, err := ReadSessionSearchSegments(context.Background(), s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 {
		t.Fatalf("segments = %#v, want exactly user and assistant text", segments)
	}
	if segments[0].Text != "find this user sentence" || segments[1].Text != "find this assistant sentence" {
		t.Fatalf("segments = %#v", segments)
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
