package daemon

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

const maxLogSize int64 = 10 * 1024 * 1024

// LogPath returns $ZUT_HOME/logs/daemon.log.
func LogPath(home string) string { return filepath.Join(home, "logs", "daemon.log") }

// OpenLog opens the daemon log for appending (0600 file, 0700 directory) and
// returns a text logger writing to it and to extra writers (e.g. stderr).
// ZUT_DAEMON_LOG selects debug, info (default), warn, or error verbosity.
// The caller closes the returned io.Closer after the daemon stops.
func OpenLog(home string, extra ...io.Writer) (*slog.Logger, io.Closer, error) {
	path := LogPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, nil, fmt.Errorf("create log dir: %w", err)
	}
	w := &rotatingLog{path: path}
	if err := w.open(); err != nil {
		return nil, nil, err
	}
	if w.size > maxLogSize {
		if err := w.rotate(); err != nil {
			_ = w.Close()
			return nil, nil, err
		}
	}
	level := slog.LevelInfo
	switch os.Getenv("ZUT_DAEMON_LOG") {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	// MultiWriter stops at the first failing writer, so the file goes last:
	// a log file problem never silences stderr.
	out := io.MultiWriter(append(append([]io.Writer{}, extra...), w)...)
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level})), w, nil
}

// rotatingLog serializes writes, rotation, and close across handler calls.
// One backup is retained; a single oversized record is never split.
type rotatingLog struct {
	mu   sync.Mutex
	path string
	file *os.File
	size int64
	// rotateErr is the last rotation failure; the log keeps growing past
	// maxLogSize until a later rotation succeeds.
	rotateErr error
}

func (w *rotatingLog) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open daemon log: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("secure daemon log: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat daemon log: %w", err)
	}
	w.file, w.size = f, st.Size()
	return nil
}

// rotate moves the log to the backup and opens a fresh file. When the move
// fails it reopens the original path and keeps appending, so logging never
// stops; the move error is recorded in rotateErr. It returns an error only
// when no file could be reopened.
func (w *rotatingLog) rotate() error {
	_ = w.file.Close()
	w.file = nil
	w.rotateErr = w.moveToBackup()
	return w.open()
}

func (w *rotatingLog) moveToBackup() error {
	// Remove explicitly because replacing an existing destination via Rename
	// is not supported on every platform.
	if err := os.Remove(w.path + ".1"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove daemon log backup: %w", err)
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil {
		return fmt.Errorf("rotate daemon log: %w", err)
	}
	return nil
}

func (w *rotatingLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.size > 0 && int64(len(p)) > maxLogSize-w.size {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingLog) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
