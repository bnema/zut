package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/bnema/zut/packages/agent/modes"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// runOrchestratedPrintMode is deliberately separate from ordinary print mode:
// ordinary print continues to stream its existing one-turn implementation,
// while this path keeps every parent turn in memory until terminal synthesis.
func runOrchestratedPrintMode(ctx context.Context, args Args, version string) error {
	return runOrchestratedMode(ctx, args, version, orchestratedModeHooks{
		promptLabel: "print",
		startupSink: func() (func(core.AgentEvent), func()) {
			return nil, func() {}
		},
		runPrimary: func(turnCtx context.Context, ag *core.Agent, prompt string, persist func([]provider.Message) error) (string, modes.ContextRecoveryResult, error) {
			var captured bytes.Buffer
			_, recovery, err := modes.RunPrintWithContextRecovery(turnCtx, ag, prompt, nil, &captured, persist)
			return strings.TrimSuffix(captured.String(), "\n"), recovery, err
		},
		finish: func(finalText string) error {
			_, err := fmt.Fprintln(os.Stdout, finalText)
			return err
		},
	})
}
