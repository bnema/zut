package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
)

func TestCreateWorktreeUsesGlobalDefaultRoot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	defaultRoot := filepath.Join(t.TempDir(), "worktrees")
	branchBefore := gitTestOutput(t, repo, "branch", "--show-current")
	tool := newCreateWorktreeTestTool(t, repo, defaultRoot)

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"branch":"feature/default-root"}`), nil)
	if err != nil {
		t.Fatal(err)
	}

	repoID := testWorktreeRepoID(t, repo)
	worktree := filepath.Join(defaultRoot, repoID, "feature", "default-root")
	if info, err := os.Stat(worktree); err != nil || !info.IsDir() {
		t.Fatalf("worktree %q was not created: %v", worktree, err)
	}
	if got := gitTestOutput(t, worktree, "branch", "--show-current"); got != "feature/default-root" {
		t.Fatalf("worktree branch = %q, want %q", got, "feature/default-root")
	}
	if got := gitTestOutput(t, repo, "branch", "--show-current"); got != branchBefore {
		t.Fatalf("source branch changed from %q to %q", branchBefore, got)
	}
	details := result.Details.(map[string]any)
	if state, _ := details["state"].(string); state != "created" {
		t.Fatalf("state = %q, want created", state)
	}
	if source, _ := details["root_source"].(string); source != "global default root" {
		t.Fatalf("root source = %q", source)
	}
	if id, _ := details["repo_id"].(string); id != repoID {
		t.Fatalf("repo_id = %q, want %q", id, repoID)
	}
	if got, _ := details["worktree_root"].(string); got != filepath.ToSlash(filepath.Join(defaultRoot, repoID)) {
		t.Fatalf("worktree_root = %q", got)
	}
	if got := gitTestOutputAllowExit(t, repo, "config", "--local", "--get", worktreeConfigKey); got.exitCode != 1 {
		t.Fatalf("default root saved local config: %#v", got)
	}
	for _, path := range []string{".worktrees", ".gitignore"} {
		if _, statErr := os.Stat(filepath.Join(repo, path)); !os.IsNotExist(statErr) {
			t.Fatalf("default root created repository file %s: %v", path, statErr)
		}
	}
}

func TestCreateWorktreeDefaultRootIsStableAcrossInvocations(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	defaultRoot := filepath.Join(t.TempDir(), "worktrees")
	nested := filepath.Join(repo, "nested", "dir")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	repoID := testWorktreeRepoID(t, repo)

	nestedTool := newCreateWorktreeTestTool(t, nested, defaultRoot)
	if _, err := nestedTool.Execute(context.Background(), json.RawMessage(`{"branch":"feature/from-nested"}`), nil); err != nil {
		t.Fatal(err)
	}
	rootTool := newCreateWorktreeTestTool(t, repo, defaultRoot)
	if _, err := rootTool.Execute(context.Background(), json.RawMessage(`{"branch":"feature/from-root"}`), nil); err != nil {
		t.Fatal(err)
	}

	for _, branch := range []string{"feature/from-nested", "feature/from-root"} {
		worktree := filepath.Join(defaultRoot, repoID, filepath.FromSlash(branch))
		if info, err := os.Stat(worktree); err != nil || !info.IsDir() {
			t.Fatalf("worktree %q was not created: %v", worktree, err)
		}
	}
}

func TestCreateWorktreeDefaultRootSlugIsReadable(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	parent := t.TempDir()
	repo := filepath.Join(parent, "My Project!")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	initWorktreeTestRepoAt(t, repo)
	defaultRoot := filepath.Join(t.TempDir(), "worktrees")
	tool := newCreateWorktreeTestTool(t, repo, defaultRoot)

	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"branch":"feature/slug"}`), nil); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(defaultRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("default root entries = %d, want 1", len(entries))
	}
	if name := entries[0].Name(); !strings.HasPrefix(name, "my-project-") {
		t.Fatalf("repo directory %q does not carry a readable slug", name)
	}
}

