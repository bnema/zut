package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bnema/zut/packages/provider"
)

func TestASTToolReportsMissingBackend(t *testing.T) {
	tool := &ASTTool{CWD: t.TempDir(), LookPath: func(string) (string, error) { return "", os.ErrNotExist }}
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"$A","language":"go","path":"."}`), nil)
	if err == nil || !strings.Contains(err.Error(), "ast-grep executable ('ast-grep' or 'sg') not found") {
		t.Fatalf("Execute error = %v", err)
	}
}

func TestASTToolReportsCaptureLimitWithoutBrokenPipe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test fixture uses a POSIX script")
	}
	root := t.TempDir()
	script := filepath.Join(root, "ast-grep")
	body := "#!/bin/sh\nhead -c 4194305 /dev/zero | tr '\\0' x\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	tool := &ASTTool{CWD: root, LookPath: func(string) (string, error) { return script, nil }}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"$A","language":"go","path":"."}`), nil)
	if err != nil || !strings.Contains(result.Content[0].(provider.TextBlock).Text, "search output truncated") {
		t.Fatalf("search result = %+v, error = %v", result, err)
	}
	_, err = tool.Execute(context.Background(), json.RawMessage(`{"pattern":"$A","language":"go","path":".","rewrite":"x"}`), nil)
	if err == nil || !strings.Contains(err.Error(), "output exceeds") || strings.Contains(strings.ToLower(err.Error()), "broken pipe") {
		t.Fatalf("rewrite error = %v", err)
	}
}

func TestASTToolExecutesWithoutShellAndTracksDiscoveredFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test fixture uses a POSIX script")
	}
	root := t.TempDir()
	script := filepath.Join(root, "ast-grep")
	body := "#!/bin/sh\nprintf '%s\\n' '{\"file\":\"main.go\",\"range\":{}}'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	tool := &ASTTool{CWD: root, LookPath: func(string) (string, error) { return script, nil }}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"fmt.Errorf($MSG)","language":"go","path":"."}`), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := filepath.Join(root, "main.go")
	if len(result.Context.Discovers) != 1 || result.Context.Discovers[0] != want {
		t.Fatalf("discoveries = %v, want %s", result.Context.Discovers, want)
	}
}

func TestASTToolPrefersArchExecutableName(t *testing.T) {
	root := t.TempDir()
	var lookedUp []string
	tool := &ASTTool{CWD: root, LookPath: func(name string) (string, error) {
		lookedUp = append(lookedUp, name)
		if name == "ast-grep" {
			return "/usr/bin/ast-grep", nil
		}
		return "", os.ErrNotExist
	}}
	got, err := tool.executable()
	if err != nil || got != "/usr/bin/ast-grep" || len(lookedUp) != 1 {
		t.Fatalf("executable = %q, %v; lookups = %v", got, err, lookedUp)
	}
}

