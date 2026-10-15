package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfigMergeProjectWins(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	writeFile(t, GlobalConfigPath(home), `{"mcpServers":{
		"shared":{"command":"global-cmd"},
		"only-global":{"command":"g"}}}`)
	writeFile(t, ProjectConfigPath(cwd), `{"mcpServers":{
		"shared":{"command":"project-cmd","args":["x"]},
		"only-project":{"url":"http://127.0.0.1:1/mcp"}}}`)

	cfg, errs := LoadConfig(home, cwd)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if got := sortedNames(cfg.Servers); !reflect.DeepEqual(got, []string{"only-global", "only-project", "shared"}) {
		t.Fatalf("servers = %v", got)
	}
	shared := cfg.Servers["shared"]
	if shared.Command != "project-cmd" || !reflect.DeepEqual(shared.Args, []string{"x"}) {
		t.Errorf("project did not win: %+v", shared)
	}
	if cfg.Servers["only-project"].Kind() != "http" {
		t.Errorf("url-only server should be http")
	}
}

func TestLoadConfigMissingFilesAndMalformed(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	cfg, errs := LoadConfig(home, cwd)
	if len(errs) != 0 || len(cfg.Servers) != 0 {
		t.Fatalf("missing files: cfg=%v errs=%v", cfg, errs)
	}
	writeFile(t, GlobalConfigPath(home), `{bad json`)
	writeFile(t, ProjectConfigPath(cwd), `{"mcpServers":{"ok":{"command":"c"}}}`)
	cfg, errs = LoadConfig(home, cwd)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "parse") {
		t.Fatalf("want one parse error, got %v", errs)
	}
	if _, ok := cfg.Servers["ok"]; !ok {
		t.Errorf("malformed global file must not hide the project file: %v", cfg.Servers)
	}
}

func TestLoadConfigEnvExpansion(t *testing.T) {
	t.Setenv("MCP_T_CMD", "/usr/bin/tool")
	t.Setenv("MCP_T_HOST", "127.0.0.1:9")
	t.Setenv("MCP_T_TOKEN", "tok-123")
	t.Setenv("MCP_T_EMPTY", "")
	os.Unsetenv("MCP_T_UNSET")
	home := t.TempDir()
	writeFile(t, GlobalConfigPath(home), `{"mcpServers":{
		"s":{"command":"${MCP_T_CMD}","args":["--a=${MCP_T_UNSET:-fallback}","--b=${MCP_T_EMPTY:-notused}","$BARE","${MCP_T_CMD}/x"],
			"cwd":"${MCP_T_UNSET:-.}","env":{"K":"v-${MCP_T_TOKEN}","D":"${MCP_T_UNSET:-}"}},
		"h":{"url":"http://${MCP_T_HOST}/mcp","headers":{"X-Key":"${MCP_T_TOKEN}","X-D":"${MCP_T_UNSET:-dflt}"}}}}`)

	cfg, errs := LoadConfig(home, "")
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	s := cfg.Servers["s"]
	if s.Command != "/usr/bin/tool" {
		t.Errorf("command = %q", s.Command)
	}
	wantArgs := []string{"--a=fallback", "--b=", "$BARE", "/usr/bin/tool/x"}
	if !reflect.DeepEqual(s.Args, wantArgs) {
		t.Errorf("args = %q, want %q", s.Args, wantArgs)
	}
	if s.CWD != "." || s.Env["K"] != "v-tok-123" || s.Env["D"] != "" {
		t.Errorf("cwd/env wrong: %+v", s)
	}
	h := cfg.Servers["h"]
	if h.URL != "http://127.0.0.1:9/mcp" || h.Headers["X-Key"] != "tok-123" || h.Headers["X-D"] != "dflt" {
		t.Errorf("http expansion wrong: %+v", h)
	}
}

func TestLoadConfigMissingVarDropsOnlyThatServerWithoutSecrets(t *testing.T) {
	const secret = "super-secret-value-9f3"
	t.Setenv("MCP_T_PRESENT", secret)
	os.Unsetenv("MCP_T_ABSENT")
	home := t.TempDir()
	writeFile(t, GlobalConfigPath(home), `{"mcpServers":{
		"bad":{"command":"c","env":{"TOKEN":"${MCP_T_PRESENT}-${MCP_T_ABSENT}"}},
		"good":{"command":"c","env":{"TOKEN":"${MCP_T_PRESENT}"}}}}`)
	cfg, errs := LoadConfig(home, "")
	if _, ok := cfg.Servers["bad"]; ok {
		t.Error("server with a missing variable should be dropped")
	}
	if cfg.Servers["good"].Env["TOKEN"] != secret {
		t.Error("other servers must be kept")
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v", errs)
	}
	msg := errs[0].Error()
	if strings.Contains(msg, secret) {
		t.Errorf("error leaks a secret value: %s", msg)
	}
	if !strings.Contains(msg, "MCP_T_ABSENT") || !strings.Contains(msg, `"bad"`) {
		t.Errorf("error should name the server and variable: %s", msg)
	}
	var ce *ConfigError
	if e, ok := errs[0].(*ConfigError); !ok {
		t.Errorf("want *ConfigError, got %T", errs[0])
	} else {
		ce = e
		if ce.Server != "bad" {
			t.Errorf("ConfigError.Server = %q", ce.Server)
		}
	}
}

