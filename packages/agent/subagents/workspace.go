package subagents

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// WorkspaceCapture is durable output collected before an isolated workspace
// is removed. The patch is intentionally kept as bytes so the caller can
// write it atomically beside the agent result.
type WorkspaceCapture struct {
	Patch        []byte
	ChangedFiles []string
	Base         string
	Branch       string
}

// WorkspaceRequest describes one workspace allocation.
type WorkspaceRequest struct {
	Mode           WorkspaceMode
	RepositoryRoot string
	StateDir       string
	AgentID        string
	Base           string
	Capture        CaptureMode
	AllowedRoots   []string

	// ExistingPath is set only when resuming a detached worktree. A valid
	// path is reused so uncommitted edits survive a host restart;
	// invalid or out-of-scope paths are rejected rather than treated as a
	// request to remove arbitrary files.
	ExistingPath string
}

// WorkspaceHandle is the narrow seam used by the manager. SharedWorkspace
// and GitWorktreeWorkspace are the two supported implementations in v1.
type WorkspaceHandle interface {
	Dir() string
	RepositoryRoot() string
	Mode() WorkspaceMode
	Capture(context.Context) (WorkspaceCapture, error)
	Cleanup(context.Context) error
}

// SharedWorkspace leaves the host checkout untouched by the manager; a
// child may edit it when the caller intentionally chooses shared mode.
type SharedWorkspace struct {
	Root string
}

func (w SharedWorkspace) Prepare(context.Context, WorkspaceRequest) (WorkspaceHandle, error) {
	root := w.Root
	if root == "" {
		return nil, errors.New("subagents: shared workspace root is empty")
	}
	return &sharedWorkspaceHandle{root: root}, nil
}

type sharedWorkspaceHandle struct{ root string }

func (w *sharedWorkspaceHandle) Dir() string            { return w.root }
func (w *sharedWorkspaceHandle) RepositoryRoot() string { return w.root }
func (w *sharedWorkspaceHandle) Mode() WorkspaceMode    { return WorkspaceShared }
func (w *sharedWorkspaceHandle) Capture(context.Context) (WorkspaceCapture, error) {
	return WorkspaceCapture{}, nil
}
func (w *sharedWorkspaceHandle) Cleanup(context.Context) error { return nil }

// GitWorktreeWorkspace creates a detached temporary worktree from Base and
// captures its diff before cleanup. It never merges or changes the host
// checkout's branch.
type GitWorktreeWorkspace struct{}

func (GitWorktreeWorkspace) Prepare(ctx context.Context, req WorkspaceRequest) (WorkspaceHandle, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	root, err := filepath.Abs(req.RepositoryRoot)
	if err != nil {
		return nil, fmt.Errorf("subagents worktree root: %w", err)
	}
	if _, err := os.Stat(root); err != nil {
		return nil, fmt.Errorf("subagents worktree root: %w", err)
	}
	actualRoot, err := gitOutput(ctx, root, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("subagents worktree verify repository: %w", err)
	}
	actualRoot = filepath.Clean(actualRoot)
	if len(req.AllowedRoots) > 0 {
		allowed := false
		for _, root := range req.AllowedRoots {
			if pathWithin(actualRoot, root) {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, errors.New("subagents worktree repository is outside allowed roots")
		}
	}
	base := strings.TrimSpace(req.Base)
	if base == "" {
		base = "HEAD"
	}
	if _, err := gitOutput(ctx, actualRoot, "rev-parse", "--verify", base+"^{commit}"); err != nil {
		return nil, fmt.Errorf("subagents worktree verify base %q: %w", base, err)
	}
	stateDir := req.StateDir
	if stateDir == "" {
		return nil, errors.New("subagents: worktree state directory is empty")
	}
	worktreeDir := filepath.Join(stateDir, "worktree")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("subagents worktree state dir: %w", err)
	}
	reuse := false
	if existing := strings.TrimSpace(req.ExistingPath); existing != "" {
		var err error
		reuse, err = validateReusableWorktree(ctx, actualRoot, stateDir, worktreeDir, existing)
		if err != nil {
			return nil, err
		}
		if reuse {
			worktreeDir = filepath.Clean(existing)
		}
	}
	if !reuse {
		_ = os.RemoveAll(worktreeDir)
		_, _ = gitOutput(ctx, actualRoot, "worktree", "prune")
		if _, err := gitOutput(ctx, actualRoot, "worktree", "add", "--detach", worktreeDir, base); err != nil {
			_ = os.RemoveAll(worktreeDir)
			_, _ = gitOutput(ctx, actualRoot, "worktree", "prune")
			return nil, fmt.Errorf("subagents worktree create: %w", err)
		}
	}
	return &gitWorktreeHandle{
		root:    actualRoot,
		dir:     worktreeDir,
		base:    base,
		capture: req.Capture,
		agentID: req.AgentID,
	}, nil
}

