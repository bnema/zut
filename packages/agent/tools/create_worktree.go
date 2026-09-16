package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

const (
	worktreeConfigKey          = "zut.worktrees.path"
	maxCreateWorktreeGitOutput = 64 * 1024
	// worktreeRepoIDHashLength keeps the hash short enough to read but wide
	// enough that distinct repositories do not collide in practice.
	worktreeRepoIDHashLength = 12
	worktreeRepoSlugMaxLen   = 40
)

var createWorktreeOperationMu sync.Mutex

// CreateWorktreeTool creates a persistent branch checkout. An unconfigured
// repository checks out under a stable per-repository directory beneath
// DefaultRoot. A local zut.worktrees.path Git configuration entry overrides
// that root for the repository.
type CreateWorktreeTool struct {
	CWD         string
	Sandbox     *Sandbox
	DefaultRoot string

	previewMu sync.Mutex
	preview   *createWorktreePreview
}

type createWorktreeArgs struct {
	Branch string `json:"branch"`
}

const createWorktreeSchema = `{
  "type":"object",
  "properties":{
    "branch":{
      "type":"string",
      "description":"Name of the new Git branch."
    }
  },
  "required":["branch"]
}`

func (t *CreateWorktreeTool) Name() string { return "create_worktree" }
func (t *CreateWorktreeTool) Description() string {
	return "Create a persistent Git branch worktree under the repository's configured worktree root or zut's global worktrees root."
}
func (t *CreateWorktreeTool) Schema() json.RawMessage { return json.RawMessage(createWorktreeSchema) }

// Preview reports the checkout that Execute will produce without modifying the
// repository or filesystem.
func (t *CreateWorktreeTool) Preview(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	plan, err := t.plan(ctx, raw)
	if err != nil {
		return core.ToolResult{}, err
	}
	t.previewMu.Lock()
	t.preview = &createWorktreePreview{raw: string(raw), plan: plan}
	t.previewMu.Unlock()
	return plan.preview, nil
}

func (t *CreateWorktreeTool) Execute(ctx context.Context, raw json.RawMessage, progress func(string)) (core.ToolResult, error) {
	createWorktreeOperationMu.Lock()
	defer createWorktreeOperationMu.Unlock()

	plan, err := t.planForExecute(ctx, raw)
	if err != nil {
		return core.ToolResult{}, err
	}
	return executeCreateWorktreePlan(ctx, plan, progress)
}

func executeCreateWorktreePlan(ctx context.Context, plan createWorktreePlan, progress func(string)) (core.ToolResult, error) {
	createdWorktreeParents, err := missingWorktreeParents(plan.worktreePath)
	if err != nil {
		return core.ToolResult{}, fmt.Errorf("create_worktree: inspect worktree directories: %w", err)
	}
	rollback := func(cause error) error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := plan.cleanupFailedCheckout(cleanupCtx); err != nil {
			cause = fmt.Errorf("%w; cleanup worktree: %v", cause, err)
		}
		if err := removeCreatedWorktreeParents(createdWorktreeParents); err != nil {
			cause = fmt.Errorf("%w; remove created worktree directories: %v", cause, err)
		}
		return cause
	}

	if err := os.MkdirAll(filepath.Dir(plan.worktreePath), 0o755); err != nil {
		return core.ToolResult{}, rollback(fmt.Errorf("create_worktree: make worktree directory: %w", err))
	}
	if progress != nil {
		progress("Creating Git worktree...\n")
	}
	if _, err := createWorktreeGitOutput(ctx, plan.repoRoot, "worktree", "add", "-b", plan.branch, plan.worktreePath, plan.base); err != nil {
		return core.ToolResult{}, rollback(fmt.Errorf("create_worktree: create worktree: %w", err))
	}

	result := plan.preview
	previewDetails, _ := plan.preview.Details.(map[string]any)
	details := make(map[string]any, len(previewDetails)+1)
	for key, value := range previewDetails {
		details[key] = value
	}
	details["state"] = "created"
	result.Details = details
	result.Content = []provider.Content{provider.TextBlock{Text: strings.Replace(plan.previewText, "create worktree", "created worktree", 1)}}
	return result, nil
}

