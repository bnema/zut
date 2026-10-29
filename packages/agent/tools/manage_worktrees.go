package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// ManageWorktreesTool inventories repository worktrees and removes only clean,
// merged, explicitly selected secondary worktrees.
type ManageWorktreesTool struct {
	CWD string
	// Env is the explicit session environment; nil is invalid.
	Env     []string
	Sandbox *Sandbox

	previewMu sync.Mutex
	preview   *manageWorktreesPreview
}

type manageWorktreesArgs struct {
	Action       string   `json:"action"`
	Paths        []string `json:"paths,omitempty"`
	Into         string   `json:"into,omitempty"`
	DeleteBranch bool     `json:"delete_branch,omitempty"`
}

type managedWorktree struct {
	Path      string `json:"path"`
	Branch    string `json:"branch,omitempty"`
	Head      string `json:"head,omitempty"`
	Primary   bool   `json:"primary"`
	Dirty     bool   `json:"dirty"`
	Prunable  bool   `json:"prunable"`
	Merged    bool   `json:"merged"`
	CleanupOK bool   `json:"cleanup_eligible"`
	BlockedBy string `json:"blocked_by,omitempty"`
}

type manageWorktreesPlan struct {
	repoRoot string
	action   string
	// into is the requested merge target; empty means the current HEAD.
	into         string
	deleteBranch bool
	// intoCommit and intoRef pin the resolved merge target so a target that
	// moves between preview and execute is detected. intoRef is the symbolic
	// full name (empty for a commit or detached HEAD).
	intoCommit string
	intoRef    string
	selected   []managedWorktree
	all        []managedWorktree
	text       string
}

type manageWorktreesPreview struct {
	raw  string
	plan manageWorktreesPlan
}

const manageWorktreesSchema = `{
  "type":"object",
  "properties":{
    "action":{"type":"string","enum":["list","cleanup"],"description":"List repository worktrees, or remove explicitly selected worktrees that are clean and merged."},
    "paths":{"type":"array","items":{"type":"string"},"description":"Exact worktree paths to remove. Required for cleanup."},
    "into":{"type":"string","description":"Branch, tag, or commit the worktree branches must be merged into. Defaults to the current HEAD."},
    "delete_branch":{"type":"boolean","description":"Also delete each removed worktree's local branch if it is still at the inspected commit."}
  },
  "required":["action"]
}`

func (t *ManageWorktreesTool) Name() string { return "manage_worktrees" }
func (t *ManageWorktreesTool) Description() string {
	return "List repository worktrees and safely remove explicitly selected clean, merged worktrees."
}
func (t *ManageWorktreesTool) Schema() json.RawMessage { return json.RawMessage(manageWorktreesSchema) }

func (t *ManageWorktreesTool) Preview(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	if t.CWD == "" {
		return core.ToolResult{}, fmt.Errorf("tool manage_worktrees: session cwd not set")
	}
	if t.Env == nil {
		return core.ToolResult{}, fmt.Errorf("tool manage_worktrees: session env not set")
	}
	ctx = withToolEnv(ctx, t.Env)
	plan, err := t.plan(ctx, raw)
	if err != nil {
		return core.ToolResult{}, err
	}
	t.previewMu.Lock()
	t.preview = &manageWorktreesPreview{raw: string(raw), plan: plan}
	t.previewMu.Unlock()
	return manageWorktreesResult(plan, "preview"), nil
}

