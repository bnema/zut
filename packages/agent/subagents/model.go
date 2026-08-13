package subagents

import "time"

// WebSearchPolicy is the capability decision propagated from a host agent to
// a resident child.
type WebSearchPolicy uint8

const (
	WebSearchInherit WebSearchPolicy = iota
	WebSearchDeny
	WebSearchAllow
)

func (p WebSearchPolicy) Allows() bool { return p == WebSearchAllow }

// childPolicy turns an unresolved or corrupt policy into an explicit deny.
func (p WebSearchPolicy) childPolicy() WebSearchPolicy {
	if p == WebSearchAllow {
		return WebSearchAllow
	}
	return WebSearchDeny
}

func (p WebSearchPolicy) String() string {
	switch p {
	case WebSearchAllow:
		return "allow"
	case WebSearchDeny:
		return "deny"
	case WebSearchInherit:
		return "inherit"
	default:
		// Unknown values must not serialize as inherit: that would let a
		// worker fall back to its own default and potentially regain access.
		return "deny"
	}
}

// WorkspaceMode selects how a child is allowed to access the repository.
type WorkspaceMode string

const (
	WorkspaceShared   WorkspaceMode = "shared"
	WorkspaceWorktree WorkspaceMode = "worktree"
)

// CaptureMode controls which durable worktree output is collected.
type CaptureMode string

const (
	CapturePatch CaptureMode = "patch"
	CaptureDiff  CaptureMode = "diff"
)

// SubagentPolicy contains the resident manager's limits and capability ceiling.
type SubagentPolicy struct {
	MaxConcurrent int
	QueueTimeout  time.Duration
	AllowedTools  []string
	AllowedRoots  []string
}

func (p SubagentPolicy) allowedTool(name string) bool {
	if len(p.AllowedTools) == 0 {
		return true
	}
	for _, allowed := range p.AllowedTools {
		if allowed == name {
			return true
		}
	}
	return false
}

// AllowsTool reports whether a child may receive name under the host policy.
// An empty allowlist retains the historical unrestricted policy.
func (p SubagentPolicy) AllowsTool(name string) bool { return p.allowedTool(name) }
