package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/bnema/zut/packages/agent/mcp"
	"github.com/bnema/zut/packages/agent/mcp/oauth"
	"github.com/bnema/zut/packages/provider"
)

var mcpCommandStderr io.Writer = os.Stderr

// runMCPCommand dispatches `zut mcp ...`. It returns handled=false when
// rawArgs does not start with "mcp".
func runMCPCommand(ctx context.Context, rawArgs []string, version string, out io.Writer) (bool, error) {
	if len(rawArgs) == 0 || rawArgs[0] != "mcp" {
		return false, nil
	}
	if len(rawArgs) == 1 {
		printMCPHelp(out)
		return true, nil
	}
	cwd, _ := os.Getwd()
	rest := rawArgs[2:]
	switch rawArgs[1] {
	case "login", "logout":
		return true, mcpAuth(ctx, out, cwd, rawArgs[1], rest)
	case "list", "ls":
		return true, mcpList(out, cwd, rest)
	case "get":
		return true, mcpGet(out, cwd, rest)
	case "add":
		return true, mcpAdd(out, cwd, rest)
	case "remove", "rm":
		return true, mcpRemove(out, cwd, rest)
	case "status", "test":
		return true, mcpTest(ctx, out, cwd, version, rest)
	case "-h", "--help", "help":
		printMCPHelp(out)
		return true, nil
	default:
		printMCPHelp(out)
		return true, fmt.Errorf("unknown mcp subcommand %q", rawArgs[1])
	}
}

func printMCPHelp(out io.Writer) {
	fmt.Fprint(out, `zut mcp: manage MCP servers

  zut mcp login NAME                       sign in to an HTTP server
  zut mcp logout NAME                      remove stored OAuth credentials
  zut mcp list [--json]                     list configured servers
  zut mcp get NAME [--json]                 show one server (secret values hidden)
  zut mcp add NAME [--project] [opts] -- COMMAND [ARGS...]
                                            add a stdio server
  zut mcp add NAME [--project] --url URL [opts]
                                            add a streamable HTTP server
  zut mcp remove NAME [--project]           remove a server
  zut mcp status [NAME]                     connect and list tools (server stderr is shown live)

add options:
  --env KEY=VALUE            stdio environment variable (repeatable)
  --env-var NAME             pass a host variable through (repeatable)
  --header KEY=VALUE         HTTP header (repeatable)
  --bearer-token-env-var V   send "Authorization: Bearer $V"
  --startup-timeout SEC      startup timeout (default 30)
  --tool-timeout SEC         tools/call timeout (default 300)
  --required                 fail startup when this server cannot start

Global servers live in $ZUT_HOME/mcp.json; --project writes .zut/mcp.json.
`)
}

func mcpConfigPath(cwd string, project bool) string {
	if project {
		return mcp.ProjectConfigPath(cwd)
	}
	return mcp.GlobalConfigPath(ZutHome())
}

// rawMCPConfig merges the global and project files without ${VAR} expansion
// or validation, so list/get show disabled and currently broken entries and
// never print expanded secret values.
func rawMCPConfig(cwd string) (mcp.Config, error) {
	cfg := mcp.Config{Servers: map[string]mcp.ServerConfig{}}
	var errs []error
	for _, path := range []string{mcp.GlobalConfigPath(ZutHome()), mcp.ProjectConfigPath(cwd)} {
		file, err := mcp.ReadConfigFile(path)
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
	return cfg, errors.Join(errs...)
}

func mcpList(out io.Writer, cwd string, args []string) error {
	asJSON := len(args) > 0 && args[0] == "--json"
	cfg, cfgErr := rawMCPConfig(cwd)
	if cfgErr != nil {
		return cfgErr
	}
	if asJSON {
		redacted := map[string]mcp.ServerConfig{}
		for name, srv := range cfg.Servers {
			redacted[name] = redactServer(srv)
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"mcpServers": redacted})
	}
	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		fmt.Fprintln(out, "no MCP servers configured")
	}
	for _, name := range names {
		srv := redactServer(cfg.Servers[name])
		target := srv.URL
		if srv.Kind() == "stdio" {
			target = strings.TrimSpace(srv.Command + " " + strings.Join(srv.Args, " "))
		}
		state := ""
		if srv.Enabled != nil && !*srv.Enabled {
			state = "\tdisabled"
		}
		fmt.Fprintf(out, "%s\t%s\t%s%s\n", name, srv.Kind(), target, state)
	}
	return nil
}

