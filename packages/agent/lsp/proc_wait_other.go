//go:build !linux && !windows

package lsp

// waitLeaderExited is unavailable here: the portable APIs reap the child. The
// guard then cleans up after the leader was reaped (see processGuard).
func waitLeaderExited(int) bool { return false }

// liveGroupMembers cannot inspect the process table portably.
func liveGroupMembers(int) (live, known bool) { return false, false }
