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

// addMergedSideWorktree creates branch "side" at the primary HEAD in a new
// worktree, so it is merged into HEAD and eligible for cleanup.
func addMergedSideWorktree(t *testing.T, repo, branch string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), branch)
	gitTestOutput(t, repo, "worktree", "add", "-b", branch, path)
	return path
}

func branchExists(t *testing.T, repo, branch string) bool {
	t.Helper()
	return gitTestOutputAllowExit(t, repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).exitCode == 0
}

func TestDeleteMergedBranchRefusesSymbolicRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	mainBranch := gitTestOutput(t, repo, "branch", "--show-current")
	head := gitTestOutput(t, repo, "rev-parse", "HEAD")
	gitTestOutput(t, repo, "branch", "side")
	gitTestOutput(t, repo, "symbolic-ref", "refs/heads/side", "refs/heads/"+mainBranch)

	err := deleteMergedBranch(context.Background(), repo, "side", head)
	if err == nil || !strings.Contains(err.Error(), "symbolic") {
		t.Fatalf("error = %v, want symbolic-ref rejection", err)
	}
	if !branchExists(t, repo, mainBranch) {
		t.Fatal("symbolic ref deletion removed the target branch")
	}
}

func TestManageWorktreesDeleteBranchKeepsTargetWhenBranchBecomesSymref(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	mainBranch := gitTestOutput(t, repo, "branch", "--show-current")
	path := addMergedSideWorktree(t, repo, "side")
	tool := newManageWorktreesTestTool(repo)
	args := mustJSON(t, map[string]any{"action": "cleanup", "paths": []string{path}, "delete_branch": true})

	// The worktree is already removed when the delete progress is reported;
	// swap the branch for a symref to the primary branch at the same commit.
	progress := func(msg string) {
		if strings.HasPrefix(msg, "Deleting branch") {
			gitTestOutput(t, repo, "symbolic-ref", "refs/heads/side", "refs/heads/"+mainBranch)
		}
	}
	_, err := tool.Execute(context.Background(), args, progress)
	if err == nil || !strings.Contains(err.Error(), "symbolic") || !strings.Contains(err.Error(), "removed worktrees:") {
		t.Fatalf("error = %v, want symbolic-ref branch failure", err)
	}
	if !branchExists(t, repo, mainBranch) {
		t.Fatal("primary branch was deleted through a symbolic ref")
	}
}

func TestManageWorktreesDeleteBranchRefusesBranchCheckedOutAfterRemoval(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	for _, where := range []string{"primary", "worktree"} {
		t.Run(where, func(t *testing.T) {
			repo := initWorktreeTestRepo(t)
			path := addMergedSideWorktree(t, repo, "side")
			tool := newManageWorktreesTestTool(repo)
			args := mustJSON(t, map[string]any{"action": "cleanup", "paths": []string{path}, "delete_branch": true})
			progress := func(msg string) {
				if !strings.HasPrefix(msg, "Deleting branch") {
					return
				}
				if where == "primary" {
					gitTestOutput(t, repo, "checkout", "-q", "side")
				} else {
					gitTestOutput(t, repo, "worktree", "add", filepath.Join(t.TempDir(), "again"), "side")
				}
			}
			_, err := tool.Execute(context.Background(), args, progress)
			if err == nil || !strings.Contains(err.Error(), `delete branch "side"`) || !strings.Contains(err.Error(), "checked out") {
				t.Fatalf("error = %v, want checked-out branch failure", err)
			}
			if !branchExists(t, repo, "side") {
				t.Fatal("checked-out branch was deleted")
			}
		})
	}
}

