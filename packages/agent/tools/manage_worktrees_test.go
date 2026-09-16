package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestManageWorktreesListsCleanupEligibility(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	mergedPath := filepath.Join(t.TempDir(), "merged")
	gitTestOutput(t, repo, "branch", "merged")
	gitTestOutput(t, repo, "worktree", "add", mergedPath, "merged")
	dirtyPath := filepath.Join(t.TempDir(), "dirty")
	gitTestOutput(t, repo, "worktree", "add", "-b", "dirty", dirtyPath)
	if err := os.WriteFile(filepath.Join(dirtyPath, "dirty.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := newManageWorktreesTestTool(repo)
	result, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"action": "list"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := resultText(result)
	for _, want := range []string{"repository worktrees", "merged", "cleanup eligible", "dirty", "uncommitted changes", "primary worktree"} {
		if !strings.Contains(got, want) {
			t.Fatalf("result missing %q:\n%s", want, got)
		}
	}
}

func TestManageWorktreesCleanupRemovesOnlyExplicitCleanMergedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	path := filepath.Join(t.TempDir(), "merged")
	gitTestOutput(t, repo, "branch", "merged")
	gitTestOutput(t, repo, "worktree", "add", path, "merged")
	tool := newManageWorktreesTestTool(repo)
	args := mustJSON(t, map[string]any{"action": "cleanup", "paths": []string{path}})

	if _, err := tool.Preview(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("preview removed worktree: %v", err)
	}
	if _, err := tool.Execute(context.Background(), args, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("worktree remains: %v", err)
	}
	if got := gitTestOutput(t, repo, "show-ref", "--verify", "refs/heads/merged"); got == "" {
		t.Fatal("cleanup unexpectedly removed branch")
	}
}

func TestManageWorktreesRejectsDirtyCleanup(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	path := filepath.Join(t.TempDir(), "dirty")
	gitTestOutput(t, repo, "worktree", "add", "-b", "dirty", path)
	if err := os.WriteFile(filepath.Join(path, "dirty.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := newManageWorktreesTestTool(repo)
	_, err := tool.Preview(context.Background(), mustJSON(t, map[string]any{"action": "cleanup", "paths": []string{path}}))
	if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("error = %v, want dirty-worktree rejection", err)
	}
}

func TestWorktreeInventoryContextReportsRepositoryWorktrees(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	path := filepath.Join(t.TempDir(), "merged")
	gitTestOutput(t, repo, "branch", "merged")
	gitTestOutput(t, repo, "worktree", "add", path, "merged")

	contextText := WorktreeInventoryContext(context.Background(), repo)
	wantPath, err := canonicalOrParent(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[Repository worktrees]", filepath.ToSlash(wantPath), "worktree action=list"} {
		if !strings.Contains(contextText, want) {
			t.Fatalf("context missing %q:\n%s", want, contextText)
		}
	}
}

func TestManageWorktreesRejectsIgnoredFilesCleanup(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	path := filepath.Join(t.TempDir(), "ignored")
	gitTestOutput(t, repo, "worktree", "add", "-b", "ignored", path)
	if err := os.WriteFile(filepath.Join(path, ".gitignore"), []byte("local.env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestOutput(t, path, "add", ".gitignore")
	gitTestOutput(t, path, "commit", "-m", "ignore local file")
	gitTestOutput(t, repo, "merge", "--ff-only", "ignored")
	if err := os.WriteFile(filepath.Join(path, "local.env"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := newManageWorktreesTestTool(repo)
	_, err := tool.Preview(context.Background(), mustJSON(t, map[string]any{"action": "cleanup", "paths": []string{path}}))
	if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("error = %v, want ignored-file rejection", err)
	}
}

func TestManageWorktreesRejectsCleanupWithoutExplicitPaths(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	tool := newManageWorktreesTestTool(repo)
	_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"action": "cleanup"}), nil)
	if err == nil || !strings.Contains(err.Error(), "explicit path") {
		t.Fatalf("error = %v, want explicit-path rejection", err)
	}
}

func newManageWorktreesTestTool(cwd string) *ManageWorktreesTool {
	return &ManageWorktreesTool{CWD: cwd, Sandbox: NewSandbox(cwd)}
}