func TestCreateWorktreeSeparatesSameNamedRepositories(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	parent := t.TempDir()
	repoA := filepath.Join(parent, "a", "shared")
	repoB := filepath.Join(parent, "b", "shared")
	for _, repo := range []string{repoA, repoB} {
		if err := os.MkdirAll(repo, 0o755); err != nil {
			t.Fatal(err)
		}
		initWorktreeTestRepoAt(t, repo)
	}
	defaultRoot := filepath.Join(t.TempDir(), "worktrees")

	toolA := newCreateWorktreeTestTool(t, repoA, defaultRoot)
	if _, err := toolA.Execute(context.Background(), json.RawMessage(`{"branch":"feature/a"}`), nil); err != nil {
		t.Fatal(err)
	}
	toolB := newCreateWorktreeTestTool(t, repoB, defaultRoot)
	if _, err := toolB.Execute(context.Background(), json.RawMessage(`{"branch":"feature/b"}`), nil); err != nil {
		t.Fatal(err)
	}

	if idA, idB := testWorktreeRepoID(t, repoA), testWorktreeRepoID(t, repoB); idA == idB {
		t.Fatalf("same-named repositories share repo ID %q", idA)
	}
	entries, err := os.ReadDir(defaultRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("default root entries = %d, want 2", len(entries))
	}
	for repo, branch := range map[string]string{repoA: "feature/a", repoB: "feature/b"} {
		worktree := filepath.Join(defaultRoot, testWorktreeRepoID(t, repo), filepath.FromSlash(branch))
		if _, err := os.Stat(worktree); err != nil {
			t.Fatalf("worktree %q was not created: %v", worktree, err)
		}
	}
}

func TestCreateWorktreeConfiguredRootOverridesDefaultRoot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	externalRoot, err := canonicalOrParent(filepath.Join(t.TempDir(), "company-worktrees"))
	if err != nil {
		t.Fatal(err)
	}
	defaultRoot := filepath.Join(t.TempDir(), "worktrees")
	gitTestOutput(t, repo, "config", "--local", worktreeConfigKey, externalRoot)
	tool := newCreateWorktreeTestTool(t, repo, defaultRoot)

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"branch":"feature/external"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(externalRoot, "feature", "external")
	if info, err := os.Stat(worktree); err != nil || !info.IsDir() {
		t.Fatalf("external worktree %q was not created: %v", worktree, err)
	}
	if got := gitTestOutput(t, worktree, "branch", "--show-current"); got != "feature/external" {
		t.Fatalf("external branch = %q", got)
	}
	if source, _ := result.Details.(map[string]any)["root_source"].(string); source != "local Git config" {
		t.Fatalf("root source = %q", source)
	}
	if _, err := os.Stat(defaultRoot); !os.IsNotExist(err) {
		t.Fatalf("configured root still touched the default root: %v", err)
	}
}

func TestCreateWorktreeRejectsRelativeConfiguredRoot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	gitTestOutput(t, repo, "config", "--local", worktreeConfigKey, ".worktrees")
	tool := newCreateWorktreeTestTool(t, repo, filepath.Join(t.TempDir(), "worktrees"))

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"branch":"feature/relative"}`), nil)
	if err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("error = %v, want absolute-path error", err)
	}
	if _, statErr := os.Stat(filepath.Join(repo, ".worktrees")); !os.IsNotExist(statErr) {
		t.Fatalf("relative configured root created directory: %v", statErr)
	}
	if got := gitTestOutputAllowExit(t, repo, "show-ref", "--verify", "--quiet", "refs/heads/feature/relative"); got.exitCode != 1 {
		t.Fatalf("relative configured root created branch: %#v", got)
	}
}

func TestCreateWorktreeRejectsConfiguredRootResolvingInsideRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	link := filepath.Join(t.TempDir(), "repository-link")
	if err := os.Symlink(repo, link); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	root := filepath.Join(link, "external-worktrees")
	gitTestOutput(t, repo, "config", "--local", worktreeConfigKey, root)
	tool := newCreateWorktreeTestTool(t, repo, filepath.Join(t.TempDir(), "worktrees"))

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"branch":"feature/inside-repository"}`), nil)
	if err == nil || !strings.Contains(err.Error(), "must be outside the repository") {
		t.Fatalf("error = %v, want outside-repository error", err)
	}
}

