package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPerSessionStdioEnvironment(t *testing.T) {
	t.Setenv("HOME", "host-home")
	got := buildStdioEnv(ServerConfig{BaseEnv: []string{"HOME=session-home", "SECRET=hidden"}, Env: map[string]string{"LOCAL": "value"}})
	joined := strings.Join(got, "|")
	if strings.Contains(joined, "host-home") || strings.Contains(joined, "SECRET") || !strings.Contains(joined, "HOME=session-home") {
		t.Fatalf("environment = %v", got)
	}
	empty := buildStdioEnv(ServerConfig{BaseEnv: []string{}})
	if len(empty) != 0 {
		t.Fatalf("empty session inherited environment: %v", empty)
	}
}

func TestSessionCredentialsRedacted(t *testing.T) {
	t.Setenv("L2_HEADER", "different-process-secret")
	secrets := []string{"session-header-secret", "session-env-secret", "session-bearer-secret"}
	for _, rpc := range []bool{false, true} {
		t.Run(fmt.Sprint(rpc), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+secrets[2] || r.Header.Get("X-Secret") != secrets[0] {
					t.Error("session credentials not sent")
				}
				if rpc {
					_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32000, "message": strings.Join(secrets, " ")}})
				} else {
					w.WriteHeader(500)
					fmt.Fprint(w, strings.Join(secrets, " "))
				}
			}))
			defer srv.Close()
			cfg := ServerConfig{URL: srv.URL, BaseEnv: []string{"L2_HEADER=" + secrets[0], "L2_ENV=" + secrets[1], "L2_BEARER=" + secrets[2]}, EnvHeaders: map[string]string{"X-Secret": "L2_HEADER"}, EnvVars: []string{"L2_ENV"}, BearerTokenEnvVar: "L2_BEARER"}
			tr, err := newHTTPTransport("session", cfg, srv.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = tr.request(context.Background(), 1, "initialize", map[string]any{})
			if err == nil {
				t.Fatal("expected diagnostic")
			}
			for _, secret := range secrets {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("secret leaked: %v", err)
				}
			}
			if !strings.Contains(err.Error(), redactedMarker) {
				t.Fatalf("missing redaction: %v", err)
			}
		})
	}
}

func TestSessionStdioExecutablePATH(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	session := filepath.Join(cwd, "bin")
	host := t.TempDir()
	if err := os.Mkdir(session, 0700); err != nil {
		t.Fatal(err)
	}
	name := "l2-server"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	for _, dir := range []string{session, host} {
		in, err := os.Open(bin)
		if err != nil {
			t.Fatal(err)
		}
		out, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY, 0700)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(out, in)
		in.Close()
		out.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", host)
	cfg := ServerConfig{Command: name, Args: []string{"-test.run=^$"}, BaseEnv: []string{"PATH=bin"}}
	tr, err := startStdio("session", cfg, cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr.cmd.Path != filepath.Join(session, name) {
		t.Fatalf("path = %q", tr.cmd.Path)
	}
	tr.close()
	cfg.BaseEnv = []string{}
	if _, err := startStdio("empty", cfg, cwd, nil); err == nil {
		t.Fatal("empty env used host PATH")
	}
}
