package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

const (
	maxBashLines = 2000
	maxBashBytes = 50 * 1024

	// maxBashTimeoutSeconds is the largest whole-second timeout that can be
	// converted to a time.Duration without overflowing it.
	maxBashTimeoutSeconds int64 = int64(time.Duration(1<<63-1) / time.Second)
)

// BashTool runs a shell command in the agent's cwd.
type BashTool struct {
	CWD     string
	Sandbox *Sandbox
}

type bashArgs struct {
	Command string `json:"command"`
	Timeout *int64 `json:"timeout"`
}

var bashSchema = fmt.Sprintf(`{"type":"object","properties":{"command":{"type":"string"},"timeout":{"type":"integer","minimum":1,"maximum":%d,"description":"Maximum command runtime in seconds."}},"required":["command","timeout"]}`, maxBashTimeoutSeconds)

func (t *BashTool) Name() string            { return "bash" }
func (t *BashTool) Description() string     { return shellDescription(currentShell()) }
func (t *BashTool) Schema() json.RawMessage { return json.RawMessage(bashSchema) }

func bashTimeoutDuration(seconds int64) (time.Duration, error) {
	if seconds < 1 || seconds > maxBashTimeoutSeconds {
		return 0, fmt.Errorf("timeout must be between 1 and %d seconds", maxBashTimeoutSeconds)
	}
	return time.Duration(seconds) * time.Second, nil
}

func (t *BashTool) Execute(ctx context.Context, raw json.RawMessage, progress func(string)) (core.ToolResult, error) {
	var a bashArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return core.ToolResult{}, fmt.Errorf("invalid args: %w", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return core.ToolResult{}, fmt.Errorf("command is required")
	}
	if a.Timeout == nil {
		return core.ToolResult{}, fmt.Errorf("timeout is required")
	}
	timeout, err := bashTimeoutDuration(*a.Timeout)
	if err != nil {
		return core.ToolResult{}, err
	}
	if err := t.Sandbox.CheckCommand(a.Command); err != nil {
		return core.ToolResult{}, err
	}
	if err := t.Sandbox.CheckBashPermission(a.Command); err != nil {
		return core.ToolResult{}, err
	}
	cwd := t.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := newShellCmd(runCtx, a.Command)
	cmd.Dir = cwd
	cmd.Env = os.Environ()

	// Capture merged stdout+stderr with line-by-line streaming.
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	closeOutput := configureBashProcess(cmd, pw)

	if err := cmd.Start(); err != nil {
		closeOutput()
		return core.ToolResult{}, fmt.Errorf("start: %w", err)
	}

	// Writer to both the buffer (trimmed) and progress callback.
	captured := &bytes.Buffer{}
	readerDone := make(chan struct{})

	go func() {
		defer close(readerDone)
		buf := make([]byte, 4096)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				if captured.Len() < maxBashBytes {
					room := maxBashBytes - captured.Len()
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

	output := captured.String()
	truncBytes := captured.Len() >= maxBashBytes
	lines := strings.Split(output, "\n")
	truncLines := false
	if len(lines) > maxBashLines {
		lines = lines[:maxBashLines]
		truncLines = true
	}
	trimmed := strings.Join(lines, "\n")

	exitCode := 0
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}

	// Terminal-log style: echo the command on the first line with
	// a shell-prompt prefix, a blank line, the captured output, and
	// a footer showing the exit code. Invocation timing is added once
	// by the core tool boundary for the model-only projection.
	var sb strings.Builder
	fmt.Fprintf(&sb, "$ %s\n", a.Command)
	if trimmed != "" {
		sb.WriteString("\n")
		sb.WriteString(trimmed)
		if !strings.HasSuffix(trimmed, "\n") {
			sb.WriteString("\n")
		}
	}
	if truncLines {
		fmt.Fprintf(&sb, "... [truncated at %d lines]\n", maxBashLines)
	}
	if truncBytes {
		fmt.Fprintf(&sb, "... [truncated at %d bytes]\n", maxBashBytes)
	}
	sb.WriteString("\n")
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		fmt.Fprintf(&sb, "[timed out after %d second", *a.Timeout)
		if *a.Timeout != 1 {
			sb.WriteByte('s')
		}
		sb.WriteByte(']')
	} else if exitCode == 0 {
		fmt.Fprintf(&sb, "[exit 0]")
	} else {
		fmt.Fprintf(&sb, "[exit %d]", exitCode)
	}

	var fullPath string
	if truncBytes || truncLines {
		fullPath = writeFullOutput(output)
		if fullPath != "" {
			fmt.Fprintf(&sb, " (full output: %s)", fullPath)
		}
	}

	isErr := exitCode != 0 || runCtx.Err() != nil
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: sb.String()}},
		IsError: isErr,
		Details: map[string]any{
			"exit_code":        exitCode,
			"full_output_path": fullPath,
			"lines_truncated":  truncLines,
			"bytes_truncated":  truncBytes,
		},
	}, nil
}

func writeFullOutput(s string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := filepath.Join(os.TempDir(), "zut-bash-"+hex.EncodeToString(b)+".log")
	if err := os.WriteFile(name, []byte(s), 0o600); err != nil {
		return ""
	}
	return name
}

type shellCommand struct {
	path   string
	flag   string
	isBash bool
}

func currentShell() shellCommand {
	return resolveShell(runtime.GOOS, isExecutableFile, exec.LookPath)
}

func resolveShell(goos string, executable func(string) bool, lookPath func(string) (string, error)) shellCommand {
	if goos == "windows" {
		return shellCommand{path: "cmd", flag: "/C"}
	}
	if executable("/bin/bash") {
		return shellCommand{path: "/bin/bash", flag: "-c", isBash: true}
	}
	if path, err := lookPath("bash"); err == nil {
		return shellCommand{path: path, flag: "-c", isBash: true}
	}
	return shellCommand{path: "/bin/sh", flag: "-c"}
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
}

func shellDescription(shell shellCommand) string {
	if shell.flag == "/C" {
		return "Run a Windows Command Prompt command via cmd /C. stdout+stderr merged."
	}
	if shell.isBash {
		return fmt.Sprintf("Run a Bash command via %s -c. stdout+stderr merged.", shell.path)
	}
	return "Bash is unavailable; run a POSIX sh command via /bin/sh -c. stdout+stderr merged."
}

func newShellCmd(ctx context.Context, command string) *exec.Cmd {
	shell := currentShell()
	return exec.CommandContext(ctx, shell.path, shell.flag, command)
}