func TestCreateWorktreeRequiresDefaultRoot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	tool := &CreateWorktreeTool{CWD: repo, Sandbox: NewSandbox(repo)}

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"branch":"feature/no-default-root"}`), nil)
	if err == nil || !strings.Contains(err.Error(), "default worktree root is not configured") {
		t.Fatalf("error = %v, want default-root error", err)
	}
}

func TestCreateWorktreeSchemaDropsBootstrapRoot(t *testing.T) {
	schema := string((&CreateWorktreeTool{}).Schema())
	if strings.Contains(schema, "bootstrap_root") {
		t.Fatalf("schema still exposes bootstrap_root:\n%s", schema)
	}
	var parsed struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal([]byte(schema), &parsed); err != nil {
		t.Fatal(err)
	}
	if _, ok := parsed.Properties["branch"]; !ok {
		t.Fatalf("schema lacks branch: %s", schema)
	}
	if len(parsed.Properties) != 1 {
		t.Fatalf("schema properties = %v, want only branch", parsed.Properties)
	}
	if len(parsed.Required) != 1 || parsed.Required[0] != "branch" {
		t.Fatalf("schema required = %v", parsed.Required)
	}
}

func TestWorktreeRepoIDIsReadableStableAndDistinct(t *testing.T) {
	tests := []struct {
		name      string
		commonDir string
		slug      string
	}{
		{name: "worktree metadata", commonDir: filepath.Join("home", "u", "src", "my-repo", ".git"), slug: "my-repo"},
		{name: "bare repository", commonDir: filepath.Join("srv", "git", "tools.git"), slug: "tools"},
		{name: "unusual characters", commonDir: filepath.Join("home", "u", "My Project!", ".git"), slug: "my-project"},
		{name: "empty slug", commonDir: filepath.Join("home", "@@@", ".git"), slug: "repo"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			id := worktreeRepoID(test.commonDir)
			if want := test.slug + "-"; !strings.HasPrefix(id, want) {
				t.Fatalf("repo ID = %q, want prefix %q", id, want)
			}
			if got := len(id) - len(test.slug) - 1; got != worktreeRepoIDHashLength {
				t.Fatalf("repo ID hash length = %d, want %d", got, worktreeRepoIDHashLength)
			}
			if again := worktreeRepoID(test.commonDir); again != id {
				t.Fatalf("repo ID is not stable: %q then %q", id, again)
			}
		})
	}
	if a, b := worktreeRepoID(filepath.Join("a", "shared", ".git")), worktreeRepoID(filepath.Join("b", "shared", ".git")); a == b {
		t.Fatalf("distinct repositories share repo ID %q", a)
	}
	if a, b := worktreeRepoID(filepath.Join("a", "Shared", ".git")), worktreeRepoID(filepath.Join("a", "shared", ".git")); a == b {
		t.Fatalf("case-different repositories share repo ID %q", a)
	}
}

func TestCreateWorktreeRejectsBranchSyntaxThatGitCanInterpret(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	tool := newCreateWorktreeTestTool(t, repo, filepath.Join(t.TempDir(), "worktrees"))
	for _, branch := range []string{"-option", "topic@{1}"} {
		_, err := tool.Execute(context.Background(), mustJSON(t, map[string]string{"branch": branch}), nil)
		if err == nil || !strings.Contains(err.Error(), "invalid branch") {
			t.Fatalf("branch %q error = %v", branch, err)
		}
	}
}

func TestCreateWorktreeRejectsNonRepository(t *testing.T) {
	dir := t.TempDir()
	defaultRoot := filepath.Join(t.TempDir(), "worktrees")
	tool := newCreateWorktreeTestTool(t, dir, defaultRoot)

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"branch":"feature/new"}`), nil)
	if err == nil || !strings.Contains(err.Error(), "not a Git repository") {
		t.Fatalf("error = %v, want repository error", err)
	}
	if _, statErr := os.Stat(defaultRoot); !os.IsNotExist(statErr) {
		t.Fatalf("non-repository call created the default root: %v", statErr)
	}
}

