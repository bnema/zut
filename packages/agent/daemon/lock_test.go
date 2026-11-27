package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// A concurrent first start can create daemon.lock between this process's
// Lstat and its exclusive create. The loser must open the existing file.
func TestOpenDaemonLockFileRetriesExistingAfterCreateRace(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()
	before, statErr := root.Lstat("daemon.lock")
	require.ErrorIs(t, statErr, os.ErrNotExist)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "daemon.lock"), nil, 0640))

	f, err := openDaemonLockFile(root, "daemon.lock", before, statErr)
	require.NoError(t, err)
	defer f.Close()
	info, err := os.Lstat(filepath.Join(dir, "daemon.lock"))
	require.NoError(t, err)
	opened, err := f.Stat()
	require.NoError(t, err)
	require.True(t, os.SameFile(info, opened))
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0640), info.Mode().Perm(), "existing lock was chmodded")
	}
}

func TestOpenDaemonLockFileRaceRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()
	before, statErr := root.Lstat("daemon.lock")
	require.ErrorIs(t, statErr, os.ErrNotExist)
	target := filepath.Join(t.TempDir(), "target")
	require.NoError(t, os.WriteFile(target, nil, 0600))
	if err := os.Symlink(target, filepath.Join(dir, "daemon.lock")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	_, err = openDaemonLockFile(root, "daemon.lock", before, statErr)
	require.ErrorContains(t, err, "not a regular file")
}