type createWorktreePreview struct {
	raw  string
	plan createWorktreePlan
}

func (t *CreateWorktreeTool) planForExecute(ctx context.Context, raw json.RawMessage) (createWorktreePlan, error) {
	t.previewMu.Lock()
	cached := t.preview
	t.preview = nil
	t.previewMu.Unlock()

	plan, err := t.plan(ctx, raw)
	if err != nil {
		return createWorktreePlan{}, err
	}
	if cached != nil && cached.raw == string(raw) && !sameCreateWorktreePlan(cached.plan, plan) {
		return createWorktreePlan{}, errors.New("create_worktree: repository state changed since preview; inspect and retry")
	}
	return plan, nil
}

type createWorktreePlan struct {
	repoRoot     string
	branch       string
	base         string
	worktreeRoot string
	worktreePath string
	rootSource   string
	repoID       string
	previewText  string
	preview      core.ToolResult
}

func (t *CreateWorktreeTool) plan(ctx context.Context, raw json.RawMessage) (createWorktreePlan, error) {
	var args createWorktreeArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return createWorktreePlan{}, fmt.Errorf("invalid args: %w", err)
	}
	branchInput := strings.TrimSpace(args.Branch)
	if branchInput == "" {
		return createWorktreePlan{}, errors.New("branch is required")
	}
	if strings.HasPrefix(branchInput, "-") || strings.Contains(branchInput, "@{") {
		return createWorktreePlan{}, fmt.Errorf("create_worktree: invalid branch %q", branchInput)
	}
	if t.Sandbox == nil {
		return createWorktreePlan{}, errors.New("create_worktree: sandbox is required")
	}
	if err := t.Sandbox.CheckBashPermission("git"); err != nil {
		return createWorktreePlan{}, fmt.Errorf("create_worktree: %w", err)
	}

	cwd, err := t.accessibleCWD()
	if err != nil {
		return createWorktreePlan{}, err
	}
	candidateRoot, err := t.findGitRootCandidate(cwd)
	if err != nil {
		return createWorktreePlan{}, err
	}
	repoRoot, err := createWorktreeGitOutput(ctx, candidateRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return createWorktreePlan{}, fmt.Errorf("create_worktree: verify repository: %w", err)
	}
	repoRoot, err = canonical(repoRoot)
	if err != nil {
		return createWorktreePlan{}, fmt.Errorf("create_worktree: resolve repository root: %w", err)
	}
	if err := t.checkReadAccess(repoRoot); err != nil {
		return createWorktreePlan{}, err
	}
	branch, err := createWorktreeGitOutput(ctx, repoRoot, "check-ref-format", "--branch", branchInput)
	if err != nil {
		return createWorktreePlan{}, fmt.Errorf("create_worktree: invalid branch %q: %w", branchInput, err)
	}
	if branch != branchInput {
		return createWorktreePlan{}, fmt.Errorf("create_worktree: invalid branch %q", branchInput)
	}
	base, err := createWorktreeGitOutput(ctx, repoRoot, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return createWorktreePlan{}, fmt.Errorf("create_worktree: resolve current commit: %w", err)
	}
	commonDir, err := createWorktreeGitOutput(ctx, repoRoot, "rev-parse", "--git-common-dir")
	if err != nil {
		return createWorktreePlan{}, fmt.Errorf("create_worktree: resolve Git metadata: %w", err)
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(repoRoot, commonDir)
	}
	commonDir, err = canonical(commonDir)
	if err != nil {
		return createWorktreePlan{}, fmt.Errorf("create_worktree: resolve Git metadata: %w", err)
	}
	if err := t.checkReadAccess(commonDir); err != nil {
		return createWorktreePlan{}, err
	}

	repoID := worktreeRepoID(commonDir)
	configuredRoot, configured, err := createWorktreeGitConfigValue(ctx, repoRoot, worktreeConfigKey)
	if err != nil {
		return createWorktreePlan{}, fmt.Errorf("create_worktree: read local worktree configuration: %w", err)
	}
	var worktreeRoot string
	rootSource := "global default root"
	if configured {
		worktreeRoot, err = resolveConfiguredWorktreeRoot(configuredRoot)
		if err != nil {
			return createWorktreePlan{}, err
		}
		rootSource = "local Git config"
	} else {
		defaultRoot, err := t.defaultRoot()
		if err != nil {
			return createWorktreePlan{}, err
		}
		worktreeRoot = filepath.Join(defaultRoot, repoID)
	}

	if err := validateWorktreeRoot(worktreeRoot); err != nil {
		return createWorktreePlan{}, err
	}
	worktreePath := filepath.Join(worktreeRoot, filepath.FromSlash(branch))
	rootCanonical, err := canonicalOrParent(worktreeRoot)
	if err != nil {
		return createWorktreePlan{}, fmt.Errorf("create_worktree: resolve worktree root: %w", err)
	}
	if isUnder(repoRoot, rootCanonical) {
		return createWorktreePlan{}, errors.New("create_worktree: worktree root must be outside the repository")
	}
	pathCanonical, err := canonicalOrParent(worktreePath)
	if err != nil {
		return createWorktreePlan{}, fmt.Errorf("create_worktree: resolve worktree path: %w", err)
	}
	if !isUnder(rootCanonical, pathCanonical) {
		return createWorktreePlan{}, errors.New("create_worktree: branch path escapes the worktree root")
	}

	if err := t.checkReadWriteAccess(repoRoot, commonDir, worktreeRoot, worktreePath); err != nil {
		return createWorktreePlan{}, err
	}
	if _, err := os.Lstat(worktreePath); err == nil {
		return createWorktreePlan{}, fmt.Errorf("create_worktree: worktree path %q already exists", filepath.ToSlash(worktreePath))
	} else if !os.IsNotExist(err) {
		return createWorktreePlan{}, fmt.Errorf("create_worktree: inspect worktree path: %w", err)
	}
	exists, err := createWorktreeBranchExists(ctx, repoRoot, branch)
	if err != nil {
		return createWorktreePlan{}, fmt.Errorf("create_worktree: check branch: %w", err)
	}
	if exists {
		return createWorktreePlan{}, fmt.Errorf("create_worktree: branch %q already exists", branch)
	}

	displayRoot := filepath.ToSlash(worktreeRoot)
	displayPath := filepath.ToSlash(worktreePath)
	previewText := fmt.Sprintf("create worktree\nbranch: %s\nbase: %s\nworktree root: %s\nroot source: %s\npath: %s\n", branch, base, displayRoot, rootSource, displayPath)
	return createWorktreePlan{
		repoRoot:     repoRoot,
		branch:       branch,
		base:         base,
		worktreeRoot: worktreeRoot,
		worktreePath: worktreePath,
		rootSource:   rootSource,
		repoID:       repoID,
		previewText:  previewText,
		preview: core.ToolResult{
			Content: []provider.Content{provider.TextBlock{Text: previewText}},
			Details: map[string]any{
				"state":         "ready",
				"branch":        branch,
				"base":          base,
				"worktree_root": displayRoot,
				"root_source":   rootSource,
				"repo_id":       repoID,
				"path":          displayPath,
			},
		},
	}, nil
}