func TestCreateWorktreeRequiresSandbox(t *testing.T) {
	tool := &CreateWorktreeTool{CWD: t.TempDir(), DefaultRoot: filepath.Join(t.TempDir(), "worktrees")}

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"branch":"feature/no-sandbox"}`), nil)
	if err == nil || !strings.Contains(err.Error(), "sandbox is required") {
		t.Fatalf("error = %v, want sandbox error", err)
	}
}

func TestCreateWorktreeRejectsExistingBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	tool := newCreateWorktreeTestTool(t, repo, filepath.Join(t.TempDir(), "worktrees"))
	args := json.RawMessage(`{"branch":"feature/existing"}`)
	if _, err := tool.Execute(context.Background(), args, nil); err != nil {
		t.Fatal(err)
	}

	_, err := tool.Execute(context.Background(), args, nil)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %v, want collision error", err)
	}
}

func TestCreateWorktreeDisablesPostCheckoutHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Git hook fixture uses a POSIX shell")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	sentinel := filepath.Join(repo, "hook-ran")
	hook := filepath.Join(repo, ".git", "hooks", "post-checkout")
	script := "#!/bin/sh\ntouch " + shellQuoteForTest(sentinel) + "\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	defaultRoot := filepath.Join(t.TempDir(), "worktrees")
	tool := newCreateWorktreeTestTool(t, repo, defaultRoot)

	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"branch":"feature/no-hook"}`), nil); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(defaultRoot, testWorktreeRepoID(t, repo), "feature", "no-hook")
	if info, err := os.Stat(worktree); err != nil || !info.IsDir() {
		t.Fatalf("worktree %q was not created: %v", worktree, err)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("post-checkout hook ran: %v", err)
	}
}

func TestCreateWorktreeCleanupRemovesPartialCheckoutAndBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "worktrees", "partial")
	if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
		t.Fatal(err)
	}
	base := gitTestOutput(t, repo, "rev-parse", "HEAD")
	gitTestOutput(t, repo, "worktree", "add", "-b", "feature/partial", worktreePath, base)
	plan := createWorktreePlan{
		repoRoot:     repo,
		branch:       "feature/partial",
		base:         base,
		worktreePath: worktreePath,
	}

	if err := plan.cleanupFailedCheckout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("partial worktree remains: %v", err)
	}
	if got := gitTestOutputAllowExit(t, repo, "show-ref", "--verify", "--quiet", "refs/heads/feature/partial"); got.exitCode != 1 {
		t.Fatalf("partial branch remains: %#v", got)
	}
}

