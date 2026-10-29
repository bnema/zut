package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// toolEnvKey carries the session environment through the worktree helper graph,
// including rollback contexts. Missing environments fail closed.
type toolEnvKey struct{}

func withToolEnv(ctx context.Context, env []string) context.Context {
	return context.WithValue(ctx, toolEnvKey{}, env)
}

func toolEnv(ctx context.Context) []string {
	if env, ok := ctx.Value(toolEnvKey{}).([]string); ok {
		return env
	}
	return nil
}

func envValue(env []string, key string) string {
	return envValueOS(runtime.GOOS, env, key)
}

// lookPathEnv resolves executable discovery against the session PATH and
// cwd, never the daemon's working directory or environment.
func lookPathEnv(cwd string, env []string, name string) (string, error) {
	return lookPathEnvOS(runtime.GOOS, cwd, env, name, isExecutableCandidate)
}

func isExecutableCandidate(goos, path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && (goos == "windows" || info.Mode()&0111 != 0)
}

// lookPathEnvOS holds the platform path rules so they can be tested on any host.
func lookPathEnvOS(goos, cwd string, env []string, name string, exists func(goos, path string) bool) (string, error) {
	if !isAbsSessionPath(goos, cwd) {
		return "", fmt.Errorf("executable %q: session cwd must be absolute", name)
	}
	if name == "" {
		return "", fmt.Errorf("executable name is empty")
	}
	names := []string{name}
	if goos == "windows" {
		names = windowsExecutableNames(env, name)
	}
	if hasPathSeparator(goos, name) {
		for _, candidate := range names {
			path := joinSessionPath(goos, cwd, candidate)
			if exists(goos, path) {
				return path, nil
			}
		}
		return "", fmt.Errorf("executable %q not found", name)
	}
	listSep := ":"
	if goos == "windows" {
		listSep = ";"
	}
	pathValue := envValueOS(goos, env, "PATH")
	if pathValue == "" {
		return "", fmt.Errorf("executable %q not found in session PATH", name)
	}
	for _, dir := range strings.Split(pathValue, listSep) {
		if goos == "windows" {
			dir = strings.Trim(dir, `"`)
		}
		if dir == "" {
			continue
		}
		base := joinSessionPath(goos, cwd, dir)
		for _, candidate := range names {
			path := joinSessionPath(goos, base, candidate)
			if exists(goos, path) {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("executable %q not found in session PATH", name)
}

// windowsExecutableNames mirrors PATHEXT semantics: a name that already ends
// in a PATHEXT extension is tried as-is first; other names, including dotted
// ones such as python3.12, get every PATHEXT extension appended.
func windowsExecutableNames(env []string, name string) []string {
	extensions := envValueOS("windows", env, "PATHEXT")
	if extensions == "" {
		extensions = ".COM;.EXE;.BAT;.CMD"
	}
	var exts []string
	for _, ext := range strings.Split(extensions, ";") {
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		exts = append(exts, ext)
	}
	var names []string
	lowerName := strings.ToLower(name)
	for _, ext := range exts {
		if strings.HasSuffix(lowerName, strings.ToLower(ext)) {
			names = append(names, name)
			break
		}
	}
	for _, ext := range exts {
		names = append(names, name+ext)
	}
	return names
}

func envValueOS(goos string, env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		k, v, ok := strings.Cut(env[i], "=")
		if ok && (k == key || goos == "windows" && strings.EqualFold(k, key)) {
			return v
		}
	}
	return ""
}

func hasPathSeparator(goos, name string) bool {
	if goos == "windows" {
		return strings.ContainsAny(name, `/\`) || windowsVolumeLen(name) > 0
	}
	return strings.Contains(name, "/")
}

// joinSessionPath anchors p at the absolute dir using goos path rules.
func joinSessionPath(goos, dir, p string) string {
	if isAbsSessionPath(goos, p) {
		return p
	}
	if goos == "windows" {
		if windowsVolumeLen(p) > 0 || strings.HasPrefix(p, `\`) || strings.HasPrefix(p, "/") {
			// Drive-relative or rooted-without-volume: never resolve against
			// another drive's process state; anchor under the session cwd.
			p = strings.TrimLeft(p[windowsVolumeLen(p):], `/\`)
		}
		return strings.TrimRight(dir, `/\`) + `\` + p
	}
	return strings.TrimRight(dir, "/") + "/" + p
}

// windowsVolumeLen returns the length of a leading "X:" drive designator.
func windowsVolumeLen(p string) int {
	if len(p) >= 2 && p[1] == ':' && ('a' <= p[0] && p[0] <= 'z' || 'A' <= p[0] && p[0] <= 'Z') {
		return 2
	}
	return 0
}

// isAbsSessionPath reports whether p is fully anchored for goos. On Windows a
// drive-relative path like C:project or a rooted path without a volume is
// not absolute.
func isAbsSessionPath(goos, p string) bool {
	if goos != "windows" {
		return strings.HasPrefix(p, "/")
	}
	if n := windowsVolumeLen(p); n > 0 {
		return len(p) > n && (p[n] == '\\' || p[n] == '/')
	}
	return len(p) > 2 && (strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//"))
}

// requireSessionCWD rejects relative and drive-relative directories before any
// filesystem access. Only clients may anchor cwd against process state.
func requireSessionCWD(name, cwd string) error {
	if cwd == "" {
		return fmt.Errorf("tool %s: session cwd not set", name)
	}
	if !filepath.IsAbs(cwd) || !isAbsSessionPath(runtime.GOOS, cwd) {
		return fmt.Errorf("tool %s: session cwd must be absolute", name)
	}
	return nil
}
