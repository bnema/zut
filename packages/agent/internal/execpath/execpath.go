// Package execpath resolves session executables without consulting process state.
package execpath

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
)

// Resolve returns a concrete absolute executable filename for name. Bare names
// are searched in env's PATH (relative entries resolved against cwd); explicit
// paths are resolved against cwd. Windows applies env's PATHEXT (the standard
// executable extensions if absent), normalized as os/exec does. Nil env
// preserves exec.LookPath behavior and returns explicit paths unchanged.
func Resolve(name, cwd string, env []string) (string, error) {
	if env == nil {
		if strings.ContainsAny(name, `/\`) || isAbs(runtime.GOOS == "windows", name) {
			return name, nil
		}
		return exec.LookPath(name)
	}
	windows := runtime.GOOS == "windows"
	return resolve(windows, name, cwd, env, func(file string) bool {
		info, err := os.Stat(file)
		return err == nil && !info.IsDir() && (windows || info.Mode()&0o111 != 0)
	})
}

// resolve is the platform-parameterized core of Resolve; windows selects
// Windows path, separator, environment, and PATHEXT rules.
func resolve(windows bool, name, cwd string, env []string, isExecutable func(string) bool) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty session executable name")
	}
	lookup := func(key string) string { return lookupEnv(windows, env, key) }
	var exts []string
	if windows {
		exts = pathExts(lookup("PATHEXT"))
	}
	check := func(file string) (string, bool) {
		for _, candidate := range candidates(windows, file, exts) {
			if isExecutable(candidate) {
				return candidate, true
			}
		}
		return "", false
	}
	if hasSeparator(windows, name) {
		file, err := absolute(windows, name, cwd)
		if err != nil {
			return "", err
		}
		if found, ok := check(file); ok {
			return found, nil
		}
		return "", fmt.Errorf("session executable %q not found", name)
	}
	for _, dir := range splitList(windows, lookup("PATH")) {
		if dir == "" {
			continue
		}
		dir, err := absolute(windows, dir, cwd)
		if err != nil {
			return "", err
		}
		if found, ok := check(join(windows, dir, name)); ok {
			return found, nil
		}
	}
	return "", fmt.Errorf("executable %q not found in session PATH", name)
}

// LookupEnv returns the last value for key in env. Keys compare
// case-insensitively on Windows, matching the process environment there.
func LookupEnv(env []string, key string) string {
	value, _ := LookupEnvOK(env, key)
	return value
}

// LookupEnvOK returns the last value for key in env and whether it is present.
// Keys compare case-insensitively on Windows. Nil env contains no entries.
func LookupEnvOK(env []string, key string) (string, bool) {
	return lookupEnvOK(runtime.GOOS == "windows", env, key)
}

func lookupEnv(windows bool, env []string, key string) string {
	value, _ := lookupEnvOK(windows, env, key)
	return value
}

func lookupEnvOK(windows bool, env []string, key string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		k, v, ok := strings.Cut(env[i], "=")
		if ok && (k == key || windows && strings.EqualFold(k, key)) {
			return v, true
		}
	}
	return "", false
}

// pathExts normalizes a PATHEXT value like os/exec: lowercased, empty entries
// dropped, a leading dot added where missing, and Go's default list used
// when nothing remains.
func pathExts(value string) []string {
	var exts []string
	for _, e := range strings.Split(strings.ToLower(value), ";") {
		if e == "" {
			continue
		}
		if e[0] != '.' {
			e = "." + e
		}
		exts = append(exts, e)
	}
	if len(exts) == 0 {
		return []string{".com", ".exe", ".bat", ".cmd"}
	}
	return exts
}

// candidates lists filenames to probe for file: on Windows the name itself
// when it already has an extension, then each PATHEXT suffix (so dotted names
// like server.v1 still fall back to server.v1.exe).
func candidates(windows bool, file string, exts []string) []string {
	if !windows || len(exts) == 0 {
		return []string{file}
	}
	var out []string
	if ext(windows, file) != "" {
		out = append(out, file)
	}
	for _, e := range exts {
		out = append(out, file+e)
	}
	return out
}

func isSep(windows bool, c byte) bool { return c == '/' || windows && c == '\\' }

func hasSeparator(windows bool, name string) bool {
	if windows {
		return strings.ContainsAny(name, `/\:`)
	}
	return strings.Contains(name, "/")
}

func ext(windows bool, file string) string {
	for i := len(file) - 1; i >= 0 && !isSep(windows, file[i]); i-- {
		if file[i] == '.' {
			return file[i:]
		}
	}
	return ""
}

// volume returns the Windows drive ("C:") or UNC ("\\host\share") prefix.
func volume(p string) string {
	if len(p) >= 2 && p[1] == ':' && ('a' <= p[0]|0x20 && p[0]|0x20 <= 'z') {
		return p[:2]
	}
	if len(p) >= 2 && isSep(true, p[0]) && isSep(true, p[1]) {
		n := 0
		for i := 2; i < len(p); i++ {
			if isSep(true, p[i]) {
				if n++; n == 2 {
					return p[:i]
				}
			}
		}
		return p
	}
	return ""
}

func isAbs(windows bool, p string) bool {
	if !windows {
		return strings.HasPrefix(p, "/")
	}
	v := volume(p)
	if v == "" {
		return false
	}
	if len(v) > 2 { // UNC
		return true
	}
	return len(p) > 2 && isSep(true, p[2])
}

func join(windows bool, dir, name string) string {
	if !windows {
		return path.Join(dir, name)
	}
	return cleanWindows(dir + `\` + name)
}

// cleanWindows normalizes separators to '\' and removes "." and ".."
// elements of an absolute Windows path, keeping its volume.
func cleanWindows(p string) string {
	p = strings.ReplaceAll(p, "/", `\`)
	v := volume(p)
	var parts []string
	for _, e := range strings.Split(p[len(v):], `\`) {
		switch e {
		case "", ".":
		case "..":
			if len(parts) > 0 {
				parts = parts[:len(parts)-1]
			}
		default:
			parts = append(parts, e)
		}
	}
	return v + `\` + strings.Join(parts, `\`)
}

// absolute resolves p against cwd using the platform's volume semantics.
func absolute(windows bool, p, cwd string) (string, error) {
	if isAbs(windows, p) {
		if windows {
			return cleanWindows(p), nil
		}
		return path.Clean(p), nil
	}
	if !isAbs(windows, cwd) {
		return "", fmt.Errorf("session executable cwd must be absolute: %q", cwd)
	}
	if !windows {
		return path.Join(cwd, p), nil
	}
	cv := volume(cwd)
	if v := volume(p); v != "" { // drive-relative, e.g. C:tool
		if !strings.EqualFold(v, cv) {
			return "", fmt.Errorf("session executable %q is relative to another drive's current directory", p)
		}
		return join(true, cwd, p[len(v):]), nil
	}
	if isSep(true, p[0]) { // rooted on cwd's volume
		return cleanWindows(cv + p), nil
	}
	return join(true, cwd, p), nil
}

// splitList splits a PATH value; Windows uses ';' and strips quotes like
// filepath.SplitList.
func splitList(windows bool, value string) []string {
	if value == "" {
		return nil
	}
	if !windows {
		return strings.Split(value, ":")
	}
	var out []string
	start, quoted := 0, false
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '"':
			quoted = !quoted
		case ';':
			if !quoted {
				out = append(out, value[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, value[start:])
	for i, s := range out {
		out[i] = strings.ReplaceAll(s, `"`, "")
	}
	return out
}