func (t *CreateWorktreeTool) defaultRoot() (string, error) {
	root := strings.TrimSpace(t.DefaultRoot)
	if root == "" {
		return "", errors.New("create_worktree: default worktree root is not configured")
	}
	return canonicalOrParent(root)
}

// worktreeRepoID derives a stable, collision-resistant directory name for the
// repository that owns commonDir. The name pairs a readable slug with a hash of
// the canonical common directory so repositories sharing a base name still get
// distinct roots while one repository keeps the same directory across sessions.
func worktreeRepoID(commonDir string) string {
	sum := sha256.Sum256([]byte(filepath.ToSlash(commonDir)))
	return worktreeRepoSlug(commonDir) + "-" + hex.EncodeToString(sum[:])[:worktreeRepoIDHashLength]
}

func worktreeRepoSlug(commonDir string) string {
	base := filepath.Base(commonDir)
	if trimmed := strings.TrimSuffix(base, ".git"); trimmed != "" {
		base = trimmed
	} else {
		base = filepath.Base(filepath.Dir(commonDir))
	}
	var builder strings.Builder
	dashed := false
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			builder.WriteRune(r)
			dashed = false
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r + ('a' - 'A'))
			dashed = false
		default:
			if !dashed {
				builder.WriteByte('-')
				dashed = true
			}
		}
	}
	slug := strings.Trim(builder.String(), "-._")
	if len(slug) > worktreeRepoSlugMaxLen {
		slug = strings.Trim(slug[:worktreeRepoSlugMaxLen], "-._")
	}
	if slug == "" {
		slug = "repo"
	}
	return slug
}

