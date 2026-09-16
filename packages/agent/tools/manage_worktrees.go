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
	CWD     string
	Sandbox *Sandbox

	previewMu sync.Mutex
	preview   *manageWorktreesPreview
}

type manageWorktreesArgs struct {
	Action string   `json:"action"`
	Paths  []string `json:"paths,omitempty"`
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
	selected []managedWorktree
	all      []managedWorktree
	text     string
}

type manageWorktreesPreview struct {
	raw  string
	plan manageWorktreesPlan
}

const manageWorktreesSchema = `{
  "type":"object",
  "properties":{
    "action":{"type":"string","enum":["list","cleanup"],"description":"List repository worktrees, or remove explicitly selected worktrees that are clean and merged."},
    "paths":{"type":"array","items":{"type":"string"},"description":"Exact worktree paths to remove. Required for cleanup."}
  },
  "required":["action"]
}`

func (t *ManageWorktreesTool) Name() string { return "manage_worktrees" }
func (t *ManageWorktreesTool) Description() string {
	return "List repository worktrees and safely remove explicitly selected clean, merged worktrees."
}
func (t *ManageWorktreesTool) Schema() json.RawMessage { return json.RawMessage(manageWorktreesSchema) }

func (t *ManageWorktreesTool) Preview(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
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
	for _, worktree := range plan.selected {
		if progress != nil {
			progress("Removing worktree " + filepath.ToSlash(worktree.Path) + "...\n")
		}
		if _, err := manageWorktreesGit(ctx, plan.repoRoot, "worktree", "remove", worktree.Path); err != nil {
			return core.ToolResult{}, fmt.Errorf("manage_worktrees: remove %q: %w", filepath.ToSlash(worktree.Path), err)
		}
	}
	_, _ = manageWorktreesGit(ctx, plan.repoRoot, "worktree", "prune")
	return manageWorktreesResult(plan, "removed"), nil
}

func (t *ManageWorktreesTool) plan(ctx context.Context, raw json.RawMessage) (manageWorktreesPlan, error) {
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
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return manageWorktreesPlan{}, fmt.Errorf("manage_worktrees: working directory: %w", err)
		}
	}
	if err := t.Sandbox.CheckReadPath(cwd); err != nil {
		return manageWorktreesPlan{}, fmt.Errorf("manage_worktrees: %w", err)
	}
	repoRoot, err := manageWorktreesGit(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return manageWorktreesPlan{}, fmt.Errorf("manage_worktrees: verify repository: %w", err)
	}
	repoRoot, err = filepath.Abs(repoRoot)
	if err != nil {
		return manageWorktreesPlan{}, err
	}
	if err := t.Sandbox.CheckReadPath(repoRoot); err != nil {
		return manageWorktreesPlan{}, fmt.Errorf("manage_worktrees: %w", err)
	}
	items, err := inspectManagedWorktrees(ctx, repoRoot)
	if err != nil {
		return manageWorktreesPlan{}, err
	}
	plan := manageWorktreesPlan{repoRoot: repoRoot, action: args.Action, all: items}
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

func inspectManagedWorktrees(ctx context.Context, repoRoot string) ([]managedWorktree, error) {
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
			} else if _, mergeErr := manageWorktreesGit(ctx, repoRoot, "merge-base", "--is-ancestor", item.Head, "HEAD"); mergeErr != nil {
				item.BlockedBy = "branch is not merged into the current branch: " + firstErrorLine(mergeErr)
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
	if a.repoRoot != b.repoRoot || a.action != b.action || a.text != b.text || len(a.selected) != len(b.selected) || len(a.all) != len(b.all) {
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
