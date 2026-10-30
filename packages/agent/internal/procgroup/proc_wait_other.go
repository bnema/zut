//go:build !linux && !windows && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package procgroup

import "syscall"

// Without a non-reaping wait, clean the group before permitting cmd.Wait.
func waitLeaderExited(int) error              { return syscall.ENOSYS }
func liveGroupMembers(int) (live, known bool) { return false, false }
