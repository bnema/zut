package mcp

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// keyCfg is a stdio config whose command resolves to this test binary.
func keyCfg(t *testing.T) ServerConfig {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return ServerConfig{Command: exe, Args: []string{"-x"}, Env: map[string]string{"A": "1"}, BaseEnv: []string{"PATH=" + os.Getenv("PATH")}}
}

func mustKey(t *testing.T, cfg ServerConfig, cwd string) (poolKey, string) {
	t.Helper()
	k, roots, err := sharedKey(cfg, cwd)
	if err != nil {
		t.Fatal(err)
	}
	return k, roots
}

func TestSharedKeyStableAndSensitive(t *testing.T) {
	cwd := t.TempDir()
	base := keyCfg(t)
	k1, roots := mustKey(t, base, cwd)
	k2, _ := mustKey(t, keyCfg(t), cwd)
	if k1 != k2 {
		t.Fatal("same config and cwd must give the same key")
	}
	if roots != cwd {
		t.Fatalf("rootsCWD = %q, want session cwd", roots)
	}

	other, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "other-cmd")
	if err := os.Symlink(other, link); err != nil {
		t.Skip("symlink unsupported:", err)
	}
	variants := map[string]func(*ServerConfig) string{
		"env value":                func(c *ServerConfig) string { c.Env = map[string]string{"A": "2"}; return cwd },
		"args":                     func(c *ServerConfig) string { c.Args = []string{"-y"}; return cwd },
		"command":                  func(c *ServerConfig) string { c.Command = link; return cwd },
		"empty cwd, other session": func(c *ServerConfig) string { return t.TempDir() },
		"relative cwd, other session": func(c *ServerConfig) string {
			c.CWD = "sub"
			return t.TempDir()
		},
	}
	for name, mutate := range variants {
		t.Run(name, func(t *testing.T) {
			cfg := keyCfg(t)
			sessionCWD := mutate(&cfg)
			k, _ := mustKey(t, cfg, sessionCWD)
			ref := k1
			if name == "relative cwd, other session" {
				rel := keyCfg(t)
				rel.CWD = "sub"
				ref, _ = mustKey(t, rel, cwd)
			}
			if k == ref {
				t.Fatalf("%s must change the key", name)
			}
		})
	}
}

func TestSharedKeyIgnoresSessionViewFields(t *testing.T) {
	cwd := t.TempDir()
	k1, _ := mustKey(t, keyCfg(t), cwd)
	cfg := keyCfg(t)
	cfg.Required = true
	cfg.EnabledTools = []string{"a"}
	cfg.DisabledTools = []string{"b"}
	cfg.StartupTimeoutSec, cfg.ToolTimeoutSec = 3, 4
	k2, _ := mustKey(t, cfg, cwd)
	if k1 != k2 {
		t.Fatal("per-session view fields must not change the key")
	}
}

func TestSharedKeyAbsoluteCWDKeepsSessionRoots(t *testing.T) {
	cfg := keyCfg(t)
	cfg.CWD = t.TempDir()
	session := t.TempDir()
	k1, r1 := mustKey(t, cfg, session)
	k2, _ := mustKey(t, cfg, session)
	if k1 != k2 {
		t.Fatal("same session cwd must give the same key")
	}
	if r1 != session {
		t.Fatalf("rootsCWD = %q, want session cwd %q", r1, session)
	}
	other := t.TempDir()
	k3, r3 := mustKey(t, cfg, other)
	if k3 == k1 {
		t.Fatal("different session cwd must change the key even with absolute cwd")
	}
	if r3 != other {
		t.Fatalf("rootsCWD = %q, want %q", r3, other)
	}
}

func TestSharedKeyRejectsHTTP(t *testing.T) {
	if _, _, err := sharedKey(ServerConfig{URL: "https://example.invalid/mcp"}, t.TempDir()); err == nil {
		t.Fatal("http servers must not produce a shared key")
	}
}

func TestShareEnabled(t *testing.T) {
	off, on := false, true
	for _, tc := range []struct {
		name string
		cfg  ServerConfig
		want bool
	}{
		{"stdio default", ServerConfig{Command: "x"}, true},
		{"stdio shared:true", ServerConfig{Command: "x", Shared: &on}, true},
		{"stdio shared:false", ServerConfig{Command: "x", Shared: &off}, false},
		{"http default", ServerConfig{URL: "https://example.invalid"}, false},
		{"http shared:false", ServerConfig{URL: "https://example.invalid", Shared: &off}, false},
	} {
		if got := tc.cfg.ShareEnabled(); got != tc.want {
			t.Errorf("%s: ShareEnabled = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestValidateServerSharedHTTP(t *testing.T) {
	on, off := true, false
	_, err := ValidateServer("h", ServerConfig{URL: "https://example.invalid", Shared: &on})
	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) || cfgErr.Message != "shared applies only to stdio servers" {
		t.Fatalf("err = %v", err)
	}
	if _, err := ValidateServer("h", ServerConfig{URL: "https://example.invalid", Shared: &off}); err != nil {
		t.Fatalf("shared:false on http must be accepted: %v", err)
	}
	if _, err := ValidateServer("s", ServerConfig{Command: "x", Shared: &on}); err != nil {
		t.Fatalf("shared:true on stdio must be accepted: %v", err)
	}
}