func TestExecuteCreateWorktreePlanRemovesCreatedParentsOnCheckoutFailure(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	branch := "feature/checkout-failure"
	root := filepath.Join(t.TempDir(), "worktrees", "repo-id")
	plan := createWorktreePlan{
		repoRoot:     repo,
		branch:       branch,
		base:         "not-a-commit",
		worktreeRoot: root,
		worktreePath: filepath.Join(root, "feature", "checkout-failure"),
		rootSource:   "global default root",
	}

	_, err := executeCreateWorktreePlan(context.Background(), plan, nil)
	if err == nil || !strings.Contains(err.Error(), "create worktree") {
		t.Fatalf("error = %v, want worktree creation error", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("failed checkout left worktree directories: %v", err)
	}
	if got := gitTestOutputAllowExit(t, repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); got.exitCode != 1 {
		t.Fatalf("failed checkout created branch: %#v", got)
	}
}

func TestCreateWorktreePreviewDoesNotModifyRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	defaultRoot := filepath.Join(t.TempDir(), "worktrees")
	tool := newCreateWorktreeTestTool(t, repo, defaultRoot)
	args := json.RawMessage(`{"branch":"feature/preview"}`)

	preview, err := tool.Preview(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	text := preview.Content[0].(provider.TextBlock).Text
	for _, want := range []string{"create worktree", "feature/preview", "global default root", testWorktreeRepoID(t, repo)} {
		if !strings.Contains(text, want) {
			t.Fatalf("preview missing %q:\n%s", want, text)
		}
	}
	if state, _ := preview.Details.(map[string]any)["state"].(string); state != "ready" {
		t.Fatalf("preview state = %q, want ready", state)
	}
	if _, err := os.Stat(defaultRoot); !os.IsNotExist(err) {
		t.Fatalf("preview created the default root: %v", err)
	}
	for _, path := range []string{".worktrees", ".gitignore"} {
		if _, statErr := os.Stat(filepath.Join(repo, path)); !os.IsNotExist(statErr) {
			t.Fatalf("preview created repository file %s: %v", path, statErr)
		}
	}
	cmd := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/feature/preview")
	if err := cmd.Run(); err == nil {
		t.Fatal("preview created the branch")
	}
}

func TestCreateWorktreePreviewThenExecuteCreatesWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	defaultRoot := filepath.Join(t.TempDir(), "worktrees")
	tool := newCreateWorktreeTestTool(t, repo, defaultRoot)
	args := json.RawMessage(`{"branch":"feature/previewed"}`)

	if _, err := tool.Preview(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	result, err := tool.Execute(context.Background(), args, nil)
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(defaultRoot, testWorktreeRepoID(t, repo), "feature", "previewed")
	if info, err := os.Stat(worktree); err != nil || !info.IsDir() {
		t.Fatalf("worktree %q was not created: %v", worktree, err)
	}
	if state, _ := result.Details.(map[string]any)["state"].(string); state != "created" {
		t.Fatalf("state = %q, want created", state)
	}
}

func TestCreateWorktreeRejectsStateChangedAfterPreview(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	tool := newCreateWorktreeTestTool(t, repo, filepath.Join(t.TempDir(), "worktrees"))
	args := json.RawMessage(`{"branch":"feature/stale-preview"}`)
	if _, err := tool.Preview(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "after-preview.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestOutput(t, repo, "add", "after-preview.txt")
	gitTestOutput(t, repo, "commit", "-qm", "change after preview")

	_, err := tool.Execute(context.Background(), args, nil)
	if err == nil || !strings.Contains(err.Error(), "state changed since preview") {
		t.Fatalf("error = %v, want stale-preview error", err)
	}
	if got := gitTestOutputAllowExit(t, repo, "show-ref", "--verify", "--quiet", "refs/heads/feature/stale-preview"); got.exitCode != 1 {
		t.Fatalf("stale preview created branch: %#v", got)
	}
}

func TestCreateWorktreeRespectsJail(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	nested := filepath.Join(repo, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	defaultRoot := filepath.Join(t.TempDir(), "worktrees")
	sandbox := NewSandbox(nested)
	sandbox.Lock()
	tool := &CreateWorktreeTool{CWD: nested, Sandbox: sandbox, DefaultRoot: defaultRoot}

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"branch":"feature/jailed"}`), nil)
	if err == nil || !strings.Contains(err.Error(), "jailed:") {
		t.Fatalf("error = %v, want jailed error", err)
	}
	for _, path := range []string{filepath.Join(repo, ".worktrees"), filepath.Join(repo, ".gitignore")} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("jailed call created %s: %v", path, statErr)
		}
	}
	if _, statErr := os.Stat(defaultRoot); !os.IsNotExist(statErr) {
		t.Fatalf("jailed call created the default root: %v", statErr)
	}
}

func TestCreateWorktreeRejectsJailedAncestorBeforeGitDiscovery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Git invocation fixture uses a POSIX shell")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	nested := filepath.Join(repo, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(repo, "git-was-invoked")
	binDir := t.TempDir()
	fakeGit := filepath.Join(binDir, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\ntouch "+shellQuoteForTest(sentinel)+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	sandbox := NewSandbox(nested)
	sandbox.Lock()
	tool := &CreateWorktreeTool{CWD: nested, Sandbox: sandbox, DefaultRoot: filepath.Join(t.TempDir(), "worktrees")}

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"branch":"feature/jailed-discovery"}`), nil)
	if err == nil || !strings.Contains(err.Error(), "jailed:") {
		t.Fatalf("error = %v, want jailed error", err)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("Git ran before jailed ancestor was rejected: %v", err)
	}
}

