//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package procgroup

import (
	"errors"
	"golang.org/x/sys/unix"
)

// EVFILT_PROC observes exit without consuming the child's wait status. The
// caller owns reaping, so even an already-exited leader reserves its PID while
// we register. If registration fails, the guard cleans before allowing Wait.
func waitLeaderExited(pid int) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(kq)
	var change unix.Kevent_t
	unix.SetKevent(&change, pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ENABLE|unix.EV_ONESHOT)
	change.Fflags = unix.NOTE_EXIT
	for {
		_, err = unix.Kevent(kq, []unix.Kevent_t{change}, nil, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	events := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(kq, nil, events, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n > 0 {
			if events[0].Flags&unix.EV_ERROR != 0 {
				return unix.Errno(events[0].Data)
			}
			return nil
		}
	}
}

func liveGroupMembers(int) (live, known bool) { return false, false }
