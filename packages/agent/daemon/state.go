package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

func statePath(home string) string { return filepath.Join(stateDir(home), "state.json") }

// writeFileAtomic writes a 0600 file via temp+rename in a 0700 directory and
// fsyncs the parent directory on Unix so the rename is durable.
func writeFileAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if err = tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("rename state file: %w", err)
	}
	if runtime.GOOS != "windows" {
		d, derr := os.Open(dir)
		if derr != nil {
			return fmt.Errorf("open state dir: %w", derr)
		}
		serr := d.Sync()
		_ = d.Close()
		if serr != nil {
			return fmt.Errorf("sync state dir: %w", serr)
		}
	}
	return nil
}