func TestCreateWorktreeRequiresFilesystemPermissions(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	defaultRoot := filepath.Join(t.TempDir(), "worktrees")
	sandbox := NewSandbox(repo)
	permissions := &PermissionSet{}
	permissions.Bash.Mode = "allowlist"
	permissions.Bash.Allow = []string{"git"}
	sandbox.SetPermissions(permissions)
	tool := &CreateWorktreeTool{CWD: repo, Sandbox: sandbox, DefaultRoot: defaultRoot}

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"branch":"feature/restricted"}`), nil)
	if err == nil || !strings.Contains(err.Error(), "no filesystem read permission") {
		t.Fatalf("error = %v, want filesystem permission error", err)
	}
	if _, statErr := os.Stat(defaultRoot); !os.IsNotExist(statErr) {
		t.Fatalf("restricted call created the default root: %v", statErr)
	}
}

func TestCreateWorktreeRequiresGitBashPermission(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	defaultRoot := filepath.Join(t.TempDir(), "worktrees")
	sandbox := NewSandbox(repo)
	permissions := &PermissionSet{}
	permissions.FS.Read = []string{repo}
	permissions.FS.Write = []string{repo}
	sandbox.SetPermissions(permissions)
	tool := &CreateWorktreeTool{CWD: repo, Sandbox: sandbox, DefaultRoot: defaultRoot}

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"branch":"feature/no-git"}`), nil)
	if err == nil || !strings.Contains(err.Error(), "no bash permission") {
		t.Fatalf("error = %v, want git permission error", err)
	}
	if _, statErr := os.Stat(defaultRoot); !os.IsNotExist(statErr) {
		t.Fatalf("restricted call created the default root: %v", statErr)
	}
}

func TestCreateWorktreeWritesOutsideDeclaredScopesAreDenied(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	defaultRoot := filepath.Join(t.TempDir(), "worktrees")
	sandbox := NewSandbox(repo)
	permissions := &PermissionSet{}
	permissions.FS.Read = []string{repo}
	permissions.FS.Write = []string{repo}
	permissions.Bash.Mode = "allowlist"
	permissions.Bash.Allow = []string{"git"}
	sandbox.SetPermissions(permissions)
	tool := &CreateWorktreeTool{CWD: repo, Sandbox: sandbox, DefaultRoot: defaultRoot}

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"branch":"feature/out-of-scope"}`), nil)
	if err == nil || !strings.Contains(err.Error(), "outside declared scopes") {
		t.Fatalf("error = %v, want scope-denied error", err)
	}
	if _, statErr := os.Stat(defaultRoot); !os.IsNotExist(statErr) {
		t.Fatalf("denied call created the default root: %v", statErr)
	}
}

func TestCreateWorktreeRejectsInvalidWorktreePathBeforeCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := initWorktreeTestRepo(t)
	defaultRoot := filepath.Join(t.TempDir(), "worktrees")
	repoRoot := filepath.Join(defaultRoot, testWorktreeRepoID(t, repo))
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "blocked"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := newCreateWorktreeTestTool(t, repo, defaultRoot)

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"branch":"blocked/new"}`), nil)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("error = %v, want non-directory path error", err)
	}
	cmd := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/blocked/new")
	if err := cmd.Run(); err == nil {
		t.Fatal("invalid path created the branch")
	}
}

