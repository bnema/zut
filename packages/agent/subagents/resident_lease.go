package subagents

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const residentLeaseName = "owner.lock"

// ErrResidentLeaseBusy means another zut process owns this resident journal.
// Callers must not read the authoritative transcript or mutate its projections.
var ErrResidentLeaseBusy = errors.New("resident journal: owned by another process")

// residentLease keeps the OS-owned exclusive lock alive for a resident's full
// writer lifetime. The lock file is deliberately stable: deleting or replacing
// it would permit separate processes to lock different filesystem objects.
type residentLease struct {
	file *os.File
}

func acquireResidentLease(dir string) (*residentLease, error) {
	file, err := os.OpenFile(filepath.Join(dir, residentLeaseName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("resident journal lease: %w", err)
	}
	_ = file.Chmod(0o600)
	if err := lockResidentLease(file); err != nil {
		_ = file.Close()
		if errors.Is(err, ErrResidentLeaseBusy) {
			return nil, err
		}
		return nil, fmt.Errorf("resident journal lease lock: %w", err)
	}
	return &residentLease{file: file}, nil
}

func (l *residentLease) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	unlockErr := unlockResidentLease(file)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}