func TestLoadConfigDisabledSkipped(t *testing.T) {
	home := t.TempDir()
	writeFile(t, GlobalConfigPath(home), `{"mcpServers":{
		"off":{"command":"c","enabled":false},
		"on":{"command":"c","enabled":true},
		"off-invalid":{"enabled":false}}}`)
	cfg, errs := LoadConfig(home, "")
	if len(errs) != 0 {
		t.Fatalf("disabled servers must not produce errors: %v", errs)
	}
	if got := sortedNames(cfg.Servers); !reflect.DeepEqual(got, []string{"on"}) {
		t.Fatalf("servers = %v", got)
	}
}

func TestLoadConfigProjectDisablesGlobalServer(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	writeFile(t, GlobalConfigPath(home), `{"mcpServers":{"s":{"command":"c"}}}`)
	writeFile(t, ProjectConfigPath(cwd), `{"mcpServers":{"s":{"command":"c","enabled":false}}}`)
	cfg, errs := LoadConfig(home, cwd)
	if len(errs) != 0 || len(cfg.Servers) != 0 {
		t.Fatalf("cfg=%v errs=%v", cfg.Servers, errs)
	}
}

func TestLoadConfigInvalidShapes(t *testing.T) {
	cases := map[string]struct {
		body string
		want string
	}{
		"stdio-no-command":   {`{"command":"  "}`, "requires command"},
		"empty":              {`{}`, "requires command"},
		"typed-stdio-no-cmd": {`{"type":"stdio","url":"http://x"}`, "requires command"},
		"http-no-url":        {`{"type":"http"}`, "requires url"},
		"sse":                {`{"type":"sse","url":"http://127.0.0.1/sse"}`, "sse"},
		"sse-transport":      {`{"transport":"sse","url":"http://127.0.0.1/sse"}`, "sse"},
		"unknown":            {`{"type":"carrier-pigeon","command":"c"}`, "unknown transport"},
		"bad-scheme":         {`{"url":"ftp://x/y"}`, "http:// or https://"},
		"stdio-with-url":     {`{"command":"c","url":"http://x"}`, "only to http"},
		"stdio-with-header":  {`{"type":"stdio","command":"c","headers":{"A":"b"}}`, "only to http"},
		"stdio-with-bearer":  {`{"command":"c","bearerTokenEnvVar":"X"}`, "only to http"},
		"http-with-args":     {`{"type":"http","url":"http://x","args":["a"]}`, "only to stdio"},
		"http-with-env":      {`{"url":"http://x","env":{"A":"b"}}`, "only to stdio"},
		"http-with-cwd":      {`{"url":"http://x","cwd":"/tmp"}`, "only to stdio"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			writeFile(t, GlobalConfigPath(home), `{"mcpServers":{"bad":`+c.body+`,"fine":{"command":"c"}}}`)
			cfg, errs := LoadConfig(home, "")
			if _, ok := cfg.Servers["bad"]; ok {
				t.Error("invalid server should be dropped")
			}
			if _, ok := cfg.Servers["fine"]; !ok {
				t.Error("valid sibling should be kept")
			}
			if len(errs) != 1 || !strings.Contains(errs[0].Error(), c.want) {
				t.Errorf("errs = %v, want substring %q", errs, c.want)
			}
		})
	}
}

func TestLoadConfigTransportAliases(t *testing.T) {
	home := t.TempDir()
	writeFile(t, GlobalConfigPath(home), `{"mcpServers":{
		"a":{"type":"streamable-http","url":"http://127.0.0.1:1"},
		"b":{"transport":"http","url":"https://example.invalid/mcp"},
		"c":{"type":"stdio","command":"x"}}}`)
	cfg, errs := LoadConfig(home, "")
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	for name, want := range map[string]string{"a": "http", "b": "http", "c": "stdio"} {
		if got := cfg.Servers[name].Kind(); got != want {
			t.Errorf("%s kind = %q, want %q", name, got, want)
		}
	}
}

