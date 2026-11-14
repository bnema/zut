package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

//go:embed codemode_prelude.py
var codemodePrelude string

// CodemodeTool runs Python with access to guarded nested tool invocations.
type CodemodeTool struct {
	CWD string
	// Env is the explicit session environment; nil is invalid.
	Env         []string
	Sandbox     *Sandbox
	Interpreter string
}

func (*CodemodeTool) Name() string { return "codemode" }
func (*CodemodeTool) Description() string {
	return "Run Python with the same host access as python; prints go to output. zut.call(name, check=True, **args), zut.tools.<name>(check=True, **args), or zut.tools[name](**args) returns ToolResult (.text, .data, .is_error, .id). Names resolve case-insensitively with underscores/dashes equivalent; dir(zut.tools) lists allowed names. zut.mcp.<server>.<method>(check=True, **args) exposes MCP tools with Python-safe identifiers; dir() lists servers/methods. Sanitization collisions, reserved dunders, and unparseable names are omitted; use zut.call with the canonical name. check is reserved, not a tool argument. zut.search(query, limit=10) returns a list with .total/.truncated; zut.describe(name), zut.result(value). Bash .data['exit_code'] gives status (also output, timed_out). Exceptions: zut.ToolDenied, zut.ToolError for is_error unless check=False, zut.CodemodeError with .kind. Each nested call follows normal confirmations."
}
func (*CodemodeTool) Schema() json.RawMessage { return json.RawMessage(pythonSchema) }

func (t *CodemodeTool) Execute(ctx context.Context, raw json.RawMessage, progress func(string)) (core.ToolResult, error) {
	if err := requireSessionCWD("codemode", t.CWD); err != nil {
		return core.ToolResult{}, err
	}
	if t.Env == nil {
		return core.ToolResult{}, fmt.Errorf("tool codemode: session env not set")
	}
	var a pythonArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return protocolToolError(fmt.Sprintf("invalid args: %v", err))
	}
	if strings.TrimSpace(a.Code) == "" {
		return protocolToolError("code is required")
	}
	if a.Timeout == nil {
		return protocolToolError("timeout is required")
	}
	timeout, err := pythonTimeoutDuration(*a.Timeout)
	if err != nil {
		return protocolToolError(err.Error())
	}
	if err := CodemodeAdmission(t.Sandbox); err != nil {
		return core.ToolResult{}, err
	}
	inv, ok := core.NestedInvokerFrom(ctx)
	if !ok {
		return toolErr("codemode: nested tool calls are unavailable in this context"), nil
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	interp, err := resolvePythonInterpreter(runCtx, pythonResolveDeps{CWD: t.CWD, Env: t.Env, Configured: t.Interpreter})
	if err != nil {
		return toolErr(err.Error()), nil
	}
	reqR, reqW, err := os.Pipe()
	if err != nil {
		return core.ToolResult{}, err
	}
	defer reqR.Close()
	defer reqW.Close()
	respR, respW, err := os.Pipe()
	if err != nil {
		return core.ToolResult{}, err
	}
	defer respR.Close()
	defer respW.Close()
	bridge := newCodemodeBridge(inv, a.Code)
	bridgeDone := make(chan error, 1)
	go func() { bridgeDone <- bridge.serve(runCtx, reqR, respW) }()
	outcome, runErr := runPythonProcess(runCtx, ctx, pythonRunSpec{
		Exe: interp.Path, Code: codemodePrelude, CWD: t.CWD, Env: t.Env, Progress: progress,
		ChildFiles: []*os.File{reqW, respR},
		Started:    func() { _ = reqW.Close(); _ = respR.Close() },
	})
	cancel() // Python exit must cancel any in-flight nested invocation before joining.
	// The child has exited (or failed to start). Close all parent ends before
	// joining: descendants may still hold bridge descriptors, outside WaitDelay.
	_ = reqW.Close()
	_ = respR.Close()
	_ = reqR.Close()
	_ = respW.Close()
	bridgeErr := <-bridgeDone
	if runErr != nil {
		return core.ToolResult{}, runErr
	}
	if bridgeErr != nil && !errors.Is(bridgeErr, os.ErrClosed) && !errors.Is(bridgeErr, context.Canceled) && !errors.Is(bridgeErr, context.DeadlineExceeded) {
		outcome.CleanupErr = bridgeErr
	}
	trimmed, truncLines := truncatePythonLines(outcome.Captured)
	outcome.LinesTrunc = truncLines
	text := formatPythonResult(interp, outcome, trimmed, int64(timeout/time.Second))
	fullPath := ""
	if outcome.BytesTrunc || outcome.LinesTrunc {
		fullPath = writePythonFullOutput(outcome.Captured)
		if fullPath != "" {
			text += fmt.Sprintf(" (full output: %s)", fullPath)
		}
	}
	if value := bridge.Result(); len(value) > 0 {
		text += "\nresult: " + string(value)
	}
	calls, errs, refusals := bridge.Counts()
	text += fmt.Sprintf("\nnested calls: %d (errors: %d, denied: %d)", calls, errs, refusals)
	details := map[string]any{"exit_code": outcome.ExitCode, "interpreter": interp.Path, "python_version": interp.Version.String(), "bytes_truncated": outcome.BytesTrunc, "lines_truncated": outcome.LinesTrunc, "full_output_path": fullPath}
	if outcome.CleanupErr != nil {
		details["cleanup_error"] = outcome.CleanupErr.Error()
	}
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: text}}, IsError: outcome.ExitCode != 0 || outcome.TimedOut || outcome.Cancelled, Details: details, Context: bridge.Context()}, nil
}

// CodemodeAdmission checks whether a sandbox permits arbitrary Python execution.
// It is shared by execution and dynamic MCP deferral.
func CodemodeAdmission(s *Sandbox) error {
	if s != nil && s.Permissions != nil {
		return denied("permission denied: this agent cannot run codemode")
	}
	if s.Locked() {
		return denied("jailed: arbitrary Python cannot be confined by the current jail (use /unjail to disable)")
	}
	return nil
}
