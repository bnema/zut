package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrepChecksDeclaredReadScopeBeforeRunning(t *testing.T) {
	skipGrepScriptTestsOnWindows(t)
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	allowed := filepath.Join(base, "allowed")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(allowed, 0o755); err != nil {
		t.Fatal(err)
	}
	sandbox := NewSandbox(base)
	sandbox.Lock()
	permissions := &PermissionSet{}
	permissions.FS.Read = []string{allowed}
	sandbox.SetPermissions(permissions)
	called := false
	tool := &GrepTool{
		CWD:     base,
		Sandbox: sandbox,
		LookPath: func(string) (string, error) {
			called = true
			return "", errors.New("lookup should not run")
		},
	}
	_, err := tool.Execute(context.Background(), grepArgsJSON("needle", root), nil)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error = %v, want declared read-scope denial", err)
	}
	if called {
		t.Fatal("executable lookup ran before the read-scope check")
	}
}