func (t *CreateWorktreeTool) accessibleCWD() (string, error) {
	cwd := t.CWD
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("create_worktree: working directory: %w", err)
		}
	}
	cwd, err := canonical(cwd)
	if err != nil {
		return "", fmt.Errorf("create_worktree: resolve working directory: %w", err)
	}
	if err := t.checkReadAccess(cwd); err != nil {
		return "", err
	}
	return cwd, nil
}

func (t *CreateWorktreeTool) findGitRootCandidate(cwd string) (string, error) {
	for dir := cwd; ; dir = filepath.Dir(dir) {
		if err := t.checkReadAccess(dir); err != nil {
			return "", err
		}
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("create_worktree: inspect Git metadata: %w", err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return "", errors.New("create_worktree: working directory is not a Git repository")
}

func (t *CreateWorktreeTool) checkReadAccess(paths ...string) error {
	for _, path := range paths {
		if err := t.Sandbox.CheckReadPath(path); err != nil {
			return fmt.Errorf("create_worktree: %w", err)
		}
	}
	return nil
}

func (t *CreateWorktreeTool) checkReadWriteAccess(paths ...string) error {
	for _, path := range paths {
		if err := t.Sandbox.CheckReadPath(path); err != nil {
			return fmt.Errorf("create_worktree: %w", err)
		}
		if err := t.Sandbox.CheckWritePath(path); err != nil {
			return fmt.Errorf("create_worktree: %w", err)
		}
	}
	return nil
}

func resolveConfiguredWorktreeRoot(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("create_worktree: local worktree configuration is empty")
	}
	if !filepath.IsAbs(value) {
		return "", errors.New("create_worktree: local worktree configuration must be an absolute path; remove a legacy relative value with `git config --local --unset zut.worktrees.path`")
	}
	return filepath.Clean(value), nil
}

func validateWorktreeRoot(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create_worktree: inspect worktree root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("create_worktree: worktree root must not be a symbolic link")
	}
	if !info.IsDir() {
		return errors.New("create_worktree: worktree root exists but is not a directory")
	}
	return nil
}

func sameCreateWorktreePlan(a, b createWorktreePlan) bool {
	return a.repoRoot == b.repoRoot &&
		a.branch == b.branch &&
		a.base == b.base &&
		a.worktreeRoot == b.worktreeRoot &&
		a.worktreePath == b.worktreePath &&
		a.rootSource == b.rootSource &&
		a.repoID == b.repoID
}

