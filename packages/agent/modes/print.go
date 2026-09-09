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
	var finalText strings.Builder
	var lastAssistant string
	var usage provider.Usage
	var haveUsage bool
	var runErr error

	sink := func(ev core.AgentEvent) {
		switch e := ev.(type) {
		case core.EvAssistantMessage:
			// Keep the most recent assistant text block; by the end it's the final answer.
			var sb strings.Builder
			for _, c := range e.Message.Content {
				if tb, ok := c.(provider.TextBlock); ok {
					if sb.Len() > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(tb.Text)
				}
			}
			if sb.Len() > 0 {
				lastAssistant = sb.String()
			}
		case core.EvUsage:
			if haveUsage {
				usage = usage.Add(e.Usage)
			} else {
				usage = e.Usage
				haveUsage = true
			}
		case core.EvTurnRecovery:
			// The core owns the single bounded continuation; surface it
			// on stderr so stdout keeps only the final answer. An
			// exhausted allowance returns ErrIncompleteTurn from Prompt
			// and never reaches here as success.
			fmt.Fprintln(os.Stderr, "Continuing: no final answer received.")
		case core.EvTurnEnd:
			if e.Err != nil {
				runErr = e.Err
			}
		}
	}

	recovery, err := PromptWithContextRecovery(ctx, ag, prompt, images, sink, ContextRecoveryOptions{
		PersistCompaction:            persistCompaction,
		SuppressInitialOverflowEvent: true,
	})
	if err != nil {
		return usage, recovery, err
	}
	if runErr != nil {
		return usage, recovery, runErr
	}

	finalText.WriteString(lastAssistant)
	if finalText.Len() > 0 && !strings.HasSuffix(finalText.String(), "\n") {
		finalText.WriteString("\n")
	}
	_, err = fmt.Fprint(out, finalText.String())
	return usage, recovery, err
}
