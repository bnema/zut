package agent

import (
	"fmt"
	"strings"
	"time"
)

// ToolSummary is a name+one-line description. Kept as part of the
// public opts type for backwards compatibility with callers that
// still pass tool summaries in; the default prompt no longer lists
// them because the provider already advertises tools in the request
// body's tools[] array, so listing them again in prose is pure
// duplication.
type ToolSummary struct {
	Name        string
	Description string
}

// SystemPromptOpts configures BuildSystemPrompt.
type SystemPromptOpts struct {
	CWD        string
	Tools      []ToolSummary
	Custom     string   // if set, replaces the built-in identity and docs guidance
	Append     []string // extra text appended at the end
	Now        time.Time
	ZutDocsDir string
}

// BuildSystemPrompt constructs the system prompt.
//
// Design note: kept intentionally small. Every byte here is part of
// the cached prefix on every request, so bloat is cumulatively
// expensive. We ship only:
//
//   - A one-paragraph identity (who zut is, what the name means,
//     what the TUI expects for output format).
//   - The date + cwd footer so the model has current-context.
//
// Everything else (tool listing, operating guidelines, "don't run
// sudo", "prefer edit over write", etc.) is left out because the
// current-generation frontier models already internalise it, and
// the tool schemas sent alongside the request carry each tool's
// own description.
//
// Users who want extra biasing can use --system-prompt (replace),
// --append-system-prompt (additive, repeatable), or drop a
// SYSTEM.md in $ZUT_HOME that overrides the default identity.
func BuildSystemPrompt(o SystemPromptOpts) string {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	date := o.Now.Format("2006-01-02")
	cwd := o.CWD
	if cwd == "" {
		cwd = "."
	}

	var sb strings.Builder

	if o.Custom != "" {
		sb.WriteString(o.Custom)
	} else {
		sb.WriteString(defaultIdentity)
	}

	sb.WriteString("\n\n")
	sb.WriteString(compactedSummaryHandoffInstruction)

	if o.Custom == "" && strings.TrimSpace(o.ZutDocsDir) != "" {
		sb.WriteString("\n\nZut's own docs are installed under ")
		sb.WriteString(o.ZutDocsDir)
		sb.WriteString("; use the read tool there when you need details about zut RPC, extensions, skills, or built-in behaviour.")
	}

	for _, a := range o.Append {
		if strings.TrimSpace(a) == "" {
			continue
		}
		sb.WriteString("\n\n")
		sb.WriteString(a)
	}

	fmt.Fprintf(&sb, "\n\nCurrent date: %s\nCurrent working directory: %s\n", date, cwd)
	return sb.String()
}

const defaultIdentity = `You are an expert coding assistant operating inside zut, a coding agent harness. The name "zut" stands for "zero-overhead-tool"; if the user asks what zut means, answer exactly that.

Your output renders in a TUI that understands markdown for prose and plain text for tool output. Use markdown freely, keep answers concise, and let tool calls speak for themselves rather than narrating them in prose before you invoke them. Act first, then summarise what you did.

For focused changes to an existing file, inspect its current contents and use edit with verbatim oldText taken from that same file. Include only enough context to make each match unambiguous. Use write when creating a file or replacing it wholesale. Do not mutate files through bash redirections or commands such as cat, echo, sed, or tee, because those changes appear as opaque shell output instead of a readable edit diff.`

const compactedSummaryHandoffInstruction = `When you see a "## Context Summary (compacted)" message, treat it as a handoff from earlier work. Keep its active constraints and preferences in force, continue the most recent unresolved user request without waiting for the user to type "continue", and follow a newer user request when it supersedes the summary.`
