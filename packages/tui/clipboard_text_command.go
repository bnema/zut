package tui

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

type clipboardTextCommand struct {
	name string
	args []string
}

var errClipboardCommandUnavailable = errors.New("no supported clipboard command found")

// readClipboardTextCommands tries each installed command in order. Command
// stdout is the clipboard payload and is never included in returned errors.
func readClipboardTextCommands(commands ...clipboardTextCommand) (string, bool, error) {
	var lastErr error
	found := false
	for _, candidate := range commands {
		path, err := exec.LookPath(candidate.name)
		if err != nil {
			continue
		}
		found = true
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := exec.CommandContext(ctx, path, candidate.args...).Output()
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		if len(out) == 0 {
			return "", false, nil
		}
		return string(out), true, nil
	}
	if !found {
		return "", false, errClipboardCommandUnavailable
	}
	return "", false, lastErr
}

// writeClipboardTextCommands tries each installed command in order. Text is
// supplied on stdin so it never becomes part of an executable command line.
func writeClipboardTextCommands(text string, commands ...clipboardTextCommand) error {
	var lastErr error
	found := false
	for _, candidate := range commands {
		path, err := exec.LookPath(candidate.name)
		if err != nil {
			continue
		}
		found = true
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, path, candidate.args...)
		cmd.Stdin = strings.NewReader(text)
		err = cmd.Run()
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	if !found {
		return errClipboardCommandUnavailable
	}
	return lastErr
}
