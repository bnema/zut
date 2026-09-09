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

// RunStream runs the agent and writes assistant text to out as it
// arrives (EvTextDelta). Tool activity is written to stderr so stdout
// stays the live assistant transcript. When a provider does not emit
// deltas, the full assistant text is written when the message completes.
func RunStream(ctx context.Context, ag *core.Agent, prompt string, images []provider.ImageBlock, out io.Writer) error {
	return RunStreamWithDiag(ctx, ag, prompt, images, out, os.Stderr)
}

// RunStreamWithDiag is RunStream with an explicit diagnostics writer
// for tool activity (tests pass a buffer).
func RunStreamWithDiag(ctx context.Context, ag *core.Agent, prompt string, images []provider.ImageBlock, out, diag io.Writer) error {
	_, err := RunStreamWithContextRecovery(ctx, ag, prompt, images, out, diag, nil)
	return err
}

// RunStreamWithContextRecovery is RunStreamWithDiag with an optional
// checkpoint writer for a one-shot context-overflow compaction. OutputStart
// identifies the transcript suffix that callers must persist after a
// successful checkpoint.
func RunStreamWithContextRecovery(ctx context.Context, ag *core.Agent, prompt string, images []provider.ImageBlock, out, diag io.Writer, persistCompaction func([]provider.Message) error) (ContextRecoveryResult, error) {
	if diag == nil {
		diag = io.Discard
	}
	var (
		runErr    error
		streamed  bool
		wroteText bool
		lastWasNL bool
	)
	flush := func() {
		if f, ok := out.(interface{ Sync() error }); ok {
			_ = f.Sync()
		}
	}
	writeText := func(s string) {
		if s == "" {
			return
		}
		_, _ = fmt.Fprint(out, s)
		flush()
		wroteText = true
		lastWasNL = strings.HasSuffix(s, "\n")
	}
	assistantText := func(m provider.Message) string {
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

	sink := func(ev core.AgentEvent) {
		switch e := ev.(type) {
		case core.EvAssistantStart:
			streamed = false
		case core.EvTextDelta:
			streamed = true
			writeText(e.Delta)
		case core.EvAssistantMessage:
			if !streamed {
				writeText(assistantText(e.Message))
			}
		case core.EvToolCall:
			fmt.Fprintf(diag, "\n[tool] %s\n", e.Name)
			if len(e.Args) > 0 {
				fmt.Fprintf(diag, "%s\n", string(e.Args))
			}
		case core.EvToolProgress:
			if e.Text != "" {
				fmt.Fprint(diag, e.Text)
			}
		case core.EvToolResult:
			for _, c := range e.Result.Content {
				if tb, ok := c.(provider.TextBlock); ok && tb.Text != "" {
					fmt.Fprint(diag, tb.Text)
					if !strings.HasSuffix(tb.Text, "\n") {
						fmt.Fprint(diag, "\n")
					}
				}
			}
			if e.Result.IsError {
				fmt.Fprintln(diag, "[tool error]")
			}
		case core.EvTurnRecovery:
			// Assistant text stays on stdout; the recovery notice is a
			// host diagnostic and belongs on the diag channel.
			fmt.Fprintln(diag, "Continuing: no final answer received.")
		case core.EvTurnEnd:
			if e.Err != nil {
				runErr = e.Err
			}
		case core.EvError:
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
		return recovery, err
	}
	if runErr != nil {
		return recovery, runErr
	}
	if wroteText && !lastWasNL {
		_, _ = fmt.Fprint(out, "\n")
		flush()
	}
	return recovery, nil
}