func TestCreateWorktreeGitOutputKeepsStderrOutOfSuccessfulOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Git fixture uses a POSIX shell")
	}
	binDir := t.TempDir()
	gitPath := filepath.Join(binDir, "git")
	if err := os.WriteFile(gitPath, []byte("#!/bin/sh\nprintf 'value\\n'\nprintf 'warning\\n' >&2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	output, err := createWorktreeGitOutput(context.Background(), t.TempDir(), "status")
	if err != nil {
		t.Fatal(err)
	}
	if output != "value" {
		t.Fatalf("output = %q, want value", output)
	}
}

func TestCreateWorktreeGitCancelsDescendants(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group fixture uses a POSIX shell")
	}
	binDir := t.TempDir()
	started := filepath.Join(t.TempDir(), "git-started")
	childPID := filepath.Join(t.TempDir(), "child-pid")
	gitPath := filepath.Join(binDir, "git")
	script := "#!/bin/sh\nsleep 30 &\nprintf '%s' \"$!\" > " + shellQuoteForTest(childPID) + "\nprintf started > " + shellQuoteForTest(started) + "\nwait\n"
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cwd := t.TempDir()
	done := make(chan error, 1)
	go func() {
		_, err := createWorktreeGitOutput(ctx, cwd, "status")
		done <- err
	}()

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		if _, err := os.Stat(started); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		select {
		case <-deadline.C:
			t.Fatal("fake git did not start")
		case <-poll.C:
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Git process group did not exit after cancellation")
	}
	pid, err := os.ReadFile(childPID)
	if err != nil {
		t.Fatal(err)
	}
	exitDeadline := time.NewTimer(5 * time.Second)
	defer exitDeadline.Stop()
	for {
		err := exec.Command("kill", "-0", strings.TrimSpace(string(pid))).Run()
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				break
			}
			t.Fatalf("check child process: %v", err)
		}
		select {
		case <-exitDeadline.C:
			t.Fatalf("Git descendant %q remained after cancellation", pid)
		case <-poll.C:
		}
	}
}

func newCreateWorktreeTestTool(t *testing.T, cwd, defaultRoot string) *CreateWorktreeTool {
	t.Helper()
	return &CreateWorktreeTool{CWD: cwd, Sandbox: NewSandbox(cwd), DefaultRoot: defaultRoot}
}

func testWorktreeRepoID(t *testing.T, repo string) string {
	t.Helper()
	real, err := canonical(repo)
	if err != nil {
		t.Fatal(err)
	}
	commonDir, err := canonical(filepath.Join(real, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	return worktreeRepoID(commonDir)
}

func initWorktreeTestRepo(t *testing.T) string {
	t.Helper()
	return initWorktreeTestRepoAt(t, t.TempDir())
}

func initWorktreeTestRepoAt(t *testing.T, repo string) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	gitTestOutput(t, repo, "init", "-q")
	gitTestOutput(t, repo, "config", "user.email", "test@example.invalid")
	gitTestOutput(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestOutput(t, repo, "add", "README.md")
	gitTestOutput(t, repo, "commit", "-qm", "initial")
	return repo
}

func shellQuoteForTest(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

type gitTestResult struct {
	output   string
	exitCode int
}

func gitTestOutputAllowExit(t *testing.T, dir string, args ...string) gitTestResult {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return gitTestResult{output: strings.TrimSpace(string(output))}
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return gitTestResult{output: strings.TrimSpace(string(output)), exitCode: exitErr.ExitCode()}
	}
	t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	return gitTestResult{}
}

func gitTestOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
