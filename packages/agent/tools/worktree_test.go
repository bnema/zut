package tools

import (
	"context"
	"os/exec"
	"path/filepath"
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