func (t *ManageWorktreesTool) Execute(ctx context.Context, raw json.RawMessage, progress func(string)) (core.ToolResult, error) {
	if t.CWD == "" {
		return core.ToolResult{}, fmt.Errorf("tool manage_worktrees: session cwd not set")
	}
	if t.Env == nil {
		return core.ToolResult{}, fmt.Errorf("tool manage_worktrees: session env not set")
	}
	ctx = withToolEnv(ctx, t.Env)
	plan, err := t.plan(ctx, raw)
	if err != nil {
		return core.ToolResult{}, err
	}
	t.previewMu.Lock()
	cached := t.preview
	t.preview = nil
	t.previewMu.Unlock()
	if cached != nil && cached.raw == string(raw) && !sameManageWorktreesPlan(cached.plan, plan) {
		return core.ToolResult{}, errors.New("manage_worktrees: repository state changed since preview; inspect and retry")
	}
	if plan.action == "list" {
		return manageWorktreesResult(plan, "listed"), nil
	}
	var removed, deleted []string
	var branchErrs []string
	// summary reports progress made so far; it is attached to every failure
	// return so a partial cleanup is never silently dropped.
	summary := func() string {
		var parts []string
		if len(branchErrs) > 0 {
			parts = append(parts, strings.Join(branchErrs, "; "))
		}
		if len(removed) > 0 {
			parts = append(parts, "removed worktrees: "+strings.Join(removed, ", "))
		}
		if len(deleted) > 0 {
			parts = append(parts, "deleted branches: "+strings.Join(deleted, ", "))
		}
		if len(parts) == 0 {
			return ""
		}
		return "; " + strings.Join(parts, "; ")
	}
	for _, worktree := range plan.selected {
		if progress != nil {
			progress("Removing worktree " + filepath.ToSlash(worktree.Path) + "...\n")
		}
		if _, err := manageWorktreesGit(ctx, plan.repoRoot, "worktree", "remove", worktree.Path); err != nil {
			_, _ = manageWorktreesGit(ctx, plan.repoRoot, "worktree", "prune")
			return core.ToolResult{}, fmt.Errorf("manage_worktrees: remove %q: %w%s", filepath.ToSlash(worktree.Path), err, summary())
		}
		removed = append(removed, filepath.ToSlash(worktree.Path))
		if !plan.deleteBranch {
			continue
		}
		if progress != nil {
			progress("Deleting branch " + worktree.Branch + "...\n")
		}
		if err := deleteMergedBranch(ctx, plan.repoRoot, worktree.Branch, worktree.Head); err != nil {
			branchErrs = append(branchErrs, fmt.Sprintf("delete branch %q: %v", worktree.Branch, err))
			continue
		}
		deleted = append(deleted, worktree.Branch)
	}
	_, _ = manageWorktreesGit(ctx, plan.repoRoot, "worktree", "prune")
	if len(branchErrs) > 0 {
		return core.ToolResult{}, fmt.Errorf("manage_worktrees: worktrees were removed but some branches were kept%s", summary())
	}
	result := manageWorktreesResult(plan, "removed")
	if len(deleted) > 0 {
		text := plan.text + "deleted branches:\n"
		for _, branch := range deleted {
			text += "- " + branch + "\n"
		}
		result.Content = []provider.Content{provider.TextBlock{Text: text}}
	}
	if details, ok := result.Details.(map[string]any); ok {
		details["removed"] = removed
		details["deleted_branches"] = deleted
	}
	return result, nil
}

// deleteMergedBranch deletes refs/heads/<branch> only while it still points at
// head, is a plain (non-symbolic) ref, and is not checked out in any worktree.
// The checks run immediately before deletion because state can change after
// planning and after the worktree was removed.
func deleteMergedBranch(ctx context.Context, repoRoot, branch, head string) error {
	if branch == "" || head == "" {
		return errors.New("branch or inspected commit is unknown")
	}
	ref := "refs/heads/" + branch
	// A symbolic branch ref would make update-ref delete its target.
	if _, err := manageWorktreesGit(ctx, repoRoot, "symbolic-ref", "-q", ref); err == nil {
		return errors.New("branch ref is symbolic and was kept")
	} else if createWorktreeGitExitCode(err) != 1 {
		return fmt.Errorf("inspect branch ref: %w", err)
	}
	where, err := branchCheckedOutAt(ctx, repoRoot, ref)
	if err != nil {
		return err
	}
	if where != "" {
		return fmt.Errorf("branch is checked out in %q", filepath.ToSlash(where))
	}
	if _, err := manageWorktreesGit(ctx, repoRoot, "update-ref", "--no-deref", "-d", ref, head); err != nil {
		return fmt.Errorf("branch changed since inspection or cannot be deleted: %w", err)
	}
	return nil
}

// branchCheckedOutAt returns the path of a worktree (primary included) that has
// ref checked out, or "" when none does.
func branchCheckedOutAt(ctx context.Context, repoRoot, ref string) (string, error) {
	output, err := manageWorktreesGit(ctx, repoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("list worktrees: %w", err)
	}
	for _, block := range strings.Split(strings.TrimSpace(output), "\n\n") {
		path, checkedOut := "", false
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "worktree ") {
				path = strings.TrimPrefix(line, "worktree ")
			} else if line == "branch "+ref {
				checkedOut = true
			}
		}
		if checkedOut {
			return path, nil
		}
	}
	return "", nil
}

