package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreeToolDispatchesListAndCreate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	root := t.TempDir()
	tool := &WorktreeTool{
		Create: &CreateWorktreeTool{CWD: repo, Sandbox: NewSandbox(repo), DefaultRoot: root},
		Manage: &ManageWorktreesTool{CWD: repo, Sandbox: NewSandbox(repo)},
	}
	if _, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"action": "list"}), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"action": "create", "branch": "feature/unified"}), nil); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(root, "*", "feature", "unified"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("created paths = %v, error = %v", matches, err)
	}
}

func TestWorktreeToolRequiresAction(t *testing.T) {
	tool := &WorktreeTool{Create: &CreateWorktreeTool{}, Manage: &ManageWorktreesTool{}}
	if _, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"branch": "missing-action"}), nil); err == nil {
		t.Fatal("missing action was accepted")
	}
}

// nestedWorktreeFixture creates feat/x and feat/x-api (based on feat/x)
// through the public tool and returns their paths.
func nestedWorktreeFixture(t *testing.T) (tool *WorktreeTool, repo, featPath, apiPath string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo = initWorktreeTestRepo(t)
	root := t.TempDir()
	tool = &WorktreeTool{
		Create: &CreateWorktreeTool{CWD: repo, Sandbox: NewSandbox(repo), DefaultRoot: root},
		Manage: &ManageWorktreesTool{CWD: repo, Sandbox: NewSandbox(repo)},
	}
	ctx := context.Background()
	if _, err := tool.Execute(ctx, mustJSON(t, map[string]any{"action": "create", "branch": "feat/x"}), nil); err != nil {
		t.Fatal(err)
	}
	featPath = filepath.Join(root, testWorktreeRepoID(t, repo), "feat", "x")
	// Advance the feature branch so HEAD of the primary checkout differs.
	if err := os.WriteFile(filepath.Join(featPath, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestOutput(t, featPath, "add", "feature.txt")
	gitTestOutput(t, featPath, "commit", "-qm", "feature work")

	result, err := tool.Execute(ctx, mustJSON(t, map[string]any{"action": "create", "branch": "feat/x-api", "base": "feat/x"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	details := result.Details.(map[string]any)
	featHead := gitTestOutput(t, featPath, "rev-parse", "HEAD")
	if details["base"] != featHead || details["base_ref"] != "feat/x" {
		t.Fatalf("details = %#v, want base %s from feat/x", details, featHead)
	}
	if text := resultText(result); !strings.Contains(text, "base: feat/x ("+featHead+")") {
		t.Fatalf("result text missing input ref and sha:\n%s", text)
	}
	apiPath = filepath.Join(root, testWorktreeRepoID(t, repo), "feat", "x-api")
	if got := gitTestOutput(t, apiPath, "rev-parse", "HEAD"); got != featHead {
		t.Fatalf("sub-worktree HEAD = %s, want %s", got, featHead)
	}
	return tool, repo, featPath, apiPath
}

func commitInWorktree(t *testing.T, path, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(path, name), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestOutput(t, path, "add", name)
	gitTestOutput(t, path, "commit", "-qm", name)
}

func TestWorktreeToolRejectsInvalidBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	tool := &WorktreeTool{Create: &CreateWorktreeTool{CWD: repo, Sandbox: NewSandbox(repo), DefaultRoot: t.TempDir()}}
	for _, base := range []string{"no-such-branch", "--output=/tmp/x", "-h"} {
		_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"action": "create", "branch": "feat/bad", "base": base}), nil)
		if err == nil || !strings.HasPrefix(err.Error(), "worktree: ") || !strings.Contains(err.Error(), "invalid base") {
			t.Fatalf("base %q: error = %v, want worktree invalid base", base, err)
		}
	}
	if got := gitTestOutputAllowExit(t, repo, "show-ref", "--verify", "--quiet", "refs/heads/feat/bad"); got.exitCode != 1 {
		t.Fatalf("branch was created: %#v", got)
	}
}

func TestWorktreeToolBasePreviewDetectsMovedBase(t *testing.T) {
	tool, _, featPath, _ := nestedWorktreeFixture(t)
	args := mustJSON(t, map[string]any{"action": "create", "branch": "feat/x-ui", "base": "feat/x"})
	if _, err := tool.Preview(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	commitInWorktree(t, featPath, "moved.txt")
	_, err := tool.Execute(context.Background(), args, nil)
	if err == nil || !strings.Contains(err.Error(), "changed since preview") {
		t.Fatalf("error = %v, want stale preview rejection", err)
	}
}

func TestWorktreeToolCleanupIntoFeatureBranchAndDeleteBranch(t *testing.T) {
	tool, repo, featPath, apiPath := nestedWorktreeFixture(t)
	ctx := context.Background()
	commitInWorktree(t, apiPath, "api.txt")
	cleanup := map[string]any{"action": "cleanup", "paths": []string{apiPath}}

	// Unmerged: blocked for both the default target and the feature branch.
	for _, args := range []map[string]any{cleanup, {"action": "cleanup", "paths": []string{apiPath}, "into": "feat/x"}} {
		_, err := tool.Preview(ctx, mustJSON(t, args))
		if err == nil || !strings.Contains(err.Error(), "not merged") {
			t.Fatalf("args %v: error = %v, want unmerged rejection", args, err)
		}
	}
	gitTestOutput(t, featPath, "merge", "--no-edit", "feat/x-api")

	// Default HEAD (primary checkout) does not contain the merge.
	_, err := tool.Preview(ctx, mustJSON(t, cleanup))
	if err == nil || !strings.Contains(err.Error(), "not merged into the current branch") {
		t.Fatalf("error = %v, want current-branch rejection", err)
	}
	// list with into reports eligibility against the feature branch.
	listed, err := tool.Execute(ctx, mustJSON(t, map[string]any{"action": "list", "into": "feat/x"}), nil)
	if err != nil || !strings.Contains(resultText(listed), `clean and merged into "feat/x"; cleanup eligible`) {
		t.Fatalf("list = %v, %q", err, resultText(listed))
	}
	_, err = tool.Preview(ctx, mustJSON(t, map[string]any{"action": "cleanup", "paths": []string{apiPath}, "into": "-bad"}))
	if err == nil || !strings.Contains(err.Error(), "invalid into") {
		t.Fatalf("error = %v, want invalid into", err)
	}

	args := mustJSON(t, map[string]any{"action": "cleanup", "paths": []string{apiPath}, "into": "feat/x", "delete_branch": true})
	preview, err := tool.Preview(ctx, args)
	if err != nil || !strings.Contains(resultText(preview), "delete branches:\n- feat/x-api") {
		t.Fatalf("preview = %v, %q", err, resultText(preview))
	}
	result, err := tool.Execute(ctx, args, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(apiPath); !os.IsNotExist(err) {
		t.Fatalf("sub-worktree remains: %v", err)
	}
	if got := gitTestOutputAllowExit(t, repo, "show-ref", "--verify", "--quiet", "refs/heads/feat/x-api"); got.exitCode != 1 {
		t.Fatalf("branch remains: %#v", got)
	}
	if got := gitTestOutput(t, repo, "show-ref", "--verify", "refs/heads/feat/x"); got == "" {
		t.Fatal("feature branch was deleted")
	}
	if text := resultText(result); !strings.Contains(text, "deleted branches:\n- feat/x-api") {
		t.Fatalf("result text missing deleted branch:\n%s", text)
	}
	details := result.Details.(map[string]any)
	if deleted, _ := details["deleted_branches"].([]string); len(deleted) != 1 || deleted[0] != "feat/x-api" {
		t.Fatalf("details = %#v", details)
	}
}

func TestWorktreeToolCleanupWithoutDeleteBranchKeepsBranch(t *testing.T) {
	tool, repo, _, apiPath := nestedWorktreeFixture(t)
	args := mustJSON(t, map[string]any{"action": "cleanup", "paths": []string{apiPath}, "into": "feat/x"})
	if _, err := tool.Execute(context.Background(), args, nil); err != nil {
		t.Fatal(err)
	}
	if got := gitTestOutput(t, repo, "show-ref", "--verify", "refs/heads/feat/x-api"); got == "" {
		t.Fatal("branch removed without delete_branch")
	}
}

func TestWorktreeToolDeleteBranchRefusesMergeTarget(t *testing.T) {
	tool, repo, featPath, _ := nestedWorktreeFixture(t)
	args := mustJSON(t, map[string]any{"action": "cleanup", "paths": []string{featPath}, "into": "feat/x", "delete_branch": true})
	_, err := tool.Preview(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "merge target") {
		t.Fatalf("error = %v, want merge-target rejection", err)
	}
	if _, statErr := os.Stat(featPath); statErr != nil {
		t.Fatal(statErr)
	}
	_ = repo
}

func TestWorktreeToolDeleteBranchRejectsMovedBranchAfterPreview(t *testing.T) {
	tool, repo, featPath, apiPath := nestedWorktreeFixture(t)
	args := mustJSON(t, map[string]any{"action": "cleanup", "paths": []string{apiPath}, "into": "feat/x", "delete_branch": true})
	if _, err := tool.Preview(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	// A new commit after preview leaves the branch unmerged, so execution
	// must fail closed without removing the worktree or deleting the branch.
	commitInWorktree(t, apiPath, "late.txt")
	_, err := tool.Execute(context.Background(), args, nil)
	if err == nil || !strings.Contains(err.Error(), "not merged") {
		t.Fatalf("error = %v, want unmerged rejection", err)
	}
	if got := gitTestOutput(t, repo, "show-ref", "--verify", "refs/heads/feat/x-api"); got == "" {
		t.Fatal("branch was deleted")
	}
	if _, statErr := os.Stat(apiPath); statErr != nil {
		t.Fatalf("worktree was removed: %v", statErr)
	}
	_, _ = repo, featPath
}

func TestDeleteMergedBranchKeepsBranchThatMoved(t *testing.T) {
	_, repo, _, apiPath := nestedWorktreeFixture(t)
	inspected := gitTestOutput(t, apiPath, "rev-parse", "HEAD")
	commitInWorktree(t, apiPath, "moved.txt")
	gitTestOutput(t, repo, "worktree", "remove", apiPath)

	err := deleteMergedBranch(context.Background(), repo, "feat/x-api", inspected)
	if err == nil || !strings.Contains(err.Error(), "changed since inspection") {
		t.Fatalf("error = %v, want moved-branch rejection", err)
	}
	if got := gitTestOutput(t, repo, "show-ref", "--verify", "refs/heads/feat/x-api"); got == "" {
		t.Fatal("moved branch was deleted")
	}
	current := gitTestOutput(t, repo, "rev-parse", "refs/heads/feat/x-api")
	if err := deleteMergedBranch(context.Background(), repo, "feat/x-api", current); err != nil {
		t.Fatal(err)
	}
}

func TestWorktreeToolReportsBranchDeletionFailureAfterRemoval(t *testing.T) {
	tool, repo, _, apiPath := nestedWorktreeFixture(t)
	ctx := context.Background()
	args := mustJSON(t, map[string]any{"action": "cleanup", "paths": []string{apiPath}, "into": "feat/x", "delete_branch": true})
	// Lock the branch ref so update-ref cannot delete it after worktree removal.
	lock := filepath.Join(repo, ".git", "refs", "heads", "feat", "x-api.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := tool.Execute(ctx, args, nil)
	if err == nil || !strings.HasPrefix(err.Error(), "worktree: ") || !strings.Contains(err.Error(), `delete branch "feat/x-api"`) || !strings.Contains(err.Error(), "worktrees were removed") {
		t.Fatalf("error = %v, want clear branch-deletion failure", err)
	}
	if _, statErr := os.Stat(apiPath); !os.IsNotExist(statErr) {
		t.Fatalf("worktree remains: %v", statErr)
	}
}