// redactServer hides values that commonly carry secrets: env and header
// values, URL credentials and query strings. Arguments are shown as written
// in the file (${VAR} references stay unexpanded).
func redactServer(srv mcp.ServerConfig) mcp.ServerConfig {
	srv.URL = redactURL(srv.URL)
	hide := func(m map[string]string) map[string]string {
		if len(m) == 0 {
			return m
		}
		out := make(map[string]string, len(m))
		for k := range m {
			out[k] = "*****"
		}
		return out
	}
	srv.Env = hide(srv.Env)
	srv.Headers = hide(srv.Headers)
	return srv
}

// redactURL hides userinfo, query, and fragment with string operations so an
// unexpanded ${VAR} template that url.Parse rejects cannot fail open.
func redactURL(raw string) string {
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i] + "?*****"
	}
	schemeEnd := strings.Index(raw, "://")
	start := 0
	if schemeEnd >= 0 {
		start = schemeEnd + 3
	}
	authorityEnd := len(raw)
	if i := strings.IndexByte(raw[start:], '/'); i >= 0 {
		authorityEnd = start + i
	}
	if at := strings.LastIndexByte(raw[start:authorityEnd], '@'); at >= 0 {
		raw = raw[:start] + "*****@" + raw[start+at+1:]
	}
	return raw
}

func mcpGet(out io.Writer, cwd string, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: zut mcp get NAME [--json]")
	}
	cfg, err := rawMCPConfig(cwd)
	if err != nil {
		return err
	}
	srv, ok := cfg.Servers[args[0]]
	if !ok {
		return fmt.Errorf("mcp server %q is not configured", args[0])
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(redactServer(srv))
}

func mcpAdd(out io.Writer, cwd string, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: zut mcp add NAME [options] (-- COMMAND [ARGS...] | --url URL)")
	}
	name := args[0]
	var (
		srv     mcp.ServerConfig
		project bool
	)
	keyValue := func(flag, v string) (string, string, error) {
		k, val, ok := strings.Cut(v, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return "", "", fmt.Errorf("%s expects KEY=VALUE", flag)
		}
		return k, val, nil
	}
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		value := func() (string, error) {
			if i+1 >= len(rest) {
				return "", fmt.Errorf("%s requires a value", arg)
			}
			i++
			return rest[i], nil
		}
		var (
			v   string
			err error
		)
		switch arg {
		case "--":
			cmd := rest[i+1:]
			if len(cmd) == 0 {
				return errors.New("missing command after --")
			}
			srv.Command, srv.Args = cmd[0], append([]string(nil), cmd[1:]...)
			i = len(rest)
			continue
		case "--project":
			project = true
			continue
		case "--global":
			project = false
			continue
		case "--required":
			srv.Required = true
			continue
		}
		if v, err = value(); err != nil {
			return err
		}
		switch arg {
		case "--url":
			srv.URL = v
		case "--env":
			k, val, err := keyValue(arg, v)
			if err != nil {
				return err
			}
			if srv.Env == nil {
				srv.Env = map[string]string{}
			}
			srv.Env[k] = val
		case "--env-var":
			srv.EnvVars = append(srv.EnvVars, v)
		case "--header":
			k, val, err := keyValue(arg, v)
			if err != nil {
				return err
			}
			if srv.Headers == nil {
				srv.Headers = map[string]string{}
			}
			srv.Headers[k] = val
		case "--bearer-token-env-var":
			srv.BearerTokenEnvVar = v
		case "--startup-timeout", "--tool-timeout":
			sec, err := strconv.ParseFloat(v, 64)
			if err != nil || sec <= 0 {
				return fmt.Errorf("%s expects a positive number of seconds", arg)
			}
			if arg == "--startup-timeout" {
				srv.StartupTimeoutSec = sec
			} else {
				srv.ToolTimeoutSec = sec
			}
		default:
			return fmt.Errorf("unknown option %q", arg)
		}
	}
	if (srv.Command == "") == (srv.URL == "") {
		return errors.New("give exactly one of -- COMMAND or --url URL")
	}
	if srv.URL != "" {
		srv.Type = "http"
	}
	if _, err := mcp.ValidateServer(name, srv); err != nil {
		return err
	}
	path := mcpConfigPath(cwd, project)
	file, err := mcp.ReadConfigFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		file, err = mcp.Config{Servers: map[string]mcp.ServerConfig{}}, nil
	}
	if err != nil {
		return err
	}
	_, existed := file.Servers[name]
	file.Servers[name] = srv
	if err := mcp.WriteConfigFile(path, file); err != nil {
		return err
	}
	verb := "added"
	if existed {
		verb = "updated"
	}
	fmt.Fprintf(out, "%s mcp server %q in %s\n", verb, name, path)
	return nil
}