func (t *ManageWorktreesTool) plan(ctx context.Context, raw json.RawMessage) (manageWorktreesPlan, error) {
	if t.Env == nil {
		return manageWorktreesPlan{}, fmt.Errorf("tool manage_worktrees: session env not set")
	}
	ctx = withToolEnv(ctx, t.Env)
	var args manageWorktreesArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return manageWorktreesPlan{}, fmt.Errorf("invalid args: %w", err)
	}
	if args.Action != "list" && args.Action != "cleanup" {
		return manageWorktreesPlan{}, errors.New("manage_worktrees: action must be list or cleanup")
	}
	if t.Sandbox == nil {
		return manageWorktreesPlan{}, errors.New("manage_worktrees: sandbox is required")
	}
	if err := t.Sandbox.CheckBashPermission("git"); err != nil {
		return manageWorktreesPlan{}, fmt.Errorf("manage_worktrees: %w", err)
	}
	cwd := t.CWD
	if cwd == "" {
		return manageWorktreesPlan{}, fmt.Errorf("tool manage_worktrees: session cwd not set")
	}
	if err := t.Sandbox.CheckReadPath(cwd); err != nil {
		return manageWorktreesPlan{}, fmt.Errorf("manage_worktrees: %w", err)
	}
	repoRoot, err := manageWorktreesGit(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return manageWorktreesPlan{}, fmt.Errorf("manage_worktrees: verify repository: %w", err)
	}
	repoRoot = filepath.Clean(resolvePath(cwd, repoRoot))
	if err := t.Sandbox.CheckReadPath(repoRoot); err != nil {
		return manageWorktreesPlan{}, fmt.Errorf("manage_worktrees: %w", err)
	}
	if args.Action == "list" && args.DeleteBranch {
		return manageWorktreesPlan{}, errors.New("manage_worktrees: delete_branch applies only to cleanup")
	}
	into := strings.TrimSpace(args.Into)
	if strings.HasPrefix(into, "-") {
		return manageWorktreesPlan{}, fmt.Errorf("manage_worktrees: invalid into %q", into)
	}
	target := into
	if target == "" {
		target = "HEAD"
	}
	intoCommit, err := manageWorktreesGit(ctx, repoRoot, "rev-parse", "--verify", target+"^{commit}")
	if err != nil {
		if into == "" {
			return manageWorktreesPlan{}, fmt.Errorf("manage_worktrees: resolve current commit: %w", err)
		}
		return manageWorktreesPlan{}, fmt.Errorf("manage_worktrees: invalid into %q: %w", into, err)
	}
	intoRef, _ := manageWorktreesGit(ctx, repoRoot, "rev-parse", "--symbolic-full-name", target)
	items, err := inspectManagedWorktrees(ctx, repoRoot, intoCommit, into)
	if err != nil {
		return manageWorktreesPlan{}, err
	}
	plan := manageWorktreesPlan{repoRoot: repoRoot, action: args.Action, into: into, intoCommit: intoCommit, intoRef: intoRef, deleteBranch: args.DeleteBranch, all: items}
	if args.Action == "cleanup" {
		if len(args.Paths) == 0 {
			return manageWorktreesPlan{}, errors.New("manage_worktrees: cleanup requires at least one explicit path")
		}
		byPath := make(map[string]managedWorktree, len(items))
		for _, item := range items {
			canonicalPath, err := canonicalOrParent(item.Path)
			if err != nil {
				return manageWorktreesPlan{}, fmt.Errorf("manage_worktrees: resolve registered path: %w", err)
			}
			byPath[canonicalPath] = item
		}
		seen := map[string]bool{}
		for _, requested := range args.Paths {
			if !filepath.IsAbs(requested) {
				requested = filepath.Join(cwd, requested)
			}
			path, err := canonicalOrParent(requested)
			if err != nil {
				return manageWorktreesPlan{}, fmt.Errorf("manage_worktrees: resolve path: %w", err)
			}
			item, ok := byPath[path]
			if !ok {
				return manageWorktreesPlan{}, fmt.Errorf("manage_worktrees: %q is not a registered worktree", filepath.ToSlash(path))
			}
			if !item.CleanupOK {
				return manageWorktreesPlan{}, fmt.Errorf("manage_worktrees: %q cannot be removed: %s", filepath.ToSlash(path), item.BlockedBy)
			}
			if err := t.Sandbox.CheckWritePath(item.Path); err != nil {
				return manageWorktreesPlan{}, fmt.Errorf("manage_worktrees: %w", err)
			}
			if !seen[item.Path] {
				plan.selected = append(plan.selected, item)
				seen[item.Path] = true
			}
		}
		if args.DeleteBranch {
			// Deleting a branch writes to Git ref storage, which lives outside the
			// worktree path. Require write access to it and fail closed. Jail mode
			// is a guardrail, not a sandbox: it cannot stop git from touching refs
			// once permitted, so this only enforces the declared scopes.
			commonDir, err := manageWorktreesGit(ctx, repoRoot, "rev-parse", "--git-common-dir")
			if err != nil {
				return manageWorktreesPlan{}, fmt.Errorf("manage_worktrees: resolve git directory: %w", err)
			}
			if !filepath.IsAbs(commonDir) {
				commonDir = filepath.Join(repoRoot, commonDir)
			}
			commonDir, err = canonical(commonDir)
			if err != nil {
				return manageWorktreesPlan{}, fmt.Errorf("manage_worktrees: resolve git directory: %w", err)
			}
			if err := t.Sandbox.CheckWritePath(commonDir); err != nil {
				return manageWorktreesPlan{}, fmt.Errorf("manage_worktrees: %w", err)
			}
			for _, item := range plan.selected {
				if intoRef == "refs/heads/"+item.Branch {
					return manageWorktreesPlan{}, fmt.Errorf("manage_worktrees: branch %q is the merge target and cannot be deleted", item.Branch)
				}
				for _, other := range items {
					if other.Branch == item.Branch && !seen[other.Path] {
						return manageWorktreesPlan{}, fmt.Errorf("manage_worktrees: branch %q is checked out in %q and cannot be deleted", item.Branch, filepath.ToSlash(other.Path))
					}
				}
			}
		}
	}
	plan.text = renderManagedWorktrees(plan)
	return plan, nil
}

