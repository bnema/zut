package daemonclient

import (
	"fmt"
	"path/filepath"
	"runtime"
)

// SocketPath resolves the daemon socket from getenv (P0.3): ZUT_DAEMON_SOCKET
// overrides; Windows uses %LOCALAPPDATA%\zut\run\daemon.sock; other systems use
// $XDG_RUNTIME_DIR/zut/daemon.sock. Both fall back to $ZUT_HOME/run/daemon.sock
// when their base variable is unset.
func SocketPath(getenv func(string) string) string {
	return socketPath(runtime.GOOS, getenv)
}

func socketPath(goos string, getenv func(string) string) string {
	join := filepath.Join
	if goos == "windows" {
		join = windowsJoin
	}
	if v := getenv("ZUT_DAEMON_SOCKET"); v != "" {
		return v
	}
	if goos == "windows" {
		if v := getenv("LOCALAPPDATA"); v != "" {
			return join(v, "zut", "run", "daemon.sock")
		}
	} else if v := getenv("XDG_RUNTIME_DIR"); v != "" {
		return join(v, "zut", "daemon.sock")
	}
	return join(zutHome(goos, getenv), "run", "daemon.sock")
}

// ZutHome mirrors agent.ZutHome over getenv. It is duplicated here because the
// agent package will import daemonclient.
func ZutHome(getenv func(string) string) string { return zutHome(runtime.GOOS, getenv) }

func zutHome(goos string, getenv func(string) string) string {
	join := filepath.Join
	if goos == "windows" {
		join = windowsJoin
	}
	if v := getenv("ZUT_HOME"); v != "" {
		return v
	}
	if v := getenv("XDG_STATE_HOME"); v != "" {
		return join(v, "zut")
	}
	home := getenv("HOME")
	switch goos {
	case "darwin":
		if home != "" {
			return join(home, "Library", "Application Support", "zut")
		}
	case "windows":
		if v := getenv("LOCALAPPDATA"); v != "" {
			return join(v, "zut")
		}
		home = getenv("USERPROFILE")
	}
	if home != "" {
		return join(home, ".local", "state", "zut")
	}
	return ".zut"
}

// windowsJoin joins with backslashes so resolution is testable on any host.
func windowsJoin(parts ...string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" && out[len(out)-1] != '\\' && out[len(out)-1] != '/' {
			out += `\`
		}
		out += p
	}
	return out
}

// checkSocketPath rejects paths longer than the platform's sun_path allows
// (104 bytes on darwin/BSD, 108 elsewhere, both including the NUL).
func checkSocketPath(path string) error { return checkSocketPathFor(runtime.GOOS, path) }

func checkSocketPathFor(goos, path string) error {
	limit := 108
	switch goos {
	case "windows":
		return nil
	case "darwin", "ios", "freebsd", "openbsd", "netbsd", "dragonfly":
		limit = 104
	}
	if len(path) >= limit {
		return fmt.Errorf("daemon socket path %q is %d bytes, over the %s limit of %d; set ZUT_DAEMON_SOCKET to a shorter path", path, len(path), goos, limit-1)
	}
	return nil
}
