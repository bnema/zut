package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

const (
	maxPythonLines = 2000
	maxPythonBytes = 50 * 1024

	// maxPythonTimeoutSeconds is the largest whole-second timeout that can
	// be converted to a time.Duration without overflowing it.
	maxPythonTimeoutSeconds int64 = int64(time.Duration(1<<63-1) / time.Second)
)

// PythonTool executes inline Python 3 code against an already prepared
// environment. Zut never installs Python, packages, uv, or virtual
// environments. Each call runs a fresh, stateless subprocess.
type PythonTool struct {
	CWD         string
	Sandbox     *Sandbox
	Interpreter string

	// resolveHook substitutes interpreter resolution in tests. Nil uses
	// the default lazy resolver, which shares the caller's timeout budget.
	resolveHook func(ctx context.Context, cwd, configured string) (resolvedPython, error)
	// execHook substitutes process execution in tests. Nil runs a real
	// subprocess directly (no shell, no temp file).
	execHook func(ctx context.Context, exe, code, cwd string, progress func(string)) (pythonExecOutcome, error)
}

// pythonExecOutcome is the raw result of one Python subprocess run.
type pythonExecOutcome struct {
	Captured   string
	ExitCode   int
	TimedOut   bool
	Cancelled  bool
	BytesTrunc bool
	LinesTrunc bool
	CleanupErr error
}

type pythonArgs struct {
	Code    string `json:"code"`
	Timeout *int64 `json:"timeout"`
}

var pythonSchema = fmt.Sprintf(`{"type":"object","properties":{"code":{"type":"string","description":"Inline Python 3 source to execute via stdin (no interactive input; stdin observes EOF). Save reusable scripts with write/edit instead."},"timeout":{"type":"integer","minimum":1,"maximum":%d,"description":"Maximum runtime in seconds, including interpreter discovery."}},"required":["code","timeout"]}`, maxPythonTimeoutSeconds)

func (t *PythonTool) Name() string { return "python" }

func (t *PythonTool) Description() string {
	return "Execute inline Python 3 code with an existing interpreter (stateless stdin execution in the session cwd, merged stdout/stderr). Never installs packages; save reusable scripts with write/edit."
}

func (t *PythonTool) Schema() json.RawMessage { return json.RawMessage(pythonSchema) }

func pythonTimeoutDuration(seconds int64) (time.Duration, error) {
	if seconds < 1 || seconds > maxPythonTimeoutSeconds {
		return 0, fmt.Errorf("timeout must be between 1 and %d seconds", maxPythonTimeoutSeconds)
	}
	return time.Duration(seconds) * time.Second, nil
}

// pythonExecArgv builds the direct execution argv. The executable and its
// arguments stay separate; they are never concatenated into a shell command.
func pythonExecArgv(exe string) []string {
	return []string{exe, "-u", "-B", "-"}
}

func (t *PythonTool) Execute(ctx context.Context, raw json.RawMessage, progress func(string)) (core.ToolResult, error) {
	var a pythonArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return core.ToolResult{}, fmt.Errorf("invalid args: %w", err)
	}
	if strings.TrimSpace(a.Code) == "" {
		return core.ToolResult{}, fmt.Errorf("code is required")
	}
	if a.Timeout == nil {
		return core.ToolResult{}, fmt.Errorf("timeout is required")
	}
	timeout, err := pythonTimeoutDuration(*a.Timeout)
	if err != nil {
		return core.ToolResult{}, err
	}
	// Admission checks run before any interpreter discovery, so a denied
	// call never probes the filesystem or spawns a process.
	if t.Sandbox != nil && t.Sandbox.Permissions != nil {
		return core.ToolResult{}, fmt.Errorf("permission denied: this agent cannot run python")
	}
	if t.Sandbox.Locked() {
		return core.ToolResult{}, fmt.Errorf("jailed: arbitrary Python cannot be confined by the current jail (use /unjail to disable)")
	}
	cwd := t.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	// Resolution and execution share the caller's timeout budget.
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	interp, err := t.resolve(runCtx, cwd)
	if err != nil {
		return core.ToolResult{}, err
	}

	outcome, err := t.run(runCtx, ctx, interp.Path, a.Code, cwd, progress)
	if err != nil {
		return core.ToolResult{}, err
	}

	trimmed, truncLines := truncatePythonLines(outcome.Captured)
	outcome.LinesTrunc = truncLines
	text := formatPythonResult(interp, outcome, trimmed, int64(timeout/time.Second))
	isErr := outcome.ExitCode != 0 || outcome.TimedOut || outcome.Cancelled
	details := map[string]any{
		"exit_code":       outcome.ExitCode,
		"interpreter":     interp.Path,
		"python_version":  interp.Version.String(),
		"bytes_truncated": outcome.BytesTrunc,
		"lines_truncated": outcome.LinesTrunc,
	}
	if outcome.CleanupErr != nil {
		details["cleanup_error"] = outcome.CleanupErr.Error()
	}
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: text}},
		IsError: isErr,
		Details: details,
	}, nil
}

