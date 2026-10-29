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
// including rollback contexts. Standalone inventory helpers retain process env.
type toolEnvKey struct{}

func withToolEnv(ctx context.Context, env []string) context.Context {
	return context.WithValue(ctx, toolEnvKey{}, env)
}

func toolEnv(ctx context.Context) []string {
	if env, ok := ctx.Value(toolEnvKey{}).([]string); ok {
		return env
	}
	return os.Environ()
}

func envValue(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		k, v, ok := strings.Cut(env[i], "=")
		if ok && (k == key || runtime.GOOS == "windows" && strings.EqualFold(k, key)) {
			return v
		}
	}
	return ""
}

// sessionLookPath resolves executable discovery against the session PATH and
// cwd, never the daemon's working directory or environment.
func sessionLookPath(cwd string, env []string, name string) (string, error) {
	names := []string{name}
	if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
		names = nil
		extensions := envValue(env, "PATHEXT")
		if extensions == "" {
			extensions = ".COM;.EXE;.BAT;.CMD"
		}
		for ext := range strings.SplitSeq(extensions, ";") {
			names = append(names, name+ext)
		}
	}
	for _, dir := range filepath.SplitList(envValue(env, "PATH")) {
		for _, candidate := range names {
			path := resolvePath(cwd, filepath.Join(dir, candidate))
			info, err := os.Stat(path)
			if err == nil && !info.IsDir() && (runtime.GOOS == "windows" || info.Mode()&0111 != 0) {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("executable %q not found in session PATH", name)
}
