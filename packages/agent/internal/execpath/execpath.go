// Package execpath resolves session executables without consulting process state.
package execpath

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Resolve searches env's PATH for bare names, resolving relative entries against
// cwd. Windows uses env's PATHEXT (the standard executable extensions if absent).
// Nil env preserves exec.LookPath behavior. Explicit paths are returned unchanged.
func Resolve(name, cwd string, env []string) (string, error) {
	if filepath.IsAbs(name) || strings.ContainsAny(name, `/\`) {
		return name, nil
	}
	if env == nil {
		return exec.LookPath(name)
	}
	lookup := func(key string) string {
		for i := len(env) - 1; i >= 0; i-- {
			k, v, ok := strings.Cut(env[i], "=")
			if ok && (k == key || runtime.GOOS == "windows" && strings.EqualFold(k, key)) {
				return v
			}
		}
		return ""
	}
	path := lookup("PATH")
	extensions := []string{""}
	if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
		ext := lookup("PATHEXT")
		if ext == "" {
			ext = ".COM;.EXE;.BAT;.CMD"
		}
		extensions = strings.Split(ext, ";")
	}
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) {
			if !filepath.IsAbs(cwd) {
				return "", fmt.Errorf("session executable cwd must be absolute: %q", cwd)
			}
			dir = filepath.Join(cwd, dir)
		}
		for _, ext := range extensions {
			candidate := filepath.Join(dir, name+ext)
			info, err := os.Stat(candidate)
			if err == nil && !info.IsDir() && (runtime.GOOS == "windows" || info.Mode()&0111 != 0) {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("executable %q not found in session PATH", name)
}
