package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if err := store.Set(context.Background(), key, &oauth.Entry{AccessToken: "synthetic"}); err != nil {
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

func TestMCPLogoutHeldLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release }))
	defer srv.Close()
	defer close(release)
	key := srv.URL + "/mcp"
	cfg := mcp.Config{Servers: map[string]mcp.ServerConfig{"remote": {URL: key, Type: "http"}}}
	if err := mcp.WriteConfigFile(mcp.GlobalConfigPath(home), cfg); err != nil {
		t.Fatal(err)
	}
	store := oauth.Store{Path: filepath.Join(home, "mcp-auth.json")}
	if err := store.Set(context.Background(), key, &oauth.Entry{AccessToken: "old", RefreshToken: "refresh", Expiry: time.Now().Add(-time.Hour), TokenEndpoint: srv.URL}); err != nil {
		t.Fatal(err)
	}
	holderCtx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan struct{})
	go func() {
		req, _ := http.NewRequestWithContext(holderCtx, "GET", key, nil)
		resp, _ := oauth.HTTPClient(nil, store, key, "remote").Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("holder not ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := mcpAuth(ctx, io.Discard, t.TempDir(), "logout", []string{"remote"})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "locked by another zut process") {
		t.Fatalf("logout: %v", err)
	}
	stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("holder did not stop")
	}
}
