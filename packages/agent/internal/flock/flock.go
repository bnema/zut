// Package flock provides non-blocking, process-exclusive advisory file locks.
//
// The lock file is never deleted or replaced by this package: removing it
// would let separate processes lock different filesystem objects.
package flock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrBusy reports that another owner already holds the lock.
var ErrBusy = errors.New("flock: lock is held by another owner")

// Lock is a held exclusive lock. Close releases it.
type Lock struct {
	file *os.File
	path string
}

// TryLock opens or creates path (0600, parent created 0700) and takes an
// exclusive lock without blocking. A held lock returns an error wrapping ErrBusy.
func TryLock(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("flock: create lock dir: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("flock: open lock file: %w", err)
	}
	_ = file.Chmod(0o600)
	if err := lockFile(file); err != nil {
		_ = file.Close()
		if errors.Is(err, ErrBusy) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("flock: lock: %w", err)
	}
	return &Lock{file: file, path: path}, nil
}

// Path returns the lock file path.
func (l *Lock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Close releases the lock. It is idempotent and nil-safe.
func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	return errors.Join(unlockFile(file), file.Close())
}
