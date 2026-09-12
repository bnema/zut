package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func globArgsJSON(pattern, path string, hidden bool) json.RawMessage {
	args := map[string]any{"pattern": pattern}
	if path != "" {
		args["path"] = path
	}
	if hidden {
		args["hidden"] = true
	}
	raw, _ := json.Marshal(args)
	return raw
}

func writeGlobFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func globDetails(result core.ToolResult) map[string]any {
	return result.Details.(map[string]any)
}

func globResultText(t *testing.T, result core.ToolResult) string {
	t.Helper()
	if len(result.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(result.Content))
	}
	text, ok := result.Content[0].(provider.TextBlock)
	if !ok {
		t.Fatalf("content type = %T, want TextBlock", result.Content[0])
	}
	return text.Text
}

func TestGlobStarMatchesBasenames(t *testing.T) {
	root := t.TempDir()
	writeGlobFile(t, filepath.Join(root, "a.go"), "")
	writeGlobFile(t, filepath.Join(root, "b.txt"), "")
	writeGlobFile(t, filepath.Join(root, "sub", "c.go"), "")

	tool := &GlobTool{CWD: root}
	result, err := tool.Execute(context.Background(), globArgsJSON("*.go", "", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	text := globResultText(t, result)
	if !strings.Contains(text, "a.go") || !strings.Contains(text, "sub/c.go") {
		t.Fatalf("output missing matches: %q", text)
	}
	if strings.Contains(text, "b.txt") {
		t.Fatalf("output contains non-match: %q", text)
	}
	if globDetails(result)["matches"] != 2 {
		t.Fatalf("matches = %v, want 2", globDetails(result)["matches"])
	}
}

func TestGlobGlobstarMatchesNestedPaths(t *testing.T) {
	root := t.TempDir()
	writeGlobFile(t, filepath.Join(root, "a.go"), "")
	writeGlobFile(t, filepath.Join(root, "sub", "deep", "b.go"), "")

	tool := &GlobTool{CWD: root}
	result, err := tool.Execute(context.Background(), globArgsJSON("**/*.go", "", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	text := globResultText(t, result)
	if !strings.Contains(text, "a.go") || !strings.Contains(text, "sub/deep/b.go") {
		t.Fatalf("globstar missed nested matches: %q", text)
	}
}

func TestGlobQuestionMarkAndCharacterClass(t *testing.T) {
	root := t.TempDir()
	writeGlobFile(t, filepath.Join(root, "a1.go"), "")
	writeGlobFile(t, filepath.Join(root, "a2.go"), "")
	writeGlobFile(t, filepath.Join(root, "abc.go"), "")

	tool := &GlobTool{CWD: root}
	result, err := tool.Execute(context.Background(), globArgsJSON("a?.go", "", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := globDetails(result)["matches"]; got != 2 {
		t.Fatalf("? matches = %v, want 2", got)
	}

	result, err = tool.Execute(context.Background(), globArgsJSON("a[12].go", "", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := globDetails(result)["matches"]; got != 2 {
		t.Fatalf("class matches = %v, want 2", got)
	}
}

func TestGlobSlashPatternMatchesRelativePath(t *testing.T) {
	root := t.TempDir()
	writeGlobFile(t, filepath.Join(root, "src", "a.go"), "")
	writeGlobFile(t, filepath.Join(root, "other", "a.go"), "")

	tool := &GlobTool{CWD: root}
	result, err := tool.Execute(context.Background(), globArgsJSON("src/*.go", "", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	text := globResultText(t, result)
	if !strings.Contains(text, "src/a.go") || strings.Contains(text, "other/a.go") {
		t.Fatalf("path pattern matched wrong set: %q", text)
	}
}

func TestGlobInvalidPatternIsRejected(t *testing.T) {
	tool := &GlobTool{CWD: t.TempDir()}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":""}`), nil); err == nil {
		t.Fatal("empty pattern accepted")
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"a["}`), nil); err == nil {
		t.Fatal("unterminated class accepted")
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":1}`), nil); err == nil {
		t.Fatal("non-string pattern accepted")
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"*.go","bogus":1}`), nil); err == nil {
		t.Fatal("unknown argument accepted")
	}
}

func TestGlobHiddenToggle(t *testing.T) {
	root := t.TempDir()
	writeGlobFile(t, filepath.Join(root, ".hidden.go"), "")
	writeGlobFile(t, filepath.Join(root, ".hdir", "inside.go"), "")
	writeGlobFile(t, filepath.Join(root, "visible.go"), "")
	writeGlobFile(t, filepath.Join(root, ".git", "objects", "x"), "")

	tool := &GlobTool{CWD: root}
	result, err := tool.Execute(context.Background(), globArgsJSON("*.go", "", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	text := globResultText(t, result)
	if strings.Contains(text, ".hidden") || strings.Contains(text, ".hdir") {
		t.Fatalf("hidden entries leaked: %q", text)
	}

	result, err = tool.Execute(context.Background(), globArgsJSON("*.go", "", true), nil)
	if err != nil {
		t.Fatal(err)
	}
	text = globResultText(t, result)
	if !strings.Contains(text, ".hidden.go") || !strings.Contains(text, ".hdir/inside.go") {
		t.Fatalf("hidden opt-in missed entries: %q", text)
	}
	if strings.Contains(text, ".git") {
		t.Fatalf(".git leaked with hidden opt-in: %q", text)
	}
}

func TestGlobHonorsRootAndNestedIgnores(t *testing.T) {
	root := t.TempDir()
	writeGlobFile(t, filepath.Join(root, ".gitignore"), "ignored.txt\nbuild/\n")
	writeGlobFile(t, filepath.Join(root, "ignored.txt"), "")
	writeGlobFile(t, filepath.Join(root, "kept.txt"), "")
	writeGlobFile(t, filepath.Join(root, "build", "out.txt"), "")
	writeGlobFile(t, filepath.Join(root, "sub", ".gitignore"), "local.txt\n")
	writeGlobFile(t, filepath.Join(root, "sub", "local.txt"), "")
	writeGlobFile(t, filepath.Join(root, "sub", "keep.txt"), "")

	tool := &GlobTool{CWD: root}
	result, err := tool.Execute(context.Background(), globArgsJSON("*.txt", "", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	text := globResultText(t, result)
	for _, leaked := range []string{"ignored.txt", "build/out.txt", "local.txt"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("ignored file leaked: %q in %q", leaked, text)
		}
	}
	if !strings.Contains(text, "kept.txt") || !strings.Contains(text, "sub/keep.txt") {
		t.Fatalf("kept files missing: %q", text)
	}

	// Narrowing with path preserves the inherited root rules.
	result, err = tool.Execute(context.Background(), globArgsJSON("*.txt", "sub", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	text = globResultText(t, result)
	if strings.Contains(text, "local.txt") {
		t.Fatalf("nested ignore lost with path narrowing: %q", text)
	}
	if !strings.Contains(text, "keep.txt") {
		t.Fatalf("narrowed search missed keep.txt: %q", text)
	}
}

func TestGlobNegationUnignores(t *testing.T) {
	root := t.TempDir()
	writeGlobFile(t, filepath.Join(root, ".gitignore"), "*.log\n!important.log\n")
	writeGlobFile(t, filepath.Join(root, "a.log"), "")
	writeGlobFile(t, filepath.Join(root, "important.log"), "")

	tool := &GlobTool{CWD: root}
	result, err := tool.Execute(context.Background(), globArgsJSON("*.log", "", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	text := globResultText(t, result)
	if strings.Contains(text, "a.log") || !strings.Contains(text, "important.log") {
		t.Fatalf("negation mishandled: %q", text)
	}
}

func TestGlobWalksExplicitlyRequestedIgnoredRoot(t *testing.T) {
	root := t.TempDir()
	writeGlobFile(t, filepath.Join(root, ".gitignore"), "build/\n")
	writeGlobFile(t, filepath.Join(root, "build", "out.txt"), "")

	tool := &GlobTool{CWD: root}
	result, err := tool.Execute(context.Background(), globArgsJSON("*.txt", "build", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(globResultText(t, result), "build/out.txt") {
		t.Fatalf("explicit ignored root not walked: %q", globResultText(t, result))
	}
}

func TestGlobFileRootIsRejected(t *testing.T) {
	root := t.TempDir()
	writeGlobFile(t, filepath.Join(root, "a.go"), "")

	tool := &GlobTool{CWD: root}
	if _, err := tool.Execute(context.Background(), globArgsJSON("*.go", "a.go", false), nil); err == nil {
		t.Fatal("file search root accepted")
	}
}

func TestGlobEmptyResults(t *testing.T) {
	tool := &GlobTool{CWD: t.TempDir()}
	result, err := tool.Execute(context.Background(), globArgsJSON("*.go", "", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := globResultText(t, result); got != "No files matched the pattern." {
		t.Fatalf("empty output = %q", got)
	}
	if globDetails(result)["matches"] != 0 || globDetails(result)["truncated"] != false {
		t.Fatalf("empty details = %v", result.Details)
	}
}

func TestGlobSortsDeterministically(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"z.go", "m.go", "a.go", "sub/z.go", "sub/a.go"} {
		writeGlobFile(t, filepath.Join(root, name), "")
	}
	tool := &GlobTool{CWD: root}
	first, err := tool.Execute(context.Background(), globArgsJSON("**/*.go", "", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := tool.Execute(context.Background(), globArgsJSON("**/*.go", "", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	a, b := globResultText(t, first), globResultText(t, second)
	if a != b {
		t.Fatalf("nondeterministic order:\n%s\n%s", a, b)
	}
	lines := strings.Split(a, "\n")
	for i := 1; i < len(lines); i++ {
		if lines[i-1] > lines[i] {
			t.Fatalf("unsorted output: %q", a)
		}
	}
}

func TestGlobRespectsSandbox(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGlobFile(t, filepath.Join(outside, "secret.go"), "")

	sandbox := NewSandbox(root)
	sandbox.Lock()
	tool := &GlobTool{CWD: root, Sandbox: sandbox}
	if _, err := tool.Execute(context.Background(), globArgsJSON("*.go", outside, false), nil); err == nil {
		t.Fatal("search outside jail accepted")
	} else if !strings.Contains(err.Error(), "jailed") {
		t.Fatalf("error = %v, want jail denial", err)
	}
}

func TestGlobRootSymlinkInsideAndOutsideJail(t *testing.T) {
	if os.Getenv("OS") == "Windows_NT" {
		t.Skip("symlink tests need privileges on windows")
	}
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	inner := filepath.Join(root, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGlobFile(t, filepath.Join(inner, "a.go"), "")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGlobFile(t, filepath.Join(outside, "secret.go"), "")

	sandbox := NewSandbox(root)
	sandbox.Lock()

	inside := filepath.Join(root, "link-inside")
	if err := os.Symlink(inner, inside); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	tool := &GlobTool{CWD: root, Sandbox: sandbox}
	result, err := tool.Execute(context.Background(), globArgsJSON("*.go", "link-inside", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(globResultText(t, result), "a.go") {
		t.Fatalf("symlinked root not traversed: %v", result)
	}

	escape := filepath.Join(root, "link-outside")
	if err := os.Symlink(outside, escape); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if _, err := tool.Execute(context.Background(), globArgsJSON("*.go", "link-outside", false), nil); err == nil {
		t.Fatal("symlinked root escaping the jail accepted")
	} else if !strings.Contains(err.Error(), "jailed") {
		t.Fatalf("error = %v, want jail denial", err)
	}
}

func TestGlobCancellationPropagates(t *testing.T) {
	root := t.TempDir()
	writeGlobFile(t, filepath.Join(root, "a.go"), "")
	tool := &GlobTool{CWD: root}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.Execute(ctx, globArgsJSON("*.go", "", false), nil); err == nil {
		t.Fatal("cancelled context accepted")
	}
}

func TestGlobReportsMetadata(t *testing.T) {
	root := t.TempDir()
	writeGlobFile(t, filepath.Join(root, "a.go"), "")
	tool := &GlobTool{CWD: root}
	result, err := tool.Execute(context.Background(), globArgsJSON("*.go", "", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	if globDetails(result)["pattern"] != "*.go" {
		t.Fatalf("pattern detail = %v", result.Details)
	}
	if globDetails(result)["truncated"] != false || globDetails(result)["matches"] != 1 {
		t.Fatalf("details = %v", result.Details)
	}
}

func TestGlobBoundsDepth(t *testing.T) {
	root := t.TempDir()
	deep := root
	for i := 0; i < maxGlobDepth+5; i++ {
		deep = filepath.Join(deep, "d")
	}
	writeGlobFile(t, filepath.Join(deep, "deep.go"), "")
	writeGlobFile(t, filepath.Join(root, "top.go"), "")

	tool := &GlobTool{CWD: root}
	result, err := tool.Execute(context.Background(), globArgsJSON("**/*.go", "", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	text := globResultText(t, result)
	if !strings.Contains(text, "top.go") {
		t.Fatalf("shallow match missing: %q", text)
	}
	if strings.Contains(text, "deep.go") {
		t.Fatalf("depth bound not enforced: %q", text)
	}
}

func TestGlobBoundsResults(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < maxGlobMatches+50; i++ {
		writeGlobFile(t, filepath.Join(root, strings.Repeat("f", 4)+string(rune('a'+i/26))+string(rune('a'+i%26))+".go"), "")
	}

	tool := &GlobTool{CWD: root}
	result, err := tool.Execute(context.Background(), globArgsJSON("*.go", "", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := globDetails(result)["matches"]; got != maxGlobMatches {
		t.Fatalf("matches = %v, want %d", got, maxGlobMatches)
	}
	if globDetails(result)["truncated"] != true {
		t.Fatalf("truncated = %v, want true", globDetails(result))
	}
	if !strings.Contains(globResultText(t, result), "Truncated") {
		t.Fatalf("truncation note missing: %q", globResultText(t, result))
	}
}

func TestGlobUnreadableEntriesAreSkipped(t *testing.T) {
	root := t.TempDir()
	writeGlobFile(t, filepath.Join(root, "a.go"), "")
	denied := filepath.Join(root, "denied")
	if err := os.MkdirAll(denied, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGlobFile(t, filepath.Join(denied, "b.go"), "")
	if err := os.Chmod(denied, 0o000); err != nil {
		t.Skipf("chmod unsupported: %v", err)
	}
	defer func() { _ = os.Chmod(denied, 0o755) }()

	tool := &GlobTool{CWD: root}
	result, err := tool.Execute(context.Background(), globArgsJSON("**/*.go", "", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(globResultText(t, result), "a.go") {
		t.Fatalf("readable match missing: %q", globResultText(t, result))
	}
}

func TestGlobNegatedCharacterClass(t *testing.T) {
	root := t.TempDir()
	writeGlobFile(t, filepath.Join(root, "a.go"), "")
	writeGlobFile(t, filepath.Join(root, "b.go"), "")
	writeGlobFile(t, filepath.Join(root, "c.go"), "")

	tool := &GlobTool{CWD: root}
	result, err := tool.Execute(context.Background(), globArgsJSON("[!ab].go", "", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	text := globResultText(t, result)
	if !strings.Contains(text, "c.go") || strings.Contains(text, "a.go") || strings.Contains(text, "b.go") {
		t.Fatalf("negated class matched wrong set: %q", text)
	}
}

func TestGlobSkipsSymlinkedDirectoryTargets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests need privileges on windows")
	}
	root := t.TempDir()
	inner := filepath.Join(root, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGlobFile(t, filepath.Join(inner, "a.go"), "")
	link := filepath.Join(root, "dirlink")
	if err := os.Symlink(inner, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	tool := &GlobTool{CWD: root}
	result, err := tool.Execute(context.Background(), globArgsJSON("*.go", "", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(globResultText(t, result), "dirlink") {
		t.Fatalf("symlinked directory listed as a file: %q", globResultText(t, result))
	}
}

func TestGlobSymlinkedRootKeepsRequestedName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests need privileges on windows")
	}
	base := t.TempDir()
	inner := filepath.Join(base, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGlobFile(t, filepath.Join(inner, "a.go"), "")
	link := filepath.Join(base, "link-inside")
	if err := os.Symlink(inner, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	tool := &GlobTool{CWD: base}
	result, err := tool.Execute(context.Background(), globArgsJSON("*.go", "link-inside", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := globResultText(t, result); got != "link-inside/a.go" {
		t.Fatalf("output = %q, want link-inside/a.go", got)
	}
}
