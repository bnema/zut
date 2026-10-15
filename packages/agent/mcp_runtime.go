package agent

import (
	"context"
	"fmt"
	"os"
	"strings"

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

// startMCP loads the MCP configuration and connects every server once per
// process. The manager is stored in args so every Resolve (primary, reloads,
// and resident children) shares the same connections. A server marked
// required that fails to start aborts the run.
func startMCP(ctx context.Context, args *Args, version string) (func(), error) {
	noop := func() {}
	if args.mcpManager != nil || !mcpAllowed(*args) {
		return noop, nil
	}
	cfg, cfgErrs := mcp.LoadConfig(ZutHome(), args.CWD)
	for _, err := range cfgErrs {
		args.mcpStartupErrors = append(args.mcpStartupErrors, err.Error())
	}
	if len(cfg.Servers) == 0 {
		reportMCPStartup(*args)
		return noop, nil
	}
	// MCP endpoints always verify TLS; --insecure is scoped to inference endpoints.
	manager := mcp.NewManager(cfg, args.CWD, mcp.ClientInfo{Name: "zut", Version: version}, provider.NewHTTPClient(false))
	failures, requiredErr := manager.Start(ctx)
	for _, err := range failures {
		args.mcpStartupErrors = append(args.mcpStartupErrors, err.Error())
	}
	if requiredErr != nil {
		_ = manager.Close()
		return noop, fmt.Errorf("required MCP server failed to start: %w", requiredErr)
	}
	args.mcpManager = manager
	reportMCPStartup(*args)
	return func() { _ = manager.Close() }, nil
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
	if !has && !(args.Orchestrate && !args.ResidentChild) {
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

// mcpStatusText renders /mcp and `zut mcp status` output.
func mcpStatusText(args Args) string {
	if args.mcpManager == nil {
		if !mcpAllowed(args) {
			return "MCP is disabled for this run"
		}
		if len(args.mcpStartupErrors) > 0 {
			return "MCP startup problems:\n" + strings.Join(args.mcpStartupErrors, "\n")
		}
		return "no MCP servers configured (" + mcp.GlobalConfigPath(ZutHome()) + " or .zut/mcp.json)"
	}
	return mcp.FormatStatus(args.mcpManager.Status())
}