func (t *PythonTool) resolve(ctx context.Context, cwd string) (resolvedPython, error) {
	if t.resolveHook != nil {
		return t.resolveHook(ctx, cwd, t.Interpreter)
	}
	return resolvePythonInterpreter(ctx, pythonResolveDeps{CWD: cwd, Configured: t.Interpreter})
}

func (t *PythonTool) run(runCtx, parentCtx context.Context, exe, code, cwd string, progress func(string)) (pythonExecOutcome, error) {
	if t.execHook != nil {
		return t.execHook(runCtx, exe, code, cwd, progress)
	}
	return runPythonProcess(runCtx, parentCtx, exe, code, cwd, progress)
}

// runPythonProcess runs one fresh subprocess directly with code on stdin.
// It bounds captured output, drains pipes, and reports cleanup failure
// without claiming every detached descendant was terminated.
func runPythonProcess(runCtx, parentCtx context.Context, exe, code, cwd string, progress func(string)) (pythonExecOutcome, error) {
	var outcome pythonExecOutcome
	cmd := exec.CommandContext(runCtx, exe, "-u", "-B", "-")
	cmd.Dir = cwd
	cmd.Env = pythonChildEnv()
	cmd.Stdin = strings.NewReader(code)

	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	closeOutput := configurePythonProcess(cmd, pw)
	defer closeOutput()

	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		return outcome, fmt.Errorf("start: %w", err)
	}

	captured := &bytes.Buffer{}
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		buf := make([]byte, 4096)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				if captured.Len() < maxPythonBytes {
					room := maxPythonBytes - captured.Len()
					if n > room {
						captured.Write(chunk[:room])
					} else {
						captured.Write(chunk)
					}
				}
				if progress != nil {
					progress(string(chunk))
				}
			}
			if err != nil {
				return
			}
		}
	}()

	waitErr := cmd.Wait()
	closeOutput()
	<-readerDone

	outcome.Captured = captured.String()
	outcome.BytesTrunc = captured.Len() >= maxPythonBytes

	exitCode := 0
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}
	outcome.ExitCode = exitCode
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) && parentCtx.Err() == nil {
		outcome.TimedOut = true
	} else if parentCtx.Err() != nil || errors.Is(runCtx.Err(), context.Canceled) {
		outcome.Cancelled = true
	}
	return outcome, nil
}

// truncatePythonLines bounds the line count of captured output.
func truncatePythonLines(output string) (trimmed string, truncLines bool) {
	lines := strings.Split(output, "\n")
	if len(lines) > maxPythonLines {
		return strings.Join(lines[:maxPythonLines], "\n"), true
	}
	return output, false
}

// formatPythonResult renders interpreter/version, captured output, and
// exit/timeout/cancellation status. Truncation is marked explicitly; v1
// provides no full-output artifact file. The caller supplies the
// line-trimmed output so Details and text agree on lines_truncated.
func formatPythonResult(interp resolvedPython, outcome pythonExecOutcome, trimmed string, timeoutSeconds int64) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "python %s (%s)\n", interp.Version.String(), interp.Path)
	if trimmed != "" {
		sb.WriteString("\n")
		sb.WriteString(trimmed)
		if !strings.HasSuffix(trimmed, "\n") {
			sb.WriteString("\n")
		}
	}
	if outcome.LinesTrunc {
		fmt.Fprintf(&sb, "... [truncated at %d lines]\n", maxPythonLines)
	}
	if outcome.BytesTrunc {
		fmt.Fprintf(&sb, "... [truncated at %d bytes]\n", maxPythonBytes)
	}
	sb.WriteString("\n")
	switch {
	case outcome.TimedOut:
		fmt.Fprintf(&sb, "[timed out after %d second", timeoutSeconds)
		if timeoutSeconds != 1 {
			sb.WriteByte('s')
		}
		sb.WriteByte(']')
	case outcome.Cancelled:
		sb.WriteString("[cancelled]")
	case outcome.ExitCode == 0:
		sb.WriteString("[exit 0]")
	default:
		fmt.Fprintf(&sb, "[exit %d]", outcome.ExitCode)
	}
	if outcome.CleanupErr != nil {
		fmt.Fprintf(&sb, " [cleanup: %s]", outcome.CleanupErr.Error())
	}
	return sb.String()
}
