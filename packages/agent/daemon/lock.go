package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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
	l, err := LockHome(home)
	if errors.Is(err, flock.ErrBusy) {
		return nil, ErrAlreadyRunning
	}
	return l, err
}

// LockHome takes the daemon's per-home lock without modifying an existing lock
// file. Both the daemon and offline maintenance use the same inode.
func LockHome(home string) (*flock.Lock, error) {
	dir := stateDir(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	expected, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !expected.IsDir() {
		return nil, errors.New("daemon lock directory is not a real directory")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(expected, actual) {
		return nil, errors.New("daemon lock directory changed")
	}
	const name = "daemon.lock"
	before, err := root.Lstat(name)
	f, err := openDaemonLockFile(root, name, before, err)
	if err != nil {
		return nil, err
	}
	return flock.TryLockFile(f)
}

// openDaemonLockFile opens name after an earlier Lstat result. A missing file
// is created exclusively; if a concurrent first start wins that race, the
// file is opened again as an existing lock with the same identity checks.
// An existing target is never created, truncated, or chmodded.
func openDaemonLockFile(root *os.Root, name string, before os.FileInfo, statErr error) (*os.File, error) {
	for attempt := 0; ; attempt++ {
		flags := os.O_RDWR
		if errors.Is(statErr, os.ErrNotExist) {
			flags |= os.O_CREATE | os.O_EXCL
			before = nil
		} else if statErr != nil {
			return nil, statErr
		} else if !before.Mode().IsRegular() {
			return nil, errors.New("daemon.lock is not a regular file")
		}
		f, err := root.OpenFile(name, flags, 0600)
		if errors.Is(err, os.ErrExist) && flags&os.O_EXCL != 0 && attempt == 0 {
			before, statErr = root.Lstat(name)
			continue
		}
		if err != nil {
			if attempt > 0 && errors.Is(err, os.ErrExist) {
				return nil, fmt.Errorf("daemon.lock changed while opening: %w", err)
			}
			return nil, err
		}
		info, infoErr := f.Stat()
		after, pathErr := root.Lstat(name)
		if infoErr != nil || pathErr != nil || !after.Mode().IsRegular() || !os.SameFile(info, after) || (before != nil && !os.SameFile(before, info)) {
			f.Close()
			return nil, errors.New("daemon.lock changed while opening")
		}
		return f, nil
	}
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

// ReadPID reads the daemon PID record. A record alone does not prove liveness;
// callers must check the daemon lock before signaling the process.
func ReadPID(home string) (int, error) {
	b, err := os.ReadFile(pidPath(home))
	if err != nil {
		return 0, fmt.Errorf("read daemon PID: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 || int64(pid) > 1<<31-1 {
		return 0, fmt.Errorf("invalid daemon PID %q", strings.TrimSpace(string(b)))
	}
	return pid, nil
}