// WorktreeInventoryContext returns a model-readable repository inventory. A
// non-Git working directory has no inventory and is not an error.
func WorktreeInventoryContext(ctx context.Context, cwd string) string {
	root, err := manageWorktreesGit(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	output, err := manageWorktreesGit(ctx, root, "worktree", "list", "--porcelain")
	if err != nil {
		return ""
	}
	const maxInventoryItems = 20
	var b strings.Builder
	b.WriteString("[Repository worktrees]\n")
	count := 0
	for _, block := range strings.Split(strings.TrimSpace(output), "\n\n") {
		if count == maxInventoryItems {
			break
		}
		path, branch := "", ""
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "worktree ") {
				path = strings.TrimPrefix(line, "worktree ")
			}
			if strings.HasPrefix(line, "branch refs/heads/") {
				branch = strings.TrimPrefix(line, "branch refs/heads/")
			}
		}
		if path == "" {
			continue
		}
		fmt.Fprintf(&b, "- %s", filepath.ToSlash(path))
		if branch != "" {
			fmt.Fprintf(&b, " [%s]", branch)
		}
		b.WriteByte('\n')
		count++
	}
	total := len(strings.Split(strings.TrimSpace(output), "\n\n"))
	if total <= 1 {
		return ""
	}
	if total > count {
		fmt.Fprintf(&b, "- ... %d more worktrees\n", total-count)
	}
	b.WriteString("Use worktree action=list for current safety status before cleanup.")
	return b.String()
}