func mcpRemove(out io.Writer, cwd string, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: zut mcp remove NAME [--project]")
	}
	project := len(args) > 1 && args[1] == "--project"
	path := mcpConfigPath(cwd, project)
	file, err := mcp.ReadConfigFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("mcp server %q not found in %s", args[0], path)
		}
		return err
	}
	if _, ok := file.Servers[args[0]]; !ok {
		return fmt.Errorf("mcp server %q not found in %s", args[0], path)
	}
	delete(file.Servers, args[0])
	if err := mcp.WriteConfigFile(path, file); err != nil {
		return err
	}
	fmt.Fprintf(out, "removed mcp server %q from %s\n", args[0], path)
	return nil
}

func mcpAuth(ctx context.Context, out io.Writer, cwd, action string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: zut mcp %s NAME", action)
	}
	cfg, _ := mcp.LoadConfig(ZutHome(), cwd)
	srv, ok := cfg.Servers[args[0]]
	if !ok {
		return fmt.Errorf("mcp server %q is not configured, disabled, or invalid", args[0])
	}
	if srv.Kind() != "http" {
		return errors.New("OAuth login/logout requires an HTTP MCP server")
	}
	key, err := oauth.Canonical(srv.URL)
	if err != nil {
		return err
	}
	store := oauth.Store{Path: filepath.Join(ZutHome(), "mcp-auth.json")}
	if action == "login" {
		err = oauth.Login(ctx, store, srv.URL, mcpCommandStderr, nil)
	} else {
		err = store.Set(key, nil)
	}
	if err != nil {
		return err
	}
	if action == "login" {
		fmt.Fprintf(out, "signed in to mcp server %q\n", args[0])
	} else {
		fmt.Fprintf(out, "logged out of mcp server %q (no stored credentials remain)\n", args[0])
	}
	return nil
}

func mcpTest(ctx context.Context, out io.Writer, cwd, version string, args []string) error {
	cfg, errs := mcp.LoadConfig(ZutHome(), cwd)
	for _, err := range errs {
		fmt.Fprintln(mcpCommandStderr, "mcp config error:", err)
	}
	if len(args) > 0 {
		srv, ok := cfg.Servers[args[0]]
		if !ok {
			return fmt.Errorf("mcp server %q is not configured, disabled, or invalid", args[0])
		}
		cfg.Servers = map[string]mcp.ServerConfig{args[0]: srv}
	}
	manager := mcp.NewManager(cfg, cwd, mcp.ClientInfo{Name: "zut", Version: version}, provider.NewHTTPClient(false))
	defer manager.Close()
	manager.SetAuthPath(filepath.Join(ZutHome(), "mcp-auth.json"))
	manager.SetStderrWriter(mcpCommandStderr)
	failures, _ := manager.Start(ctx)
	fmt.Fprintln(out, mcp.FormatStatus(manager.Status()))
	// Names come from the registry builder so collision suffixes match what
	// --tools and the model see.
	for _, tool := range manager.Tools() {
		fmt.Fprintf(out, "  %s\n", tool.Name())
	}
	if n := len(failures) + len(errs); n > 0 {
		return fmt.Errorf("%d MCP configuration or startup problem(s)", n)
	}
	return nil
}
