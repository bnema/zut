package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

const (
	maxGrepInputBytes     = 16 * 1024
	maxGrepOutputBytes    = 50 * 1024
	maxGrepOutputLines    = 2000
	grepTruncationReserve = 128
	grepInternalTimeout   = 30 * time.Second
)

// GrepTool searches text files below a requested path without invoking a
// shell. It prefers ripgrep and uses the system grep as a portable fallback.
// LookPath is injectable for deterministic tests; nil uses exec.LookPath.
type GrepTool struct {
	CWD      string
	Sandbox  *Sandbox
	LookPath func(string) (string, error)
}

var _ core.Tool = (*GrepTool)(nil)

type grepArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

const grepSchema = `{"type":"object","properties":{"pattern":{"type":"string","description":"Regular expression to search for."},"path":{"type":"string","description":"File or directory to search recursively."}},"required":["pattern","path"],"additionalProperties":false}`

func (t *GrepTool) Name() string { return "grep" }

func (t *GrepTool) Description() string {
	return "Search files for a regular expression using ripgrep when available, with a portable grep fallback; fallback regex syntax and ignore/hidden-file behavior may differ."
}

func (t *GrepTool) Schema() json.RawMessage { return json.RawMessage(grepSchema) }

// Execute searches the requested file or directory. The search root is
// checked before the executable is selected or started, so a denied path
// cannot be used to probe or read through the subprocess.
func (t *GrepTool) Execute(ctx context.Context, raw json.RawMessage, progress func(string)) (core.ToolResult, error) {
	return t.executeWithTimeout(ctx, raw, progress, grepInternalTimeout)
}

// grepTimeoutError distinguishes the search's hard deadline from a caller
// cancellation or deadline, so the caller gets actionable search guidance.
type grepTimeoutError struct {
	after time.Duration
}

func (e *grepTimeoutError) Error() string {
	return fmt.Sprintf("grep: search timed out after %s; try narrowing the path or glob", e.after)
}

func (e *grepTimeoutError) Timeout() bool { return true }

// executeWithTimeout keeps the production deadline in Execute while allowing
// deadline behavior to be tested without waiting 30 seconds.
func (t *GrepTool) executeWithTimeout(ctx context.Context, raw json.RawMessage, progress func(string), timeout time.Duration) (core.ToolResult, error) {
	args, err := parseGrepArgs(raw)
	if err != nil {
		return core.ToolResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return core.ToolResult{}, err
	}

	root := filepath.Clean(resolvePath(t.CWD, args.Path))
	if !filepath.IsAbs(root) {
		root, err = filepath.Abs(root)
		if err != nil {
			return core.ToolResult{}, err
		}
	}
	if err := t.Sandbox.CheckReadPath(root); err != nil {
		return core.ToolResult{}, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return core.ToolResult{}, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return core.ToolResult{}, fmt.Errorf("grep: %s is not a regular file or directory", args.Path)
	}

	engine, executable, err := t.searchExecutable()
	if err != nil {
		return core.ToolResult{}, err
	}

	commandArgs := grepCommandArgs(engine, args.Pattern, root)
	// WithTimeout preserves a sooner deadline or cancellation from ctx; the
	// internal deadline can only add a limit when the caller has not already
	// supplied a stricter one.
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, executable, commandArgs...)
	if info.IsDir() {
		cmd.Dir = root
	} else {
		cmd.Dir = filepath.Dir(root)
	}
	// Reuse the process-tree cancellation behavior used by the direct command
	// tools. This does not invoke a shell; executable is run directly above.
	configureBashProcess(cmd, nil)

	output := &grepOutput{
		progress: progress,
		cancel: func() {
			if cmd.Cancel != nil {
				_ = cmd.Cancel()
			}
		},
	}
	cmd.Stdout = output
	cmd.Stderr = output
	runErr := cmd.Run()
	if err := ctx.Err(); err != nil {
		return core.ToolResult{}, err
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return core.ToolResult{}, &grepTimeoutError{after: timeout}
	}

	actualExitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return core.ToolResult{}, fmt.Errorf("grep: run %s: %w", engine, runErr)
		}
		actualExitCode = exitErr.ExitCode()
	}

	outputCancelled := output.wasCancelled()
	matched := actualExitCode == 0 || outputCancelled
	noMatch := actualExitCode == 1
	if noMatch {
		// Both rg and grep use exit status 1 for a successful search with no
		// matches. Do not turn that normal result into a failed tool call.
		matched = false
	}

	text, linesTruncated := output.text()
	if noMatch && strings.TrimSpace(text) == "" {
		text = "No matches found.\n"
	}
	if text == "" && actualExitCode != 0 {
		text = fmt.Sprintf("%s exited with status %d.\n", engine, actualExitCode)
	}

	exitCode := actualExitCode
	if noMatch || outputCancelled {
		exitCode = 0
	}
	isError := actualExitCode > 1 || actualExitCode < 0
	if outputCancelled {
		// Reaching the output bound is an intentional, successful stop rather
		// than a failed search. Preserve the process status separately for
		// diagnostics while reporting the normalized tool exit code above.
		isError = false
	}
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: text}},
		IsError: isError,
		Details: map[string]any{
			"engine":            engine,
			"path":              t.Sandbox.DisplayPath(root, args.Path),
			"exit_code":         exitCode,
			"process_exit_code": actualExitCode,
			"matched":           matched,
			"bytes_truncated":   output.bytesTruncated(),
			"lines_truncated":   linesTruncated,
			"output_cancelled":  outputCancelled,
		},
	}, nil
}

