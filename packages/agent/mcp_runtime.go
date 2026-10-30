package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bnema/zut/packages/agent/mcp"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// mcpToolSelector is the --tools entry that enables every MCP tool.
const mcpToolSelector = "mcp"

// mcpAllowed reports whether MCP servers may start for args. Packaged agents
// (PermissionSet) fail closed because their manifest cannot describe what an
// external MCP server may do.
func mcpAllowed(args Args) bool {
	if args.NoMCP || args.NoTools || args.PermissionSet != nil {
		return false
	}
	if args.ToolsSet && len(args.Tools) == 0 {
		return false
	}
	if len(args.Tools) == 0 {
		return true
	}
	for _, name := range args.Tools {
		if name == mcpToolSelector || mcp.IsMCPToolName(name) {
			return true
		}
	}
	return false
}

// startMCPWithEnv loads the MCP configuration for args.CWD and connects
// every server. Startup failures are appended to args.mcpStartupErrors; a
// server marked required that fails to start aborts with an error.
func startMCPWithEnv(ctx context.Context, args *Args, version string, env []string) (*mcp.Manager, func(), error) {
	return startMCPWithEnvTimed(ctx, args, version, env, nil)
}

func startMCPWithEnvTimed(ctx context.Context, args *Args, version string, env []string, timer *startupTimer) (*mcp.Manager, func(), error) {
	return startMCPManager(ctx, args, version, env, timer, nil)
}

// mcpReadyFunc receives a background server's start result.
type mcpReadyFunc func(ctx context.Context, name string, d time.Duration, err error)

// startMCPManager loads the configuration and starts the manager. With a nil
// onReady every server is awaited (Start); otherwise only required servers
// are awaited and the rest start in the background (StartBackground).
func startMCPManager(ctx context.Context, args *Args, version string, env []string, timer *startupTimer, onReady mcpReadyFunc) (*mcp.Manager, func(), error) {
	noop := func() {}
	if args.mcpManager != nil || !mcpAllowed(*args) {
		return args.mcpManager, noop, nil
	}
	cfg, cfgErrs := mcp.LoadConfig(ZutHome(), args.CWD, env)
	for _, err := range cfgErrs {
		args.mcpStartupErrors = append(args.mcpStartupErrors, err.Error())
	}
	if len(cfg.Servers) == 0 {
		reportMCPStartup(*args)
		return args.mcpManager, noop, nil
	}
	// MCP endpoints always verify TLS; --insecure is scoped to inference endpoints.
	manager := mcp.NewManager(cfg, args.CWD, mcp.ClientInfo{Name: "zut", Version: version}, provider.NewHTTPClient(false))
	manager.SetAuthPath(filepath.Join(ZutHome(), "mcp-auth.json"))
	if timer != nil {
		manager.SetStartupObserver(func(name string, d time.Duration, err error) {
			timer.item("mcp", name, d, err)
		})
	}
	if args.mcpToolsChanged != nil {
		manager.SetToolsChangedObserver(args.mcpToolsChanged)
	}
	var failures []error
	var requiredErr error
	if onReady != nil {
		failures, requiredErr = manager.StartBackground(ctx, onReady)
	} else {
		failures, requiredErr = manager.Start(ctx)
	}

	for _, err := range failures {
		args.mcpStartupErrors = append(args.mcpStartupErrors, err.Error())
	}
	if requiredErr != nil {
		_ = manager.Close()
		return nil, noop, fmt.Errorf("required MCP server failed to start: %w", requiredErr)
	}
	reportMCPStartup(*args)
	return manager, func() { _ = manager.Close() }, nil
}

// startSessionMCP starts session-owned connections; it never installs process
// signal handlers or terminates the process. The caller owns cancellation and supplies its base env.
// The returned manager is installed by assembly; args is never mutated.
// Use the returned cleanup on every exit.
func startSessionMCP(ctx context.Context, args Args, env []string) (*mcp.Manager, func(), error) {
	return startMCPWithEnvTimed(ctx, &args, "", env, nil)
}

// startSessionMCPBackground is startSessionMCP that awaits only required
// servers; onReady reports each other server once its start ends. Startup
// errors for awaited servers and config are returned in errs, matching
// args.mcpStartupErrors. This per-session start is the seam a per-daemon
// connection pool would replace.
func startSessionMCPBackground(ctx context.Context, args Args, env []string, timer *startupTimer, onReady mcpReadyFunc) (*mcp.Manager, func(), []string, error) {
	m, stop, err := startMCPManager(ctx, &args, "", env, timer, onReady)
	return m, stop, args.mcpStartupErrors, err
}

// reportMCPStartup prints startup failures for non-interactive modes. The
// interactive TUI shows them with the extension startup errors instead.
func reportMCPStartup(args Args) {
	if args.Mode == "" || args.Mode == ModeInteractive {
		return
	}
	for _, msg := range args.mcpStartupErrors {
		fmt.Fprintln(os.Stderr, "mcp:", msg)
	}
}

// mcpToolSelected applies an explicit --tools list to one MCP tool name.
func mcpToolSelected(args Args, name string) bool {
	if len(args.Tools) == 0 {
		return true
	}
	return toolListContains(args.Tools, mcpToolSelector) || toolListContains(args.Tools, name)
}

// addMCPTools registers the shared manager's tools in reg. The strict
// headless orchestrator is manager-only, so it receives only tools the
// server marks read-only (plus resource reads) for research; its workers
// receive the full set.
func addMCPTools(reg core.Registry, args Args) {
	if reg == nil || args.mcpManager == nil || !mcpAllowed(args) {
		return
	}
	strictOrchestrator := args.Orchestrate && !args.ResidentChild
	for _, tool := range args.mcpManager.Tools() {
		name := tool.Name()
		if _, exists := reg[name]; exists || !mcpToolSelected(args, name) {
			continue
		}
		if strictOrchestrator && !mcp.IsReadOnlyTool(tool) {
			continue
		}
		reg[name] = tool
	}
}

// mcpSystemAddendum describes connected servers when their tools are present.
func mcpSystemAddendum(reg core.Registry, args Args) string {
	if args.mcpManager == nil {
		return ""
	}
	has := false
	for _, tool := range reg {
		if mcp.IsTool(tool) {
			has = true
			break
		}
	}
	strictOrchestrator := args.Orchestrate && !args.ResidentChild
	if !has && !strictOrchestrator {
		return ""
	}
	addendum := args.mcpManager.SystemPromptAddendum()
	if addendum == "" {
		return ""
	}
	if args.Orchestrate && !args.ResidentChild {
		addendum += "\nAs the orchestrator you only hold read-only MCP tools for research. Workers receive every MCP tool; delegate MCP actions that change external state."
	}
	return addendum
}

// childMCPTools returns MCP tool names available to a resident child even
// when the manager-only parent does not hold them.
func childMCPTools(child core.Registry) []string {
	var out []string
	for name, tool := range child {
		if mcp.IsTool(tool) {
			out = append(out, name)
		}
	}
	return out
}
