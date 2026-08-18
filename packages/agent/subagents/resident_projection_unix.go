//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package subagents

import (
	"bytes"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func residentProjectionCurrent(path string, data []byte) bool {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return false
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return false
	}
	current, err := io.ReadAll(file)
	return err == nil && bytes.Equal(current, data)
}
