package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
)

// setupCrashOutput preserves runtime diagnostics even when the terminal owns
// stderr. SetCrashOutput duplicates the descriptor, so our file can close now.
func setupCrashOutput(home, process string, logger *slog.Logger) {
	path := filepath.Join(home, "logs", process+"-crash.log")
	err := os.MkdirAll(filepath.Dir(path), 0700)
	if err == nil {
		var f *os.File
		f, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err == nil {
			err = debug.SetCrashOutput(f, debug.CrashOptions{})
			_ = f.Close()
		}
	}
	if err != nil && logger != nil {
		logger.Warn("set crash output", "err", err)
	}
}
