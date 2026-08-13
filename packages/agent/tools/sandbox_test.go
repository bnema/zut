package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSandboxLockedBlocksOutside(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "a.txt")
	os.WriteFile(outsideFile, []byte("secret"), 0o644)

	sb := NewSandbox(root)
	sb.Lock()

	if err := sb.CheckPath(outsideFile); err == nil {
		t.Fatal("expected outside path to be blocked")
	}
	inside := filepath.Join(root, "ok.txt")
	if err := sb.CheckPath(inside); err != nil {
		t.Fatalf("inside path blocked unexpectedly: %v", err)
	}
}

func TestSandboxUnlockedAllows(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	sb := NewSandbox(root)
	if err := sb.CheckPath(filepath.Join(outside, "a.txt")); err != nil {
		t.Fatalf("unlocked should allow: %v", err)
	}
}

func TestSandboxCommandBanned(t *testing.T) {
	sb := NewSandbox(t.TempDir())
	sb.Lock()
	cases := []string{
		"sudo apt-get install foo",
		"rm -rf /",
		"cd /etc && ls",
		"cd .. && rm foo",
	}
	for _, c := range cases {
		if err := sb.CheckCommand(c); err == nil {
			t.Fatalf("expected %q to be banned", c)
		}
	}
	// Allowed:
	for _, c := range []string{"ls", "go test ./...", "cd subdir && ls"} {
		if err := sb.CheckCommand(c); err != nil {
			t.Fatalf("expected %q to be allowed: %v", c, err)
		}
	}
}

// TestBashToolRejectsOutsidePathWhenLocked is a regression for issue #94.
func TestBashToolRejectsOutsidePathWhenLocked(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	sb := NewSandbox(root)
	sb.Lock()
	tool := &BashTool{CWD: root, Sandbox: sb}

	_, err := tool.Execute(context.Background(), mustJSONRaw(t, map[string]any{"command": "cat " + outsideFile, "timeout": 1}), nil)
	if err == nil {
		t.Fatal("expected jailed bash to reject outside path")
	}
}

func TestSandboxAllowsNullDeviceRedirectionWhenLocked(t *testing.T) {
	sb := NewSandbox(t.TempDir())
	sb.Lock()

	allowed := []string{
		"command -v go >" + os.DevNull + " 2>&1",
		"cat <" + os.DevNull,
		"cat \"" + os.DevNull + "\"",
	}
	for _, command := range allowed {
		if err := sb.CheckCommand(command); err != nil {
			t.Errorf("expected %q to be allowed: %v", command, err)
		}
	}

	blocked := []string{"cat /dev/zero", "cat /dev/null/child"}
	for _, command := range blocked {
		if err := sb.CheckCommand(command); err == nil {
			t.Errorf("expected null-device lookalike %q to be blocked", command)
		}
	}
}

// TestSandboxAllowsCDIntoSubdir is the regression for issue #39: a `cd`
// into a subdirectory of the sandbox root, spelled as an absolute path,
// must be allowed. The old guard rejected any `cd /...` outright, which
// wasted turns and nudged the model toward trying to break out of jail.
func TestSandboxAllowsCDIntoSubdir(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "packages", "provider")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	sb := NewSandbox(root)
	sb.Lock()

	insideFile := filepath.Join(root, "inside file.txt")
	if err := os.WriteFile(insideFile, []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	allowed := []string{
		"cd " + sub + " && go build ./...",
		"cd " + root + " && go build ./...",
		"cd " + root, // bare cd to root
		"cd packages/provider && go build",
		"cd \"" + sub + "\" && ls", // quoted absolute path
		"cat \"" + insideFile + "\"",
	}
	for _, c := range allowed {
		if err := sb.CheckCommand(c); err != nil {
			t.Fatalf("expected %q to be allowed: %v", c, err)
		}
	}

	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	blocked := []string{
		"cd /etc",
		"cd / && ls",
		"cd ..",                    // parent of root escapes
		"cd " + filepath.Dir(root), // explicit parent
		"cat " + outsideFile,
		"cat <" + outsideFile,
		"grep secret ../secret.txt",
		"cat ${HOME}/.ssh/config",
	}
	for _, c := range blocked {
		if err := sb.CheckCommand(c); err == nil {
			t.Fatalf("expected %q to be blocked", c)
		}
	}
}

// TestSandboxDisplayPath covers issue #39's secondary cause: tool
// results / errors should present paths relative to the sandbox root
// when jailed, so the model isn't biased toward absolute paths.
func TestSandboxDisplayPath(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "pkg", "foo.go")
	outside := filepath.Join(t.TempDir(), "x.go")

	sb := NewSandbox(root)

	// Unlocked: returns the given form verbatim.
	if got := sb.DisplayPath(sub, "pkg/foo.go"); got != "pkg/foo.go" {
		t.Fatalf("unlocked DisplayPath = %q; want verbatim", got)
	}

	sb.Lock()
	if got := sb.DisplayPath(sub, sub); got != "./pkg/foo.go" {
		t.Fatalf("DisplayPath(abs inside) = %q; want ./pkg/foo.go", got)
	}
	if got := sb.DisplayPath(root, root); got != "." {
		t.Fatalf("DisplayPath(root) = %q; want .", got)
	}
	// Outside root: fall back to the given form (don't fabricate a path).
	if got := sb.DisplayPath(outside, "x.go"); got != "x.go" {
		t.Fatalf("DisplayPath(outside) = %q; want given fallback", got)
	}
}

func TestReadToolRejectsOutsideWhenLocked(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "a.txt")
	os.WriteFile(outsideFile, []byte("x"), 0o644)

	sb := NewSandbox(root)
	sb.Lock()
	tool := &ReadTool{CWD: root, Sandbox: sb}

	_, err := tool.Execute(context.Background(),
		mustJSONRaw(t, map[string]any{"path": outsideFile}), nil)
	if err == nil {
		t.Fatal("expected sandbox error")
	}
}

func mustJSONRaw(t *testing.T, v any) []byte {
	t.Helper()
	return mustJSON(t, v)
}