func TestValidateServerAllowsUnexpandedURL(t *testing.T) {
	if _, err := ValidateServer("s", ServerConfig{URL: "${MCP_URL}"}); err != nil {
		t.Errorf("unexpanded url reference should validate: %v", err)
	}
	if _, err := ValidateServer("s", ServerConfig{Type: "http", URL: "nope"}); err == nil {
		t.Error("non-http url should be rejected")
	}
	if _, err := ValidateServer(" ", ServerConfig{Command: "c"}); err == nil {
		t.Error("empty name should be rejected")
	}
}

func TestServerConfigHelpers(t *testing.T) {
	var c ServerConfig
	if c.StartupTimeout() != DefaultStartupTimeout || c.ToolTimeout() != DefaultToolTimeout {
		t.Error("defaults wrong")
	}
	c = ServerConfig{StartupTimeoutSec: 1.5, ToolTimeoutSec: 0.25}
	if c.StartupTimeout() != 1500*time.Millisecond || c.ToolTimeout() != 250*time.Millisecond {
		t.Errorf("custom timeouts wrong: %v %v", c.StartupTimeout(), c.ToolTimeout())
	}
	c = ServerConfig{EnabledTools: []string{"a", "b"}, DisabledTools: []string{"b"}}
	for name, want := range map[string]bool{"a": true, "b": false, "c": false} {
		if got := c.ToolAllowed(name); got != want {
			t.Errorf("ToolAllowed(%q) = %v, want %v", name, got, want)
		}
	}
	if !(ServerConfig{}).ToolAllowed("anything") {
		t.Error("no filters should allow everything")
	}
	if !(ServerConfig{DisabledTools: []string{"x"}}).ToolAllowed("y") {
		t.Error("disabledTools only blocks listed names")
	}
}

func TestWriteConfigFileRoundTripAndPerms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "mcp.json")
	on := true
	want := Config{Servers: map[string]ServerConfig{
		"s": {Command: "cmd", Args: []string{"a", "b"}, Env: map[string]string{"K": "${V}"}, EnvVars: []string{"HOME"},
			Required: true, EnabledTools: []string{"x"}, StartupTimeoutSec: 2, ToolTimeoutSec: 3, Enabled: &on},
		"h": {Type: "http", URL: "https://example.invalid", Headers: map[string]string{"A": "b"},
			EnvHeaders: map[string]string{"X": "Y"}, BearerTokenEnvVar: "TOK"},
	}}
	if err := WriteConfigFile(path, want); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Errorf("perm = %o, want 600", perm)
		}
	}
	got, err := ReadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(want)
		t.Errorf("round trip mismatch:\n got %s\nwant %s", gj, wj)
	}
	// Overwrite replaces atomically and leaves no temp files behind.
	if err := WriteConfigFile(path, Config{}); err != nil {
		t.Fatal(err)
	}
	got, err = ReadConfigFile(path)
	if err != nil || len(got.Servers) != 0 || got.Servers == nil {
		t.Errorf("empty config: %v %v", got, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("leftover files in config dir: %v", entries)
	}
}

func TestBuildStdioEnv(t *testing.T) {
	t.Setenv("SECRET_API_KEY", "do-not-leak")
	t.Setenv("MCP_T_EXTRA", "passed")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("MCP_T_OVERRIDE", "host")
	env := buildStdioEnv(ServerConfig{
		EnvVars: []string{"MCP_T_EXTRA", "MCP_T_NOT_SET", "MCP_T_OVERRIDE"},
		Env:     map[string]string{"EXPLICIT": "yes", "MCP_T_OVERRIDE": "cfg"},
	})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "SECRET_API_KEY") || strings.Contains(joined, "do-not-leak") {
		t.Errorf("non-allow-listed host variable leaked: %v", env)
	}
	want := map[string]bool{"PATH=/usr/bin:/bin": false, "MCP_T_EXTRA=passed": false, "EXPLICIT=yes": false, "MCP_T_OVERRIDE=cfg": false}
	for _, e := range env {
		if _, ok := want[e]; ok {
			want[e] = true
		}
		if strings.HasPrefix(e, "MCP_T_NOT_SET") {
			t.Errorf("unset variable should not be passed: %s", e)
		}
		if e == "MCP_T_OVERRIDE=host" {
			t.Errorf("cfg.Env must override host value")
		}
	}
	for e, ok := range want {
		if !ok {
			t.Errorf("missing %q in %v", e, env)
		}
	}
	// Deterministic order.
	for i := 1; i < len(env); i++ {
		if env[i-1] > env[i] {
			t.Errorf("env not sorted: %v", env)
			break
		}
	}
}
