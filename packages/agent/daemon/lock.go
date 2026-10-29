package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/bnema/zut/packages/agent/internal/flock"
)

// ErrAlreadyRunning reports that another daemon holds $ZUT_HOME/daemon/daemon.lock.
var ErrAlreadyRunning = errors.New("zut daemon is already running")

// ErrSessionFileBusy reports that another daemon owns a session file lock.
var ErrSessionFileBusy = errors.New("session file is open in another daemon")

func stateDir(home string) string { return filepath.Join(home, "daemon") }

// acquireDaemonLock takes the per-home daemon lock. It must be held before
// any socket cleanup or state mutation.
func acquireDaemonLock(home string) (*flock.Lock, error) {
	l, err := flock.TryLock(filepath.Join(stateDir(home), "daemon.lock"))
	if errors.Is(err, flock.ErrBusy) {
		return nil, ErrAlreadyRunning
	}
	return l, err
}

// lockSessionFile takes `<session>.jsonl.lock` next to a session transcript.
func lockSessionFile(sessionPath string) (*flock.Lock, error) {
	if sessionPath == "" {
		return nil, nil
	}
	l, err := flock.TryLock(sessionPath + ".lock")
	if errors.Is(err, flock.ErrBusy) {
		return nil, ErrSessionFileBusy
	}
	if err != nil {
		return nil, fmt.Errorf("lock session file: %w", err)
	}
	return l, nil
}

func pidPath(home string) string { return filepath.Join(stateDir(home), "daemon.pid") }

func writePID(home string) error {
	return writeFileAtomic(pidPath(home), []byte(strconv.Itoa(os.Getpid())+"\n"))
}