// inspectManagedWorktrees reports each worktree's state. A branch is merged
// when its head is an ancestor of intoCommit; into names that target for
// messages and is empty for the current HEAD.
func inspectManagedWorktrees(ctx context.Context, repoRoot, intoCommit, into string) ([]managedWorktree, error) {
	output, err := manageWorktreesGit(ctx, repoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("manage_worktrees: list: %w", err)
	}
	blocks := strings.Split(strings.TrimSpace(output), "\n\n")
	items := make([]managedWorktree, 0, len(blocks))
	for index, block := range blocks {
		item := managedWorktree{Primary: index == 0}
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "worktree "):
				item.Path = strings.TrimPrefix(line, "worktree ")
			case strings.HasPrefix(line, "HEAD "):
				item.Head = strings.TrimPrefix(line, "HEAD ")
			case strings.HasPrefix(line, "branch refs/heads/"):
				item.Branch = strings.TrimPrefix(line, "branch refs/heads/")
			case strings.HasPrefix(line, "prunable"):
				item.Prunable = true
			}
		}
		if item.Path == "" {
			continue
		}
		if item.Primary {
			item.BlockedBy = "primary worktree"
		} else if item.Prunable {
			item.BlockedBy = "missing worktree; prune metadata instead"
		} else if item.Branch == "" {
			item.BlockedBy = "detached HEAD"
		} else if _, submoduleErr := os.Stat(filepath.Join(item.Path, ".gitmodules")); submoduleErr == nil {
			item.BlockedBy = "worktree contains submodules"
		} else if !os.IsNotExist(submoduleErr) {
			item.BlockedBy = "cannot inspect submodules: " + firstErrorLine(submoduleErr)
		} else {
			status, statusErr := manageWorktreesGit(ctx, item.Path, "status", "--porcelain", "--ignored=matching")
			if statusErr != nil {
				item.BlockedBy = "status unavailable: " + firstErrorLine(statusErr)
			} else if status != "" {
				item.Dirty = true
				item.BlockedBy = "uncommitted changes"
			} else if _, mergeErr := manageWorktreesGit(ctx, repoRoot, "merge-base", "--is-ancestor", item.Head, intoCommit); mergeErr != nil {
				target := "the current branch"
				if into != "" {
					target = fmt.Sprintf("%q", into)
				}
				item.BlockedBy = "branch is not merged into " + target
				if createWorktreeGitExitCode(mergeErr) != 1 {
					item.BlockedBy += ": " + firstErrorLine(mergeErr)
				}
			} else {
				item.Merged = true
				item.CleanupOK = true
			}
		}
		items = append(items, item)
	}
	return items, nil
}

func renderManagedWorktrees(plan manageWorktreesPlan) string {
	var b strings.Builder
	if plan.action == "cleanup" {
		b.WriteString("remove worktrees\n")
	} else {
		b.WriteString("repository worktrees\n")
	}
	for _, item := range plan.all {
		fmt.Fprintf(&b, "- %s", filepath.ToSlash(item.Path))
		if item.Branch != "" {
			fmt.Fprintf(&b, " [%s]", item.Branch)
		}
		state := item.BlockedBy
		if item.CleanupOK {
			state = "clean and merged; cleanup eligible"
			if plan.into != "" {
				state = fmt.Sprintf("clean and merged into %q; cleanup eligible", plan.into)
			}
		}
		if state != "" {
			fmt.Fprintf(&b, ": %s", state)
		}
		b.WriteByte('\n')
	}
	if plan.action == "cleanup" {
		b.WriteString("selected:\n")
		for _, item := range plan.selected {
			fmt.Fprintf(&b, "- %s\n", filepath.ToSlash(item.Path))
		}
		if plan.deleteBranch {
			b.WriteString("delete branches:\n")
			for _, item := range plan.selected {
				fmt.Fprintf(&b, "- %s\n", item.Branch)
			}
		}
	}
	return b.String()
}

func manageWorktreesResult(plan manageWorktreesPlan, state string) core.ToolResult {
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: plan.text}},
		Details: map[string]any{"state": state, "worktrees": plan.all},
	}
}

func sameManageWorktreesPlan(a, b manageWorktreesPlan) bool {
	if a.repoRoot != b.repoRoot || a.action != b.action || a.into != b.into || a.intoCommit != b.intoCommit || a.intoRef != b.intoRef || a.deleteBranch != b.deleteBranch || a.text != b.text || len(a.selected) != len(b.selected) || len(a.all) != len(b.all) {
		return false
	}
	for index := range a.selected {
		if a.selected[index] != b.selected[index] {
			return false
		}
	}
	for index := range a.all {
		if a.all[index] != b.all[index] {
			return false
		}
	}
	return true
}

func firstErrorLine(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	return line
}

func manageWorktreesGit(ctx context.Context, dir string, args ...string) (string, error) {
	return createWorktreeGitOutput(ctx, dir, args...)
}
