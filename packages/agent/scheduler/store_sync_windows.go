//go:build windows

package scheduler

// Windows does not support syncing directory handles through os.File.Sync.
func syncDirectory(string) error { return nil }
