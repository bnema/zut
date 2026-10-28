package agent

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/mcp"
	"github.com/bnema/zut/packages/agent/mcp/oauth"
)

func TestMCPAuthCommands(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	cfg := mcp.Config{Servers: map[string]mcp.ServerConfig{"remote": {URL: "https://example.invalid/mcp", Type: "http"}, "local": {Command: "synthetic-command"}}}
	if err := mcp.WriteConfigFile(mcp.GlobalConfigPath(home), cfg); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"login", "logout"} {
		for _, name := range []string{"missing", "local"} {
			var out bytes.Buffer
			err := mcpAuth(context.Background(), &out, cwd, action, []string{name})
			if err == nil {
				t.Fatalf("%s accepted %s", action, name)
			}
		}
	}
	store := oauth.Store{Path: filepath.Join(home, "mcp-auth.json")}
	key := "https://example.invalid/mcp"
	if err := store.Set(key, &oauth.Entry{AccessToken: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		var out bytes.Buffer
		if err := mcpAuth(context.Background(), &out, cwd, "logout", []string{"remote"}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "logged out") {
			t.Fatal(out.String())
		}
	}
	e, err := store.Get(key)
	if err != nil || e.AccessToken != "" {
		t.Fatal("logout retained token")
	}
}
