package agent

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

func TestSessionMCPNoSignalHandlers(t *testing.T) {
	// Inspect the shared startup branch: only its processSignals arm may
	// register signals. This avoids replacing function fields in new tests.
	f, err := parser.ParseFile(token.NewFileSet(), "mcp_runtime.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "startMCPWithEnv" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if branch, ok := n.(*ast.IfStmt); ok {
				if id, ok := branch.Cond.(*ast.Ident); ok && id.Name == "processSignals" {
					return false
				}
			}
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && (id.Name == "mcpSignalNotify" || id.Name == "mcpSignalExit") {
					t.Errorf("process operation outside guarded branch: %s", id.Name)
				}
			}
			return true
		})
	}
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	// Exercise the manager startup branch, not the empty-config shortcut.
	if err := os.WriteFile(filepath.Join(home, "mcp.json"), []byte(`{"mcpServers":{"missing":{"command":"/nonexistent/zut-l2-server"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	args := Args{CWD: home}
	manager, stop, err := startSessionMCP(context.Background(), args, []string{})
	if err != nil {
		t.Fatal(err)
	}
	if manager == nil {
		t.Fatal("session manager not returned")
	}
	if args.mcpManager != nil {
		t.Fatal("session args mutated")
	}
	stop()
}
