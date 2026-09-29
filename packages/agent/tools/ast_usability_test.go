package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func realASTTool(t *testing.T, root string) *ASTTool {
	t.Helper()
	binary, err := exec.LookPath("ast-grep")
	if err != nil {
		t.Skip("ast-grep not installed")
	}
	return &ASTTool{CWD: root, LookPath: func(string) (string, error) { return binary, nil }}
}

func astText(t *testing.T, tool *ASTTool, raw string) string {
	t.Helper()
	result, err := tool.Execute(context.Background(), json.RawMessage(raw), nil)
	if err != nil {
		t.Fatalf("Execute(%s): %v", raw, err)
	}
	return result.Content[0].(provider.TextBlock).Text
}

func TestASTLanguageIsOptional(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.ts"), []byte("console.log(2)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := realASTTool(t, root)
	if got := astText(t, tool, `{"pattern":"print($A)","path":"."}`); !strings.Contains(got, "a.py:1") {
		t.Fatalf("python search = %q", got)
	}
	if got := astText(t, tool, `{"pattern":"console.log($$$)","path":"."}`); !strings.Contains(got, "b.ts:1") {
		t.Fatalf("typescript search = %q", got)
	}
}

// tree-sitter-go parses a bare `pkg.Func(args)` pattern as a type
// conversion, so without the retry these calls never match.
func TestASTGoQualifiedCallMatchesCalls(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	source := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\t_ = fmt.Errorf(\"a\")\n\t_ = fmt.Errorf(\"b %d\",\n\t\t1)\n}\n"
	if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := realASTTool(t, root)
	for _, raw := range []string{
		`{"pattern":"fmt.Errorf($$$ARGS)","language":"go","path":"."}`,
		`{"pattern":"fmt.Errorf($$$ARGS)","path":"."}`,
		`{"pattern":"fmt.Errorf($$$ARGS)","path":"main.go"}`,
	} {
		got := astText(t, tool, raw)
		if !strings.Contains(got, "main.go:6") || !strings.Contains(got, "main.go:7") {
			t.Fatalf("search %s = %q", raw, got)
		}
	}
	astText(t, tool, `{"pattern":"fmt.Errorf($$$ARGS)","path":".","rewrite":"errors.New($$$ARGS)"}`)
	got, _ := os.ReadFile(path)
	if strings.Contains(string(got), "fmt.Errorf") || strings.Count(string(got), "errors.New(") != 2 {
		t.Fatalf("rewrite = %q", got)
	}
}

func TestASTRewriteWritesThroughSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real.go")
	link := filepath.Join(root, "link.go")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if err := (&ASTTool{}).applyRewrite([]astChange{{path: link, original: []byte("old"), content: []byte("new"), mode: 0o644}}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link replaced: %v, %v", info, err)
	}
	if got, _ := os.ReadFile(target); string(got) != "new" {
		t.Fatalf("target = %q", got)
	}
}

func TestASTApplyRewriteRejectsSymlinkRetargetedOutsideJail(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.go")
	link := filepath.Join(root, "link.go")
	if err := os.WriteFile(outside, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The plan was made for an in-jail file, but the link now escapes.
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	sandbox := NewSandbox(root)
	sandbox.Lock()
	err := (&ASTTool{Sandbox: sandbox}).applyRewrite([]astChange{{path: link, original: []byte("old"), content: []byte("new"), mode: 0o644}})
	if err == nil {
		t.Fatal("rewrite through escaping symlink succeeded")
	}
	if got, _ := os.ReadFile(outside); string(got) != "old" {
		t.Fatalf("outside file = %q", got)
	}
}

func TestASTGoCallRetryFailureIsReported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test fixture uses a POSIX script")
	}
	root := t.TempDir()
	// `run` finds nothing (exit 1); the `scan` retry fails with an error.
	script := filepath.Join(root, "ast-grep")
	body := "#!/bin/sh\nif [ \"$1\" = scan ]; then echo 'Error: invalid rule' >&2; exit 8; fi\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	tool := &ASTTool{CWD: root, LookPath: func(string) (string, error) { return script, nil }}
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"fmt.Errorf($$$)","language":"go","path":"."}`), nil)
	if err == nil || !strings.Contains(err.Error(), "invalid rule") {
		t.Fatalf("Execute error = %v", err)
	}
}

func TestASTNoMatchIncludesPatternTip(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := astText(t, realASTTool(t, root), `{"pattern":"missing($A)","language":"go","path":"."}`)
	if !strings.Contains(got, "No structural matches.") || !strings.Contains(got, "$$$A matches zero or more") {
		t.Fatalf("no-match text = %q", got)
	}
}

func TestASTGoCallPatternDetection(t *testing.T) {
	for _, tc := range []struct {
		args astArgs
		root string
		want bool
	}{
		{astArgs{Pattern: "fmt.Errorf($$$)", Language: "go"}, "/src", true},
		{astArgs{Pattern: "fmt.Errorf($$$)"}, "/src", true},
		{astArgs{Pattern: "fmt.Errorf($$$)"}, "/src/main.go", true},
		{astArgs{Pattern: "fmt.Errorf($$$)"}, "/src/app.ts", false},
		{astArgs{Pattern: "console.log($$$)", Language: "typescript"}, "/src", false},
		{astArgs{Pattern: "foo($A)", Language: "go"}, "/src", false},
		{astArgs{Pattern: "if err != nil { $$$ }", Language: "go"}, "/src", false},
	} {
		if got := astGoCallPattern(tc.args, tc.root); got != tc.want {
			t.Errorf("astGoCallPattern(%+v, %q) = %v, want %v", tc.args, tc.root, got, tc.want)
		}
	}
}

func TestASTSchemaAndDescriptionTeachUsage(t *testing.T) {
	tool := &ASTTool{}
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	if strings.Join(schema.Required, ",") != "pattern,path" {
		t.Fatalf("required = %v", schema.Required)
	}
	for _, want := range []string{"$$$", "rewrite", "grep"} {
		if !strings.Contains(tool.Description(), want) {
			t.Fatalf("description missing %q", want)
		}
	}
}
