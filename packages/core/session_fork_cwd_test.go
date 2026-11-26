package core

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func writeForkParent(t *testing.T, root, cwd string, compact bool) *Session {
	t.Helper()
	parent, err := NewSession(root, cwd, "anthropic", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"one", "two", "three"} {
		if err := parent.AppendMessage(textMessage(provider.RoleUser, text)); err != nil {
			t.Fatal(err)
		}
	}
	if compact {
		if err := parent.AppendCompaction([]provider.Message{{
			Role:    provider.RoleUser,
			Meta:    map[string]string{"compaction": "true"},
			Content: []provider.Content{provider.TextBlock{Text: "summary"}},
		}}); err != nil {
			t.Fatal(err)
		}
		for _, text := range []string{"four", "five"} {
			if err := parent.AppendMessage(textMessage(provider.RoleAssistant, text)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	return parent
}

func TestForkSessionToCWDCopiesFullTranscript(t *testing.T) {
	for _, compact := range []bool{false, true} {
		root := t.TempDir()
		srcCWD := filepath.Join(root, "src")
		dstCWD := filepath.Join(root, "dst")
		parent := writeForkParent(t, root, srcCWD, compact)
		before, err := os.ReadFile(parent.Path)
		if err != nil {
			t.Fatal(err)
		}
		want, err := ReadSessionSnapshot(parent.Path)
		if err != nil {
			t.Fatal(err)
		}

		path, err := ForkSessionToCWD(context.Background(), parent.Path, root, dstCWD, "test")
		if err != nil {
			t.Fatalf("ForkSessionToCWD(compact=%v): %v", compact, err)
		}
		if filepath.Dir(path) != SessionsDir(root, dstCWD) {
			t.Errorf("fork dir = %q, want %q", filepath.Dir(path), SessionsDir(root, dstCWD))
		}
		after, err := os.ReadFile(parent.Path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Error("parent file changed")
		}
		got, err := ReadSessionSnapshot(path)
		if err != nil {
			t.Fatal(err)
		}
		if got.Meta.CWD != dstCWD || got.Meta.Parent != parent.Meta.ID || got.Meta.HideFromSessions {
			t.Errorf("meta = cwd %q parent %q hidden %v", got.Meta.CWD, got.Meta.Parent, got.Meta.HideFromSessions)
		}
		if len(got.Messages) != len(want.Messages) {
			t.Errorf("compact=%v: messages = %d, want %d", compact, len(got.Messages), len(want.Messages))
		}
		listed := ListSessions(root, dstCWD)
		found := false
		for _, p := range listed {
			found = found || p == path
		}
		if !found {
			t.Errorf("fork %q missing from listing %v", path, listed)
		}
	}
}

func TestForkSessionToCWDRejectsRelativeCWD(t *testing.T) {
	root := t.TempDir()
	parent := writeForkParent(t, root, filepath.Join(root, "src"), false)
	for _, cwd := range []string{"", "rel/dir"} {
		if _, err := ForkSessionToCWD(context.Background(), parent.Path, root, cwd, "test"); err == nil {
			t.Errorf("cwd %q: want error", cwd)
		}
	}
}

func TestForkSessionToCWDCancellationLeavesNoFile(t *testing.T) {
	root := t.TempDir()
	parent := writeForkParent(t, root, filepath.Join(root, "src"), true)
	dstCWD := filepath.Join(root, "dst")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ForkSessionToCWD(ctx, parent.Path, root, dstCWD, "test"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	entries, err := os.ReadDir(SessionsDir(root, dstCWD))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("destination has %d entries after cancellation", len(entries))
	}
}
