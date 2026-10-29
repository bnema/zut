package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/bnema/zut/packages/agent/internal/execpath"
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
	return execpath.LookupEnv(env, key)
}

// lookPathEnv resolves executable discovery against the session PATH and
// cwd, never the daemon's working directory or environment. A nil env fails
// closed rather than falling back to the process PATH.
func lookPathEnv(cwd string, env []string, name string) (string, error) {
	if !isAbsSessionPath(runtime.GOOS, cwd) {
		return "", fmt.Errorf("executable %q: session cwd must be absolute", name)
	}
	if env == nil {
		env = []string{}
	}
	return execpath.Resolve(name, cwd, env)
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