func missingWorktreeParents(worktreePath string) ([]string, error) {
	var missing []string
	for directory := filepath.Dir(worktreePath); ; directory = filepath.Dir(directory) {
		info, err := os.Lstat(directory)
		if err == nil {
			if !info.IsDir() {
				return nil, fmt.Errorf("%q is not a directory", directory)
			}
			break
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
		missing = append(missing, directory)
		parent := filepath.Dir(directory)
		if parent == directory {
			return nil, errors.New("no existing parent directory")
		}
	}
	for left, right := 0, len(missing)-1; left < right; left, right = left+1, right-1 {
		missing[left], missing[right] = missing[right], missing[left]
	}
	return missing, nil
}

func removeCreatedWorktreeParents(parents []string) error {
	for index := len(parents) - 1; index >= 0; index-- {
		if err := os.Remove(parents[index]); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (p createWorktreePlan) cleanupFailedCheckout(ctx context.Context) error {
	registered, err := createWorktreeRegistered(ctx, p.repoRoot, p.worktreePath)
	if err != nil {
		return err
	}
	var cleanupErr error
	if registered {
		if _, err := createWorktreeGitOutput(ctx, p.repoRoot, "worktree", "remove", "--force", p.worktreePath); err != nil {
			cleanupErr = fmt.Errorf("remove registered worktree: %w", err)
		}
	}
	branchHead, exists, err := createWorktreeGitRef(ctx, p.repoRoot, "refs/heads/"+p.branch)
	if err == nil && exists && branchHead == p.base {
		if _, err := createWorktreeGitOutput(ctx, p.repoRoot, "branch", "-D", p.branch); err != nil && cleanupErr == nil {
			cleanupErr = fmt.Errorf("remove branch: %w", err)
		}
	}
	if err != nil && cleanupErr == nil {
		cleanupErr = err
	}
	return cleanupErr
}

func createWorktreeRegistered(ctx context.Context, repoRoot, worktreePath string) (bool, error) {
	output, err := createWorktreeGitOutput(ctx, repoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	want, err := canonicalOrParent(worktreePath)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		path, err := canonicalOrParent(strings.TrimPrefix(line, "worktree "))
		if err == nil && path == want {
			return true, nil
		}
	}
	return false, nil
}

func createWorktreeBranchExists(ctx context.Context, repoRoot, branch string) (bool, error) {
	_, err := createWorktreeGitOutput(ctx, repoRoot, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if createWorktreeGitExitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

func createWorktreeGitRef(ctx context.Context, repoRoot, ref string) (string, bool, error) {
	output, err := createWorktreeGitOutput(ctx, repoRoot, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err == nil {
		return output, true, nil
	}
	if createWorktreeGitExitCode(err) == 1 {
		return "", false, nil
	}
	return "", false, err
}

func createWorktreeGitConfigValue(ctx context.Context, repoRoot, key string) (string, bool, error) {
	output, err := createWorktreeGitOutput(ctx, repoRoot, "config", "--local", "--get", key)
	if err == nil {
		return output, true, nil
	}
	if createWorktreeGitExitCode(err) == 1 {
		return "", false, nil
	}
	return "", false, err
}

type createWorktreeGitError struct {
	args   []string
	output string
	err    error
}

func (e *createWorktreeGitError) Error() string {
	message := strings.TrimSpace(e.output)
	if message == "" {
		message = e.err.Error()
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.args, " "), message)
}

func (e *createWorktreeGitError) Unwrap() error { return e.err }

func createWorktreeGitExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func createWorktreeGitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmdArgs := append([]string{"-c", "core.hooksPath=" + os.DevNull, "-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Env = createWorktreeGitEnv()
	setProcessGroup(cmd)

	var stdout, output cappedGitOutput
	cmd.Stdout = io.MultiWriter(&stdout, &output)
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start git %s: %w", strings.Join(args, " "), err)
	}
	watchDone := make(chan struct{})
	watchStopped := make(chan struct{})
	go func() {
		defer close(watchStopped)
		select {
		case <-ctx.Done():
			killProcessGroup(cmd)
		case <-watchDone:
		}
	}()
	waitErr := cmd.Wait()
	close(watchDone)
	<-watchStopped
	if waitErr != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", &createWorktreeGitError{args: args, output: output.String(), err: waitErr}
	}
	return strings.TrimSpace(stdout.String()), nil
}

type cappedGitOutput struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	truncated bool
}

func (b *cappedGitOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := maxCreateWorktreeGitOutput - b.buffer.Len(); room > 0 {
		if len(p) > room {
			_, _ = b.buffer.Write(p[:room])
			b.truncated = true
		} else {
			_, _ = b.buffer.Write(p)
		}
	} else {
		b.truncated = true
	}
	return len(p), nil
}

func (b *cappedGitOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	output := b.buffer.String()
	if b.truncated {
		output += "\n... [git output truncated]"
	}
	return output
}

func createWorktreeGitEnv() []string {
	env := make([]string, 0, len(os.Environ())+5)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") || key == "SSH_ASKPASS" {
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
	)
}
