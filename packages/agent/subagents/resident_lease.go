package subagents

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/bnema/zut/packages/agent/internal/flock"
)

const residentLeaseName = "owner.lock"

// ErrResidentLeaseBusy means another zut process owns this resident journal.
// Callers must not read the authoritative transcript or mutate its projections.
var ErrResidentLeaseBusy = errors.New("resident journal: owned by another process")

// residentLease keeps the OS-owned exclusive lock alive for a resident's full
// writer lifetime. The lock file is deliberately stable: deleting or replacing
// it would permit separate processes to lock different filesystem objects.
type residentLease struct {
	lock *flock.Lock
}

func acquireResidentLease(dir string) (*residentLease, error) {
	lock, err := flock.TryLock(filepath.Join(dir, residentLeaseName))
	if err != nil {
		if errors.Is(err, flock.ErrBusy) {
			return nil, ErrResidentLeaseBusy
		}
		return nil, fmt.Errorf("resident journal lease: %w", err)
	}
	return &residentLease{lock: lock}, nil
}

func (l *residentLease) Close() error {
	if l == nil || l.lock == nil {
		return nil
	}
	lock := l.lock
	l.lock = nil
	return lock.Close()
}
