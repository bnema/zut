// Package mcp is zut's native Model Context Protocol client. It loads
// configured MCP servers, connects to them over stdio or streamable HTTP,
// and exposes their tools and resources as core.Tool values.
package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// DefaultStartupTimeout bounds process start, initialize, and the first
	// tools/list for one server.
	DefaultStartupTimeout = 30 * time.Second
	// DefaultToolTimeout bounds one tools/call request.
	DefaultToolTimeout = 300 * time.Second
)

// ServerConfig describes one entry below "mcpServers". Field names follow the
// Claude Desktop/Claude Code format; zut-specific options are additive.
type ServerConfig struct {
	// BaseEnv supplies the session environment; nil preserves process inheritance.
	BaseEnv []string `json:"-"`
	// Type selects the transport: "stdio" (default when command is set) or
	// "http" (default when url is set). "streamable-http" is an alias of
	// "http". "transport" is accepted as an alias key.
	Type      string `json:"type,omitempty"`
	Transport string `json:"transport,omitempty"`

	// stdio
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// EnvVars names extra host variables passed through to the process in
	// addition to the safe default allow-list.
	EnvVars []string `json:"envVars,omitempty"`
	CWD     string   `json:"cwd,omitempty"`

	// http
	URL               string            `json:"url,omitempty"`
	Headers           map[string]string `json:"headers,omitempty"`
	EnvHeaders        map[string]string `json:"envHeaders,omitempty"`
	BearerTokenEnvVar string            `json:"bearerTokenEnvVar,omitempty"`

	// common
	Enabled           *bool    `json:"enabled,omitempty"`
	Required          bool     `json:"required,omitempty"`
	EnabledTools      []string `json:"enabledTools,omitempty"`
	DisabledTools     []string `json:"disabledTools,omitempty"`
	StartupTimeoutSec float64  `json:"startupTimeoutSec,omitempty"`
	ToolTimeoutSec    float64  `json:"toolTimeoutSec,omitempty"`
}

// Config is the merged MCP configuration.
type Config struct {
	Servers map[string]ServerConfig `json:"mcpServers"`
}

// ConfigError reports one unusable server. Message never includes secret
// field values.
type ConfigError struct {
	Server  string
	Message string
}

func (e *ConfigError) Error() string { return fmt.Sprintf("mcp server %q: %s", e.Server, e.Message) }

// GlobalConfigPath is the user-level MCP configuration file.
func GlobalConfigPath(zutHome string) string { return filepath.Join(zutHome, "mcp.json") }

// ProjectConfigPath is the project-level MCP configuration file.
func ProjectConfigPath(cwd string) string { return filepath.Join(cwd, ".zut", "mcp.json") }