func TestASTToolRewritePreviewAndExecute(t *testing.T) {
	binary, err := exec.LookPath("ast-grep")
	if err != nil {
		t.Skip("ast-grep not installed")
	}
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	original := "package main\nfunc main() { println(\"x\") }\n"
	if err := os.WriteFile(path, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	tool := &ASTTool{CWD: root, LookPath: func(string) (string, error) { return binary, nil }}
	args := json.RawMessage(`{"pattern":"println($A)","language":"go","path":".","rewrite":"fmt.Println($A)"}`)
	preview, err := tool.Preview(context.Background(), args)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if !strings.Contains(preview.Content[0].(provider.TextBlock).Text, "+func main() { fmt.Println") {
		t.Fatalf("preview = %q", preview.Content[0].(provider.TextBlock).Text)
	}
	if got, _ := os.ReadFile(path); string(got) != original {
		t.Fatal("preview modified the file")
	}
	result, err := tool.Execute(context.Background(), args, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "fmt.Println") || len(result.Context.Mutates) != 1 {
		t.Fatalf("result = %q, context = %+v", got, result.Context)
	}
	if info, _ := os.Stat(path); runtime.GOOS != "windows" && info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
}

func TestASTRewriteFormatsGoSource(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	original := []byte("package main\n\nfunc add(left, right int) int { return left + right }\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(original, []byte("return left + right"))
	match := astTestMatch(path, "return left + right", "return left + right\n\n", start, start+len("return left + right"))
	plan, err := (&ASTTool{CWD: root}).planRewrite(root, astArgs{Language: "go", Rewrite: "{$$$BODY}"}, []astMatch{match})
	if err != nil {
		t.Fatalf("planRewrite: %v", err)
	}
	want := "package main\n\nfunc add(left, right int) int {\n\treturn left + right\n\n}\n"
	if got := string(plan.changes[0].content); got != want {
		t.Fatalf("rewrite = %q, want %q", got, want)
	}
}

func TestASTRewriteUsesOutermostNestedMatch(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	original := []byte("package main\nfunc main() { f(g(x)) }\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	matches := []astMatch{
		astTestMatch(path, "f(g(x))", "WRAP(f)", 27, 34),
		astTestMatch(path, "g(x)", "WRAP(g)", 29, 33),
	}
	plan, err := (&ASTTool{CWD: root}).planRewrite(root, astArgs{Rewrite: "WRAP($A)"}, matches)
	if err != nil {
		t.Fatalf("planRewrite: %v", err)
	}
	if got := string(plan.changes[0].content); !strings.Contains(got, "WRAP(f)") || strings.Contains(got, "WRAP(g)") {
		t.Fatalf("rewrite = %q", got)
	}
}

func astTestMatch(path, text, replacement string, start, end int) astMatch {
	match := astMatch{File: path, Text: text, Replacement: replacement}
	match.Range.ByteOffset.Start = start
	match.Range.ByteOffset.End = end
	return match
}

func TestBoundASTTextPreservesUTF8(t *testing.T) {
	got := boundASTText(strings.Repeat("界", maxASTOutputBytes))
	if !strings.Contains(got, "truncated") || !utf8.ValidString(got) {
		t.Fatalf("invalid bounded output")
	}
}

func TestASTFallbackRejectsOtherSG(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX script")
	}
	root := t.TempDir()
	path := filepath.Join(root, "sg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'shadow-utils 1.0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tool := &ASTTool{LookPath: func(name string) (string, error) {
		if name == "sg" {
			return path, nil
		}
		return "", os.ErrNotExist
	}}
	if _, err := tool.executable(); err == nil {
		t.Fatal("accepted non ast-grep sg")
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'ast-grep 0.44.1'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := tool.executable(); err != nil || got != path {
		t.Fatalf("executable = %q, %v", got, err)
	}
}

func TestASTRewriteFormattingDependsOnOriginal(t *testing.T) {
	for _, tc := range []struct{ name, original, replacement, want string }{
		{"unclean", "package main\nfunc main(){println(1)}\n", "println(2)", "package main\nfunc main(){println(2)}\n"},
		{"crlf", "package main\r\n\r\nfunc main() { println(1) }\r\n", "println( 2 )", "package main\r\n\r\nfunc main() { println(2) }\r\n"},
		{"mixed", "package main\r\n\nfunc main() { println(1) }\n", "println( 2 )", "package main\r\n\nfunc main() { println( 2 ) }\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "main.go")
			if err := os.WriteFile(path, []byte(tc.original), 0o644); err != nil {
				t.Fatal(err)
			}
			start := strings.Index(tc.original, "println(1)")
			match := astTestMatch(path, "println(1)", tc.replacement, start, start+len("println(1)"))
			plan, err := (&ASTTool{CWD: root}).planRewrite(root, astArgs{Language: "go", Rewrite: "x"}, []astMatch{match})
			if err != nil {
				t.Fatal(err)
			}
			if got := string(plan.changes[0].content); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestASTScopeAndReadOnly(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	path := filepath.Join(outside, "main.go")
	if err := os.WriteFile(path, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sandbox := NewSandbox(root)
	sandbox.Lock()
	tool := &ASTTool{CWD: root, Sandbox: sandbox}
	// Marshal the path: raw Windows backslashes are invalid JSON escapes.
	args, err := json.Marshal(map[string]string{"pattern": "$A", "language": "go", "path": path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), args, nil); err == nil || !strings.Contains(err.Error(), "outside sandbox") {
		t.Fatalf("jail: %v", err)
	}
	tool.ReadOnly = true
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"$A","language":"go","path":".","rewrite":"x"}`), nil); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("read-only: %v", err)
	}
	tool.ReadOnly = false
	match := astTestMatch(path, "package", "module", 0, 7)
	if _, err := tool.planRewrite(root, astArgs{Rewrite: "module"}, []astMatch{match}); err == nil || !strings.Contains(err.Error(), "outside sandbox") {
		t.Fatalf("write scope: %v", err)
	}
	permissions := &PermissionSet{}
	permissions.FS.Read = []string{root, outside}
	permissions.FS.Write = []string{root}
	unlocked := NewSandbox(root)
	unlocked.SetPermissions(permissions)
	if _, err := (&ASTTool{CWD: root, Sandbox: unlocked}).planRewrite(root, astArgs{Rewrite: "module"}, []astMatch{match}); err == nil || !strings.Contains(err.Error(), "outside declared scopes") {
		t.Fatalf("permission: %v", err)
	}
}

func TestASTWarningsAndExitError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX script")
	}
	root := t.TempDir()
	script := filepath.Join(root, "ast-grep")
	tool := &ASTTool{CWD: root, LookPath: func(string) (string, error) { return script, nil }}
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'Pattern contains an ERROR node' >&2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rewrite := range []string{"", `,"rewrite":"x"`} {
		result, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"$A","language":"go","path":"."`+rewrite+`}`), nil)
		if err != nil || !strings.Contains(result.Content[0].(provider.TextBlock).Text, "No structural matches.\nPattern contains an ERROR node") {
			t.Fatalf("result = %+v, %v", result, err)
		}
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'bad pattern' >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"$A","language":"go","path":"."}`), nil); err == nil || !strings.Contains(err.Error(), "bad pattern") {
		t.Fatalf("exit error: %v", err)
	}
}

func TestASTApplyRewriteValidatesAllBeforeWriting(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "a.go")
	second := filepath.Join(root, "b.go")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("old"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	changes := []astChange{
		{path: first, original: []byte("old"), content: []byte("new"), mode: 0o640},
		{path: second, original: []byte("old"), content: []byte("new"), mode: 0o640},
	}
	if err := os.WriteFile(second, []byte("changed"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := (&ASTTool{}).applyRewrite(changes); err == nil || !strings.Contains(err.Error(), "changed while applying") {
		t.Fatalf("error = %v", err)
	}
	if got, _ := os.ReadFile(first); string(got) != "old" {
		t.Fatalf("first file changed: %q", got)
	}
	if err := os.WriteFile(second, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := (&ASTTool{}).applyRewrite(changes); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(first); string(got) != "new" {
		t.Fatalf("first file = %q", got)
	}
	// Windows has no Unix permission bits to preserve.
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(first); err != nil || info.Mode().Perm() != 0o640 {
			t.Fatalf("mode = %v, %v", info, err)
		}
	}
}
