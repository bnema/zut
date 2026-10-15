//go:build linux

package mcp

import (
	"errors"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// waitLeaderExited blocks until pid (our direct child) has exited, leaving it
// unreaped (a zombie) so its pid, and with it the process-group id, cannot be
// reused. It reports false if the wait failed.
func waitLeaderExited(pid int) bool {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err == nil
	}
}

// liveGroupMembers reports whether any non-zombie process is in process group
// pgid, reading /proc. known is false when /proc cannot be read.
func liveGroupMembers(pgid int) (live, known bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, false
	}
	sawProc := false
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		sawProc = true
		raw, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // exited meanwhile
		}
		state, group, ok := parseProcStat(string(raw))
		if !ok || group != pgid || state == 'Z' || state == 'X' {
			continue
		}
		return true, true
	}
	return false, sawProc
}

// parseProcStat extracts the state and process-group fields of a
// /proc/<pid>/stat line. The command name is parenthesised and may itself
// contain spaces and parentheses, so fields are counted after the last ')'.
func parseProcStat(stat string) (state byte, pgrp int, ok bool) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, 0, false
	}
	fields := strings.Fields(stat[i+1:]) // state ppid pgrp ...
	if len(fields) < 3 || len(fields[0]) != 1 {
		return 0, 0, false
	}
	pg, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, false
	}
	return fields[0][0], pg, true
}
