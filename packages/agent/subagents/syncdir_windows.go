//go:build windows

package subagents

// Windows does not support syncing a directory handle through os.File.Sync.
func syncDirectory(string) error { return nil }