// LoadConfig merges the global and project files (project wins per server),
// expands ${VAR} references, and validates each server. Invalid servers are
// dropped and reported; a malformed file is a fatal error for that file only.
func LoadConfig(zutHome, cwd string, baseEnv ...[]string) (Config, []error) {
	cfg := Config{Servers: map[string]ServerConfig{}}
	var errs []error
	paths := []string{}
	if zutHome != "" {
		paths = append(paths, GlobalConfigPath(zutHome))
	}
	if cwd != "" {
		paths = append(paths, ProjectConfigPath(cwd))
	}
	for _, path := range paths {
		file, err := ReadConfigFile(path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		for name, srv := range file.Servers {
			cfg.Servers[name] = srv
		}
	}
	for _, name := range sortedNames(cfg.Servers) {
		srv := cfg.Servers[name]
		if len(baseEnv) > 0 {
			srv.BaseEnv = append([]string{}, baseEnv[0]...)
		}
		if srv.Enabled != nil && !*srv.Enabled {
			delete(cfg.Servers, name)
			continue
		}
		normalized, err := normalizeServer(name, srv)
		if err != nil {
			delete(cfg.Servers, name)
			errs = append(errs, err)
			continue
		}
		cfg.Servers[name] = normalized
	}
	return cfg, errs
}

// ReadConfigFile parses one MCP JSON file without expansion or validation.
func ReadConfigFile(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var file Config
	if err := json.Unmarshal(raw, &file); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if file.Servers == nil {
		file.Servers = map[string]ServerConfig{}
	}
	return file, nil
}

// WriteConfigFile atomically writes cfg to path with private permissions,
// because entries may contain header values or environment secrets.
func WriteConfigFile(path string, cfg Config) error {
	if cfg.Servers == nil {
		cfg.Servers = map[string]ServerConfig{}
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mcp-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Kind returns the normalized transport, "stdio" or "http".
func (s ServerConfig) Kind() string {
	kind := strings.ToLower(strings.TrimSpace(firstNonEmpty(s.Type, s.Transport)))
	switch kind {
	case "streamable-http", "streamable_http", "streamablehttp", "http":
		return "http"
	case "stdio":
		return "stdio"
	case "":
		if s.URL != "" && s.Command == "" {
			return "http"
		}
		return "stdio"
	default:
		return kind
	}
}

// StartupTimeout returns the configured or default startup timeout.
func (s ServerConfig) StartupTimeout() time.Duration {
	if s.StartupTimeoutSec > 0 {
		return time.Duration(s.StartupTimeoutSec * float64(time.Second))
	}
	return DefaultStartupTimeout
}

// ToolTimeout returns the configured or default tools/call timeout.
func (s ServerConfig) ToolTimeout() time.Duration {
	if s.ToolTimeoutSec > 0 {
		return time.Duration(s.ToolTimeoutSec * float64(time.Second))
	}
	return DefaultToolTimeout
}

// ToolAllowed applies enabledTools then disabledTools to a raw MCP tool name.
func (s ServerConfig) ToolAllowed(name string) bool {
	if len(s.EnabledTools) > 0 && !containsString(s.EnabledTools, name) {
		return false
	}
	return !containsString(s.DisabledTools, name)
}

func normalizeServer(name string, srv ServerConfig) (ServerConfig, error) {
	if strings.TrimSpace(name) == "" {
		return srv, &ConfigError{Server: name, Message: "empty server name"}
	}
	if err := expandServerEnv(name, &srv); err != nil {
		return srv, err
	}
	return ValidateServer(name, srv)
}

// ValidateServer checks one entry's transport shape without expanding
// ${VAR} references, so callers can store references unexpanded.
func ValidateServer(name string, srv ServerConfig) (ServerConfig, error) {
	if strings.TrimSpace(name) == "" {
		return srv, &ConfigError{Server: name, Message: "empty server name"}
	}
	switch srv.Kind() {
	case "stdio":
		if strings.TrimSpace(srv.Command) == "" {
			return srv, &ConfigError{Server: name, Message: "stdio server requires command"}
		}
		if srv.URL != "" || len(srv.Headers) > 0 || len(srv.EnvHeaders) > 0 || srv.BearerTokenEnvVar != "" {
			return srv, &ConfigError{Server: name, Message: "url, headers, envHeaders, and bearerTokenEnvVar apply only to http servers"}
		}
	case "http":
		if strings.TrimSpace(srv.URL) == "" {
			return srv, &ConfigError{Server: name, Message: "http server requires url"}
		}
		if !strings.HasPrefix(srv.URL, "http://") && !strings.HasPrefix(srv.URL, "https://") && !strings.HasPrefix(srv.URL, "${") {
			return srv, &ConfigError{Server: name, Message: "url must start with http:// or https://"}
		}
		if srv.Command != "" || len(srv.Args) > 0 || len(srv.Env) > 0 || len(srv.EnvVars) > 0 || srv.CWD != "" {
			return srv, &ConfigError{Server: name, Message: "command, args, env, envVars, and cwd apply only to stdio servers"}
		}
	case "sse":
		return srv, &ConfigError{Server: name, Message: "legacy sse transport is not supported; use the server's streamable http endpoint"}
	default:
		return srv, &ConfigError{Server: name, Message: fmt.Sprintf("unknown transport %q", srv.Kind())}
	}
	return srv, nil
}

// envReference matches Claude Code's braced syntax only: ${VAR} and
// ${VAR:-default}. Bare $VAR and shell expressions stay literal.
var envReference = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

func expandServerEnv(name string, srv *ServerConfig) error {
	var missing *ConfigError
	expand := func(field, value string) string {
		return envReference.ReplaceAllStringFunc(value, func(ref string) string {
			m := envReference.FindStringSubmatch(ref)
			if v, ok := lookupBaseEnv(srv.BaseEnv, m[1]); ok {
				return v
			}
			if m[2] != "" {
				return m[3]
			}
			if missing == nil {
				missing = &ConfigError{Server: name, Message: fmt.Sprintf("%s references unset variable %s", field, m[1])}
			}
			return ""
		})
	}
	srv.Command = expand("command", srv.Command)
	if len(srv.Args) > 0 {
		args := make([]string, len(srv.Args))
		for i, arg := range srv.Args {
			args[i] = expand(fmt.Sprintf("args[%d]", i), arg)
		}
		srv.Args = args
	}
	srv.CWD = expand("cwd", srv.CWD)
	srv.URL = expand("url", srv.URL)
	srv.Env = expandMap("env", srv.Env, expand)
	srv.Headers = expandMap("headers", srv.Headers, expand)
	if missing != nil {
		return missing
	}
	return nil
}

func expandMap(field string, in map[string]string, expand func(string, string) string) map[string]string {
	if len(in) == 0 {
		return in
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = expand(field+"."+k, v)
	}
	return out
}

func sortedNames[T any](m map[string]T) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// lookupBaseEnv preserves process behavior only when no base was supplied.
func lookupBaseEnv(base []string, key string) (string, bool) {
	if base == nil {
		return os.LookupEnv(key)
	}
	for i := len(base) - 1; i >= 0; i-- {
		k, v, ok := strings.Cut(base[i], "=")
		if ok && k == key {
			return v, true
		}
	}
	return "", false
}
