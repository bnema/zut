package agent

import (
	"fmt"
	"strings"
)

// validateOrchestrationArgs is the single launch-time gate for headless
// orchestration. Callers should run it before any mode-specific setup and
// again after attaching packaged-agent metadata when using `zut run`.
func validateOrchestrationArgs(args Args) error {
	if !args.Orchestrate {
		return nil
	}
	switch args.Mode {
	case ModePrint, ModeStream, ModeJSON:
		// These are the non-interactive output modes. Print is the first
		// execution path; stream and JSON retain the same eligibility contract
		// for callers that select those output protocols.
	case ModeRPC:
		return fmt.Errorf("--orchestrate is not supported with RPC mode")
	case ModeSubagentWorker:
		return fmt.Errorf("--orchestrate is not supported in subagent-worker mode")
	default:
		return fmt.Errorf("--orchestrate requires print, stream, or JSON mode")
	}
	if strings.TrimSpace(args.Subagent) != "" {
		return fmt.Errorf("--orchestrate is not supported with --subagent profiles")
	}
	if args.PermissionSet != nil || strings.TrimSpace(args.AgentName) != "" || strings.TrimSpace(args.AgentDataDir) != "" {
		return fmt.Errorf("--orchestrate is not supported for packaged agents")
	}
	if args.StatsPath != "" {
		return fmt.Errorf("--orchestrate cannot be combined with --stats")
	}
	if !autoSubagentsToolAllowed(args) {
		return fmt.Errorf("--orchestrate requires the subagent_spawn tool")
	}
	return nil
}
