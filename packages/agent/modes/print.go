// Package modes implements zut's three run modes: print, json, interactive.
package modes

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// RunPrint runs the agent to completion and writes only the final
// assistant text block to out. It returns usage for this invocation,
// excluding any cumulative usage restored from a session.
func RunPrint(ctx context.Context, ag *core.Agent, prompt string, images []provider.ImageBlock, out io.Writer) (provider.Usage, error) {
	usage, _, err := RunPrintWithContextRecovery(ctx, ag, prompt, images, out, nil)
	return usage, err
}

// RunPrintWithContextRecovery is RunPrint with an optional checkpoint writer
// for a one-shot context-overflow compaction. OutputStart identifies the
// transcript suffix that callers must persist after a successful checkpoint.
func RunPrintWithContextRecovery(ctx context.Context, ag *core.Agent, prompt string, images []provider.ImageBlock, out io.Writer, persistCompaction func([]provider.Message) error) (provider.Usage, ContextRecoveryResult, error) {
	r := NewPrintRenderer(os.Stderr)
	recovery, err := PromptWithContextRecovery(ctx, ag, prompt, images, r.Handle, ContextRecoveryOptions{
		PersistCompaction:            persistCompaction,
		SuppressInitialOverflowEvent: true,
	})
	if err != nil {
		return r.Usage(), recovery, err
	}
	if runErr := r.Err(); runErr != nil {
		return r.Usage(), recovery, runErr
	}
	return r.Usage(), recovery, r.WriteFinal(out)
}

// PrintRenderer is the -p formatter. It consumes agent events (from an
// in-process agent or a daemon session) and keeps the last assistant text
// block, the summed usage and the last turn error. Diagnostics go to diag.
type PrintRenderer struct {
	diag          io.Writer
	lastAssistant string
	usage         provider.Usage
	haveUsage     bool
	err           error
}

// NewPrintRenderer returns a print formatter writing diagnostics to diag.
func NewPrintRenderer(diag io.Writer) *PrintRenderer {
	if diag == nil {
		diag = io.Discard
	}
	return &PrintRenderer{diag: diag}
}

// Handle consumes one agent event.
func (r *PrintRenderer) Handle(ev core.AgentEvent) {
	switch e := ev.(type) {
	case core.EvAssistantMessage:
		// Keep the most recent assistant text block; by the end it's the final answer.
		if text := AssistantText(e.Message); text != "" {
			r.lastAssistant = text
		}
	case core.EvUsage:
		if r.haveUsage {
			r.usage = r.usage.Add(e.Usage)
		} else {
			r.usage = e.Usage
			r.haveUsage = true
		}
	case core.EvTurnRecovery:
		// The core owns the single bounded continuation; surface it
		// on stderr so stdout keeps only the final answer. An
		// exhausted allowance returns ErrIncompleteTurn from Prompt
		// and never reaches here as success.
		fmt.Fprintln(r.diag, "Continuing: no final answer received.")
	case core.EvRepetitionGuard:
		writeRepetitionDiag(r.diag, e)
	case core.EvTurnEnd:
		if e.Err != nil {
			r.err = e.Err
		}
	}
}

// Text returns the last assistant text block seen.
func (r *PrintRenderer) Text() string { return r.lastAssistant }

// ResetText forgets the last assistant text (start of a new parent turn).
func (r *PrintRenderer) ResetText() { r.lastAssistant = "" }

// Usage returns the usage summed over every EvUsage event.
func (r *PrintRenderer) Usage() provider.Usage { return r.usage }

// Err returns the last turn_end error.
func (r *PrintRenderer) Err() error { return r.err }

// ResetErr clears the recorded turn error (a retried turn succeeded).
func (r *PrintRenderer) ResetErr() { r.err = nil }

// WriteFinal writes the final answer followed by one newline (nothing when empty).
func (r *PrintRenderer) WriteFinal(out io.Writer) error {
	text := r.lastAssistant
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	_, err := fmt.Fprint(out, text)
	return err
}

// AssistantText joins the text blocks of an assistant message with newlines.
func AssistantText(m provider.Message) string { return MessageText(m) }

// MessageText joins the text blocks of a message with newlines.
func MessageText(m provider.Message) string {
	var sb strings.Builder
	for _, c := range m.Content {
		if tb, ok := c.(provider.TextBlock); ok {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}

func writeRepetitionDiag(diag io.Writer, e core.EvRepetitionGuard) {
	switch e.Stage {
	case core.RepetitionGuardWarning:
		fmt.Fprintf(diag, "Repeated content detected (%d occurrences); asking the agent to try another approach.\n", e.Count)
	case core.RepetitionGuardStopped:
		fmt.Fprintf(diag, "Stopped a repetitive loop after %d occurrences.\n", e.Count)
	}
}
