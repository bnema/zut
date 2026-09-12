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

func TestASTToolExecutesWithoutShellAndTracksDiscoveredFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test fixture uses a POSIX script")
	}
	root := t.TempDir()
	script := filepath.Join(root, "sg")
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
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
}

func TestBoundASTTextPreservesUTF8(t *testing.T) {
	got := boundASTText(strings.Repeat("界", maxASTOutputBytes))
	if !strings.Contains(got, "truncated") || !utf8.ValidString(got) {
		t.Fatalf("invalid bounded output")
	}
}
