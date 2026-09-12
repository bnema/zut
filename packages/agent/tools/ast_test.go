package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestASTToolReportsMissingBackend(t *testing.T) {
	tool := &ASTTool{CWD: t.TempDir(), LookPath: func(string) (string, error) { return "", os.ErrNotExist }}
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"$A","language":"go","path":"."}`), nil)
	if err == nil || !strings.Contains(err.Error(), "ast-grep executable 'sg' not found") {
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

func TestBoundASTTextPreservesUTF8(t *testing.T) {
	got := boundASTText(strings.Repeat("界", maxASTOutputBytes))
	if !strings.Contains(got, "truncated") || !utf8.ValidString(got) {
		t.Fatalf("invalid bounded output")
	}
}