func validateReusableWorktree(ctx context.Context, repositoryRoot, stateDir, expectedPath, existingPath string) (bool, error) {
	if filepath.Clean(existingPath) != filepath.Clean(expectedPath) {
		return false, errors.New("subagents worktree resume path is outside the agent state directory")
	}
	info, err := os.Stat(existingPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("subagents worktree resume path: %w", err)
	}
	if !info.IsDir() {
		return false, errors.New("subagents worktree resume path is not a directory")
	}
	stateCanonical, err := containmentPath(stateDir)
	if err != nil {
		return false, fmt.Errorf("subagents worktree resume state path: %w", err)
	}
	expectedCanonical, err := containmentPath(expectedPath)
	if err != nil {
		return false, fmt.Errorf("subagents worktree resume expected path: %w", err)
	}
	existingCanonical, err := containmentPath(existingPath)
	if err != nil {
		return false, fmt.Errorf("subagents worktree resume path: %w", err)
	}
	if existingCanonical != expectedCanonical || !pathWithin(existingCanonical, stateCanonical) {
		return false, errors.New("subagents worktree resume path escapes the agent state directory")
	}
	top, err := gitOutput(ctx, existingPath, "rev-parse", "--show-toplevel")
	if err != nil {
		return false, fmt.Errorf("subagents worktree resume verify checkout: %w", err)
	}
	topCanonical, err := containmentPath(top)
	if err != nil {
		return false, fmt.Errorf("subagents worktree resume checkout path: %w", err)
	}
	if topCanonical != existingCanonical {
		return false, errors.New("subagents worktree resume checkout path mismatch")
	}
	registered, err := gitOutput(ctx, repositoryRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("subagents worktree resume list: %w", err)
	}
	for _, line := range strings.Split(registered, "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		registeredPath, pathErr := containmentPath(strings.TrimPrefix(line, "worktree "))
		if pathErr == nil && registeredPath == existingCanonical {
			return true, nil
		}
	}
	return false, errors.New("subagents worktree resume checkout is not registered with Git")
}