func parseGrepArgs(raw json.RawMessage) (grepArgs, error) {
	if len(raw) == 0 || len(raw) > maxGrepInputBytes {
		return grepArgs{}, fmt.Errorf("grep: invalid arguments")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return grepArgs{}, fmt.Errorf("grep: invalid arguments")
	}
	for key := range fields {
		if key != "pattern" && key != "path" {
			return grepArgs{}, fmt.Errorf("grep: unknown argument %q", key)
		}
	}
	var args grepArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return grepArgs{}, fmt.Errorf("grep: invalid arguments: %w", err)
	}
	if _, ok := fields["pattern"]; !ok || args.Pattern == "" {
		return grepArgs{}, fmt.Errorf("grep: pattern is required")
	}
	if _, ok := fields["path"]; !ok || args.Path == "" {
		return grepArgs{}, fmt.Errorf("grep: path is required")
	}
	if !utf8.ValidString(args.Pattern) || !utf8.ValidString(args.Path) || strings.IndexByte(args.Pattern, 0) >= 0 || strings.IndexByte(args.Path, 0) >= 0 {
		return grepArgs{}, fmt.Errorf("grep: arguments contain invalid characters")
	}
	return args, nil
}

func (t *GrepTool) searchExecutable() (string, string, error) {
	lookup := t.LookPath
	if lookup == nil {
		lookup = exec.LookPath
	}
	if path, err := lookup("rg"); err == nil && path != "" {
		return "rg", path, nil
	}
	if path, err := lookup("grep"); err == nil && path != "" {
		return "grep", path, nil
	}
	return "", "", fmt.Errorf("grep: neither rg nor grep is available")
}

func grepCommandArgs(engine, pattern, root string) []string {
	if engine == "rg" {
		// These flags avoid headings and terminal color while retaining rg's
		// fast default ignore and binary-file filtering behavior.
		return []string{"--no-heading", "--line-number", "--color=never", "-e", pattern, root}
	}
	// -e keeps patterns beginning with '-' from being parsed as options. The
	// absolute resolved root also makes this portable across GNU and BSD grep.
	return []string{"-r", "-n", "-H", "-I", "-e", pattern, root}
}

type grepOutput struct {
	mu        sync.Mutex
	data      bytes.Buffer
	truncated bool
	cancelled bool
	cancel    func()
	progress  func(string)
}

func (w *grepOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	room := maxGrepOutputBytes - grepTruncationReserve - w.data.Len()
	var streamed string
	shouldCancel := false
	if room <= 0 {
		if !w.truncated {
			w.truncated = true
			w.cancelled = true
			shouldCancel = true
		}
	} else if len(p) > room {
		_, _ = w.data.Write(p[:room])
		w.truncated = true
		w.cancelled = true
		shouldCancel = true
		streamed = string(p[:room])
	} else {
		_, _ = w.data.Write(p)
		streamed = string(p)
	}
	cancel := w.cancel
	progress := w.progress
	w.mu.Unlock()

	if shouldCancel && cancel != nil {
		cancel()
	}
	if progress != nil && streamed != "" {
		progress(streamed)
	}
	return len(p), nil
}

func (w *grepOutput) bytesTruncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.truncated
}

func (w *grepOutput) wasCancelled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cancelled
}

func (w *grepOutput) text() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	text := w.data.String()
	lineCount := strings.Count(text, "\n")
	if text != "" && !strings.HasSuffix(text, "\n") {
		lineCount++
	}
	truncatedLines := lineCount > maxGrepOutputLines
	if truncatedLines {
		lines := strings.SplitAfter(text, "\n")
		text = strings.Join(lines[:maxGrepOutputLines], "")
	}
	if w.truncated || truncatedLines {
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		if w.truncated {
			text += fmt.Sprintf("... [truncated at %d bytes]\n", maxGrepOutputBytes)
		}
		if truncatedLines {
			text += fmt.Sprintf("... [truncated at %d lines]\n", maxGrepOutputLines)
		}
	}
	return text, truncatedLines
}
