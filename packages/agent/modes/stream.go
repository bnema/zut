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
	r := NewStreamRenderer(out, diag)
	recovery, err := PromptWithContextRecovery(ctx, ag, prompt, images, r.Handle, ContextRecoveryOptions{
		PersistCompaction:            persistCompaction,
		SuppressInitialOverflowEvent: true,
	})
	if err != nil {
		return recovery, err
	}
	if runErr := r.Err(); runErr != nil {
		return recovery, runErr
	}
	r.Finish()
	return recovery, nil
}

// StreamRenderer is the --stream formatter: assistant text goes to out as
// it arrives, tool activity and host diagnostics go to diag. It consumes
// agent events from an in-process agent or a daemon session.
type StreamRenderer struct {
	out, diag io.Writer
	err       error
	streamed  bool
	wroteText bool
	lastWasNL bool
}

// NewStreamRenderer returns a stream formatter. A nil diag discards tool activity.
func NewStreamRenderer(out, diag io.Writer) *StreamRenderer {
	if diag == nil {
		diag = io.Discard
	}
	return &StreamRenderer{out: out, diag: diag}
}

func (r *StreamRenderer) flush() {
	if f, ok := r.out.(interface{ Sync() error }); ok {
		_ = f.Sync()
	}
}

func (r *StreamRenderer) writeText(s string) {
	if s == "" {
		return
	}
	_, _ = fmt.Fprint(r.out, s)
	r.flush()
	r.wroteText = true
	r.lastWasNL = strings.HasSuffix(s, "\n")
}

// Handle consumes one agent event.
func (r *StreamRenderer) Handle(ev core.AgentEvent) {
	switch e := ev.(type) {
	case core.EvAssistantStart:
		r.streamed = false
	case core.EvTextDelta:
		r.streamed = true
		r.writeText(e.Delta)
	case core.EvAssistantMessage:
		if !r.streamed {
			r.writeText(AssistantText(e.Message))
		}
	case core.EvToolCall:
		fmt.Fprintf(r.diag, "\n[tool] %s\n", e.Name)
		if len(e.Args) > 0 {
			fmt.Fprintf(r.diag, "%s\n", string(e.Args))
		}
	case core.EvToolProgress:
		if e.Text != "" {
			fmt.Fprint(r.diag, e.Text)
		}
	case core.EvToolResult:
		for _, c := range e.Result.Content {
			if tb, ok := c.(provider.TextBlock); ok && tb.Text != "" {
				fmt.Fprint(r.diag, tb.Text)
				if !strings.HasSuffix(tb.Text, "\n") {
					fmt.Fprint(r.diag, "\n")
				}
			}
		}
		if e.Result.IsError {
			fmt.Fprintln(r.diag, "[tool error]")
		}
	case core.EvTurnRecovery:
		// Assistant text stays on stdout; the recovery notice is a
		// host diagnostic and belongs on the diag channel.
		fmt.Fprintln(r.diag, "Continuing: no final answer received.")
	case core.EvRepetitionGuard:
		writeRepetitionDiag(r.diag, e)
	case core.EvTurnEnd:
		if e.Err != nil {
			r.err = e.Err
		}
	case core.EvError:
		if e.Err != nil {
			r.err = e.Err
		}
	}
}

// Err returns the last turn_end or error event error.
func (r *StreamRenderer) Err() error { return r.err }

// ResetErr clears the recorded error (a retried turn succeeded).
func (r *StreamRenderer) ResetErr() { r.err = nil }

// Finish ends one prompt: it terminates the streamed text with a newline
// when needed and resets the per-prompt state.
func (r *StreamRenderer) Finish() {
	if r.wroteText && !r.lastWasNL {
		_, _ = fmt.Fprint(r.out, "\n")
		r.flush()
	}
	r.wroteText, r.lastWasNL, r.streamed = false, false, false
}