func TestManageWorktreesDeleteBranchRequiresRefStorageWriteAccess(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	path := addMergedSideWorktree(t, repo, "side")
	scoped := func(write ...string) *ManageWorktreesTool {
		var p PermissionSet
		p.Bash.Mode = "ask"
		p.FS.Read = []string{repo, filepath.Dir(path)}
		p.FS.Write = write
		sandbox := NewSandbox(repo)
		sandbox.SetPermissions(&p)
		return &ManageWorktreesTool{CWD: repo, Sandbox: sandbox}
	}
	args := mustJSON(t, map[string]any{"action": "cleanup", "paths": []string{path}, "delete_branch": true})

	tool := scoped(path)
	if _, err := tool.Preview(context.Background(), args); err == nil || !strings.Contains(err.Error(), "outside declared scopes") {
		t.Fatalf("preview error = %v, want ref-storage write denial", err)
	}
	if _, err := tool.Execute(context.Background(), args, nil); err == nil || !strings.Contains(err.Error(), "outside declared scopes") {
		t.Fatalf("execute error = %v, want ref-storage write denial", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("worktree was removed despite denial: %v", err)
	}
	if !branchExists(t, repo, "side") {
		t.Fatal("branch was deleted despite denial")
	}

	// Without delete_branch only the worktree path needs write access.
	keep := mustJSON(t, map[string]any{"action": "cleanup", "paths": []string{path}})
	if _, err := tool.Preview(context.Background(), keep); err != nil {
		t.Fatalf("cleanup without delete_branch: %v", err)
	}
	// Granting the common git directory allows the full operation.
	tool = scoped(path, filepath.Join(repo, ".git"))
	if _, err := tool.Execute(context.Background(), args, nil); err != nil {
		t.Fatal(err)
	}
	if branchExists(t, repo, "side") {
		t.Fatal("branch remains after permitted cleanup")
	}
}

func TestManageWorktreesDetectsMovedMergeTargetAfterPreview(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	path := addMergedSideWorktree(t, repo, "side")
	gitTestOutput(t, repo, "branch", "target")
	tool := newManageWorktreesTestTool(repo)
	args := mustJSON(t, map[string]any{"action": "cleanup", "paths": []string{path}, "into": "target", "delete_branch": true})
	if _, err := tool.Preview(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	// Advance the unchecked-out target; side remains an ancestor, so the
	// worktree stays eligible and only the pinned target differs.
	tree := gitTestOutput(t, repo, "rev-parse", "target^{tree}")
	moved := gitTestOutput(t, repo, "commit-tree", tree, "-p", "target", "-m", "advance")
	gitTestOutput(t, repo, "update-ref", "refs/heads/target", moved)

	_, err := tool.Execute(context.Background(), args, nil)
	if err == nil || !strings.Contains(err.Error(), "changed since preview") {
		t.Fatalf("error = %v, want stale-state rejection", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("worktree was removed: %v", statErr)
	}
	if !branchExists(t, repo, "side") {
		t.Fatal("branch was deleted")
	}
}

func TestManageWorktreesPartialFailureReportsBranchErrorsAndProgress(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	first := addMergedSideWorktree(t, repo, "first")
	second := addMergedSideWorktree(t, repo, "second")
	// A stale lock makes deleting "first" fail after its worktree is removed.
	lock := filepath.Join(repo, ".git", "refs", "heads", "first.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	tool := newManageWorktreesTestTool(repo)
	args := mustJSON(t, map[string]any{"action": "cleanup", "paths": []string{first, second}, "delete_branch": true})
	// Dirty the second worktree so its removal fails.
	progress := func(msg string) {
		if strings.HasPrefix(msg, "Removing worktree") && strings.Contains(msg, filepath.ToSlash(second)) {
			if err := os.WriteFile(filepath.Join(second, "untracked.txt"), []byte("x"), 0o644); err != nil {
				t.Error(err)
			}
		}
	}
	_, err := tool.Execute(context.Background(), args, progress)
	if err == nil {
		t.Fatal("expected partial failure")
	}
	for _, want := range []string{"remove ", `delete branch "first"`, "removed worktrees: " + filepath.ToSlash(first)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q:\n%v", want, err)
		}
	}
	if _, statErr := os.Stat(second); statErr != nil {
		t.Fatalf("second worktree should remain: %v", statErr)
	}
}