func pathWithin(path, root string) bool {
	pathAbs, err := containmentPath(path)
	if err != nil {
		return false
	}
	rootAbs, err := containmentPath(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(rootAbs), filepath.Clean(pathAbs))
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

// containmentPath resolves the nearest existing ancestor so validation also
// works for durable paths whose final component has not been created yet.
func containmentPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	current := absolute
	var missing []string
	for {
		if _, statErr := os.Lstat(current); statErr == nil {
			evaluated, evalErr := filepath.EvalSymlinks(current)
			if evalErr != nil {
				return "", evalErr
			}
			for index := len(missing) - 1; index >= 0; index-- {
				evaluated = filepath.Join(evaluated, missing[index])
			}
			return filepath.Clean(evaluated), nil
		} else if !os.IsNotExist(statErr) {
			return "", statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return absolute, nil
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

type gitWorktreeHandle struct {
	root    string
	dir     string
	base    string
	capture CaptureMode
	agentID string
}

func (w *gitWorktreeHandle) Dir() string            { return w.dir }
func (w *gitWorktreeHandle) RepositoryRoot() string { return w.root }
func (w *gitWorktreeHandle) Mode() WorkspaceMode    { return WorkspaceWorktree }

func (w *gitWorktreeHandle) Capture(ctx context.Context) (WorkspaceCapture, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	statusBytes, err := gitOutputBytes(ctx, w.dir, "status", "--porcelain", "-z")
	if err != nil {
		return WorkspaceCapture{}, fmt.Errorf("subagents worktree status: %w", err)
	}
	changed := changedFilesFromStatus(statusBytes)
	args := []string{"diff"}
	if w.capture == CapturePatch || w.capture == "" {
		args = append(args, "--binary")
	}
	args = append(args, w.base, "--")
	patch, err := gitOutputBytes(ctx, w.dir, args...)
	if err != nil {
		return WorkspaceCapture{}, fmt.Errorf("subagents worktree diff: %w", err)
	}

	// `git diff <base>` does not include untracked files. Add a normal
	// no-index creation patch for each untracked file so a worktree result can
	// reproduce new files as well as edits to tracked files.
	untracked, err := gitOutputBytes(ctx, w.dir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return WorkspaceCapture{}, fmt.Errorf("subagents worktree untracked files: %w", err)
	}
	for _, name := range splitNUL(untracked) {
		extra, diffErr := gitUntrackedDiff(ctx, w.dir, name, w.capture == CapturePatch || w.capture == "")
		if diffErr != nil {
			return WorkspaceCapture{}, fmt.Errorf("subagents worktree untracked diff %q: %w", name, diffErr)
		}
		if len(extra) == 0 {
			continue
		}
		if len(patch) > 0 && patch[len(patch)-1] != '\n' {
			patch = append(patch, '\n')
		}
		patch = append(patch, extra...)
	}
	return WorkspaceCapture{Patch: patch, ChangedFiles: changed, Base: w.base}, nil
}

func (w *gitWorktreeHandle) Cleanup(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if w.dir == "" {
		return nil
	}
	_, err := gitOutput(ctx, w.root, "worktree", "remove", "--force", w.dir)
	if err != nil {
		// A failed child can leave a partially registered worktree. Remove
		// only the isolated path; never delete files in the host checkout,
		// then prune stale registration metadata.
		_ = os.RemoveAll(w.dir)
		_, _ = gitOutput(ctx, w.root, "worktree", "prune")
		return fmt.Errorf("subagents worktree cleanup: %w", err)
	}
	return nil
}

func PrepareWorkspace(ctx context.Context, req WorkspaceRequest) (WorkspaceHandle, error) {
	mode := req.Mode
	if mode == "" {
		mode = WorkspaceShared
	}
	switch mode {
	case WorkspaceShared:
		return SharedWorkspace{Root: req.RepositoryRoot}.Prepare(ctx, req)
	case WorkspaceWorktree:
		return GitWorktreeWorkspace{}.Prepare(ctx, req)
	default:
		return nil, fmt.Errorf("subagents: unknown workspace mode %q", mode)
	}
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	b, err := gitOutputBytes(ctx, dir, args...)
	return strings.TrimSpace(string(b)), err
}

func gitOutputBytes(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, errors.New(message)
	}
	return stdout.Bytes(), nil
}

func gitUntrackedDiff(ctx context.Context, dir, name string, binary bool) ([]byte, error) {
	args := []string{"-C", dir, "diff", "--no-index"}
	if binary {
		args = append(args, "--binary")
	}
	args = append(args, os.DevNull, "--", name)
	cmd := exec.CommandContext(ctx, "git", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return stdout.Bytes(), nil
	}
	message := strings.TrimSpace(stderr.String())
	if message == "" {
		message = err.Error()
	}
	return nil, errors.New(message)
}

func splitNUL(data []byte) []string {
	parts := strings.Split(string(data), "\x00")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func changedFilesFromStatus(status []byte) []string {
	var files []string
	for i := 0; i < len(status); {
		end := bytes.IndexByte(status[i:], 0)
		if end < 0 {
			end = len(status) - i
		}
		record := status[i : i+end]
		i += end + 1
		if len(record) < 4 {
			continue
		}
		// With `git status --porcelain -z`, rename and copy records contain
		// the new path in the status record, followed by the old path as the
		// next NUL-delimited field.
		path := record[3:]
		if len(path) == 0 {
			continue
		}
		files = append(files, string(path))
		isRename := record[0] == 'R' || record[0] == 'C' || record[1] == 'R' || record[1] == 'C'
		if isRename {
			if i < len(status) {
				if end := bytes.IndexByte(status[i:], 0); end >= 0 {
					i += end + 1
				} else {
					i = len(status)
				}
			}
		}
	}
	return files
}
