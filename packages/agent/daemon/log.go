package daemon

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// LogPath returns $ZUT_HOME/logs/daemon.log.
func LogPath(home string) string { return filepath.Join(home, "logs", "daemon.log") }

// OpenLog opens the daemon log for appending (0600 file, 0700 directory) and
// returns a text logger writing to it and to extra writers (e.g. stderr).
// The caller closes the returned io.Closer after the daemon stops.
func OpenLog(home string, extra ...io.Writer) (*slog.Logger, io.Closer, error) {
	path := LogPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, nil, fmt.Errorf("create log dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open daemon log: %w", err)
	}
	_ = f.Chmod(0o600)
	w := io.MultiWriter(append([]io.Writer{f}, extra...)...)
	return slog.New(slog.NewTextHandler(w, nil)), f, nil
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
