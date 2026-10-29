package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionMCPNoSignalHandlers(t *testing.T) {
	// Session startup must never touch process signal handling.
	src, err := os.ReadFile("mcp_runtime.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"signal.Notify", "os.Exit"} {
		if strings.Contains(string(src), banned) {
			t.Errorf("mcp_runtime.go uses %s", banned)
		}
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
