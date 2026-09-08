// Package tools Python runtime resolution.
//
// Resolution is lazy (per invocation, after permission checks) and shares
// the caller's timeout budget. Priority:
//
//  1. Configured python_interpreter (one executable path, no PATH lookup).
//  2. Active VIRTUAL_ENV.
//  3. Session-root .venv.
//  4. PATH candidates python3, then python.
//
// A configured or active environment that fails validation is an error,
// never a silent fallthrough. A present but invalid .venv is reported.
// A missing .venv permits system discovery. PATH candidates may fall
// through; cancellation stops discovery immediately.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// maxPythonProbeSeconds bounds each version probe. The probe also observes
// the remaining invocation deadline, whichever is shorter.
const maxPythonProbeSeconds = 5

// maxPythonProbeOutput bounds captured probe stdout/stderr.
const maxPythonProbeOutput = 64 * 1024

// pythonProbeCode prints structured version info using only the standard
// library. It runs under -I -S so user configuration cannot alter it.
const pythonProbeCode = `import json,sys;print(json.dumps({"major":sys.version_info[0],"minor":sys.version_info[1],"micro":sys.version_info[2],"executable":sys.executable}))`

// pythonProbeArgv builds the isolated probe argv for exe. -B suppresses
// bytecode-cache writes during probing.
func pythonProbeArgv(exe string) []string {
	return []string{exe, "-I", "-S", "-B", "-c", pythonProbeCode}
}

// pythonVersion is a validated Python 3 interpreter identity.
type pythonVersion struct {
	Major      int
	Minor      int
	Micro      int
	Executable string
}

func (v pythonVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Micro)
}

// resolvedPython is a validated interpreter ready for execution.
type resolvedPython struct {
	Path    string
	Version pythonVersion
}

// pythonResolveDeps are the injectable seams for resolution. Production
// code fills them from the host; tests substitute fixtures. No
// package-global mutable overrides.
type pythonResolveDeps struct {
	GOOS       string
	CWD        string
	Configured string
	Getenv     func(string) string
	LookPath   func(string) (string, error)
	IsFile     func(string) bool
	EvalSym    func(string) (string, error)
	Probe      func(ctx context.Context, exe string) (pythonVersion, error)
	// LayoutOK classifies a Windows candidate as a positively identified
	// installed interpreter (or venv). Nil uses the default filesystem
	// check. Ignored on non-Windows platforms.
	LayoutOK func(exe string) bool
}

func (d *pythonResolveDeps) goos() string {
	if d.GOOS != "" {
		return d.GOOS
	}
	return runtime.GOOS
}

func (d *pythonResolveDeps) getenv(k string) string {
	if d.Getenv != nil {
		return d.Getenv(k)
	}
	return os.Getenv(k)
}

func (d *pythonResolveDeps) lookPath(name string) (string, error) {
	if d.LookPath != nil {
		return d.LookPath(name)
	}
	return exec.LookPath(name)
}

func (d *pythonResolveDeps) isFile(path string) bool {
	if d.IsFile != nil {
		return d.IsFile(path)
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func (d *pythonResolveDeps) evalSym(path string) (string, error) {
	if d.EvalSym != nil {
		return d.EvalSym(path)
	}
	return filepath.EvalSymlinks(path)
}

func (d *pythonResolveDeps) probe(ctx context.Context, exe string) (pythonVersion, error) {
	if d.Probe != nil {
		return d.Probe(ctx, exe)
	}
	return probePythonVersion(ctx, exe)
}

func (d *pythonResolveDeps) layoutOK(exe string) bool {
	if d.LayoutOK != nil {
		return d.LayoutOK(exe)
	}
	return hasWindowsRuntimeLayout(exe, d.isFile)
}

// resolveConfiguredPythonPath maps a configured python_interpreter value to
// a filesystem path. Absolute paths are used directly; relative paths
// resolve against the session CWD. No PATH lookup, no shell expansion.
func resolveConfiguredPythonPath(cwd, configured string) string {
	if filepath.IsAbs(configured) {
		return filepath.Clean(configured)
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	return filepath.Join(cwd, filepath.FromSlash(configured))
}

// pythonVenvCandidate builds the interpreter path inside an environment
// root. Relative roots resolve against the session CWD.
func pythonVenvCandidate(cwd, root, goos string) string {
	if !filepath.IsAbs(root) {
		if cwd == "" {
			cwd, _ = os.Getwd()
		}
		root = filepath.Join(cwd, filepath.FromSlash(root))
	}
	if goos == "windows" {
		return filepath.Join(root, "Scripts", "python.exe")
	}
	return filepath.Join(root, "bin", "python")
}

// resolvePythonInterpreter implements the specified priority order.
func resolvePythonInterpreter(ctx context.Context, deps pythonResolveDeps) (resolvedPython, error) {
	goos := deps.goos()
	cwd := deps.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	if err := ctx.Err(); err != nil {
		return resolvedPython{}, err
	}

	// 1. Configured interpreter: fail closed, never fall through.
	if strings.TrimSpace(deps.Configured) != "" {
		path := resolveConfiguredPythonPath(cwd, strings.TrimSpace(deps.Configured))
		return resolvePythonFailClosed(ctx, &deps, goos, "configured python_interpreter", path)
	}

	// 2. Active VIRTUAL_ENV: fail closed, never fall through.
	if venv := strings.TrimSpace(deps.getenv("VIRTUAL_ENV")); venv != "" {
		path := pythonVenvCandidate(cwd, venv, goos)
		return resolvePythonFailClosed(ctx, &deps, goos, "active VIRTUAL_ENV", path)
	}

	// 3. Session-root .venv: missing permits system discovery, but a
	// present yet invalid environment is reported, not bypassed.
	if venvCandidate := pythonVenvCandidate(cwd, filepath.Join(cwd, ".venv"), goos); deps.isFile(venvCandidate) {
		return resolvePythonFailClosed(ctx, &deps, goos, "local .venv", venvCandidate)
	} else {
		// Also consider a .venv directory whose interpreter has a
		// version-suffixed name? No: only the canonical layout is valid.
		// A .venv dir without the canonical interpreter counts as
		// present-but-invalid.
		if dirExists(filepath.Join(cwd, ".venv")) {
			return resolvedPython{}, fmt.Errorf("local .venv at %q has no usable Python 3 interpreter at %q; create the environment or configure python_interpreter to an existing interpreter (zut never installs Python or packages)", filepath.Join(cwd, ".venv"), venvCandidate)
		}
	}

	// 4. PATH candidates: absent or non-Python-3 entries fall through.
	for _, name := range []string{"python3", "python"} {
		if err := ctx.Err(); err != nil {
			return resolvedPython{}, err
		}
		path, err := deps.lookPath(name)
		if err != nil || strings.TrimSpace(path) == "" {
			continue
		}
		// A relative PATH entry can yield a relative interpreter path.
		// The probe runs in the process directory while execution runs
		// in the session CWD, so anchor it: probe, classification, and
		// execution must all address the same validated executable.
		if abs, absErr := filepath.Abs(path); absErr == nil {
			path = abs
		}
		if goos == "windows" {
			if err := classifyWindowsCandidate(path, &deps); err != nil {
				continue
			}
		}
		ver, err := deps.probe(ctx, path)
		if err != nil {
			if ctx.Err() != nil {
				return resolvedPython{}, ctx.Err()
			}
			continue
		}
		return resolvedPython{Path: path, Version: ver}, nil
	}

	return resolvedPython{}, missingPythonError()
}

// resolvePythonFailClosed validates one explicit environment source. Any
// failure is returned; the caller must not fall through to another source.
func resolvePythonFailClosed(ctx context.Context, deps *pythonResolveDeps, goos, source, path string) (resolvedPython, error) {
	if err := ctx.Err(); err != nil {
		return resolvedPython{}, err
	}
	if !deps.isFile(path) {
		return resolvedPython{}, fmt.Errorf("%s %q is not usable: executable not found; point python_interpreter at an existing Python 3 interpreter (zut never installs Python or packages)", source, path)
	}
	if goos == "windows" {
		if err := classifyWindowsCandidate(path, deps); err != nil {
			return resolvedPython{}, fmt.Errorf("%s %q is not usable: %w; configure python_interpreter to an existing installed Python 3 interpreter", source, path, err)
		}
	}
	ver, err := deps.probe(ctx, path)
	if err != nil {
		if ctx.Err() != nil {
			return resolvedPython{}, ctx.Err()
		}
		return resolvedPython{}, fmt.Errorf("%s %q is not usable: %v; configure python_interpreter to an existing Python 3 interpreter (zut never installs Python or packages)", source, path, err)
	}
	return resolvedPython{Path: path, Version: ver}, nil
}

func missingPythonError() error {
	return fmt.Errorf("no Python 3 interpreter found: an existing Python 3 environment is required; configure python_interpreter in config.json, activate a VIRTUAL_ENV, create ./.venv, or add python3 to PATH (zut never installs Python or packages)")
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// classifyWindowsCandidate enforces the Windows safety policy for EVERY
// source, including configured paths and virtual environments. It runs
// before any probe or execution. Unknown wrappers fail closed.
func classifyWindowsCandidate(exe string, deps *pythonResolveDeps) error {
	target := exe
	if resolved, err := deps.evalSym(exe); err == nil && strings.TrimSpace(resolved) != "" {
		target = resolved
	}
	lowerTarget := strings.ToLower(strings.ReplaceAll(target, "\\", "/"))
	if strings.Contains(lowerTarget, "/windowsapps/") || strings.Contains(lowerTarget, "windowsapps") && strings.Contains(lowerTarget, "microsoft") {
		return fmt.Errorf("Windows Store / app-execution alias is not supported")
	}
	// filepath.Base is separator-sensitive to the host OS, so extract the
	// basename manually: test fixtures use Windows paths on Unix hosts.
	base := lowerTarget
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	switch base {
	case "py.exe", "pyw.exe", "pylauncher.exe":
		return fmt.Errorf("Python launcher %q is not supported; execute an installed interpreter directly", base)
	case "pymanager.exe", "python-manager.exe", "python-install-manager.exe", "install-manager.exe", "installmanager.exe":
		return fmt.Errorf("Python install manager %q is not supported", base)
	}
	if strings.Contains(base, "manager") && (strings.Contains(base, "python") || strings.Contains(base, "py") || strings.Contains(base, "install")) {
		return fmt.Errorf("Python install manager %q is not supported", base)
	}
	if !deps.layoutOK(exe) && !deps.layoutOK(target) {
		return fmt.Errorf("unverifiable interpreter wrapper %q is not supported", exe)
	}
	return nil
}

// hasWindowsRuntimeLayout positively identifies an installed CPython
// executable or a virtual-environment interpreter. Anything else fails
// closed. Never trust the basename alone.
func hasWindowsRuntimeLayout(exe string, isFile func(string) bool) bool {
	dir := filepath.Dir(exe)
	parent := filepath.Dir(dir)
	// Virtual environments carry pyvenv.cfg at the environment root.
	if isFile(filepath.Join(dir, "pyvenv.cfg")) || isFile(filepath.Join(parent, "pyvenv.cfg")) {
		return true
	}
	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, e := range entries {
			name := strings.ToLower(e.Name())
			if strings.HasPrefix(name, "python") && strings.HasSuffix(name, ".dll") {
				return true
			}
		}
	}
	// Installed layout also ships the standard library next to the exe.
	if isFile(filepath.Join(dir, "Lib", "os.py")) || isFile(filepath.Join(parent, "Lib", "os.py")) {
		return true
	}
	return false
}

// probePythonVersion runs the isolated version probe, bounded by at most
// five seconds and the remaining invocation deadline.
func probePythonVersion(ctx context.Context, exe string) (pythonVersion, error) {
	timeout := maxPythonProbeSeconds * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return pythonVersion{}, ctx.Err()
		}
		if remaining < timeout {
			timeout = remaining
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, exe, "-I", "-S", "-B", "-c", pythonProbeCode)
	cmd.Env = pythonChildEnv()
	cmd.Dir = ""
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedWriter{W: &stdout, N: maxPythonProbeOutput}
	cmd.Stderr = &limitedWriter{W: &stderr, N: maxPythonProbeOutput}
	closeOutput := configurePythonProcess(cmd, nil)
	defer closeOutput()
	if err := cmd.Start(); err != nil {
		return pythonVersion{}, fmt.Errorf("probe start: %w", err)
	}
	waitErr := cmd.Wait()
	if probeCtx.Err() != nil && ctx.Err() == nil {
		return pythonVersion{}, fmt.Errorf("probe timed out after %d seconds", int(timeout/time.Second))
	}
	if waitErr != nil {
		out := strings.TrimSpace(stdout.String() + stderr.String())
		if out != "" {
			if len(out) > 512 {
				out = out[:512]
			}
			return pythonVersion{}, fmt.Errorf("probe failed: %v: %s", waitErr, out)
		}
		return pythonVersion{}, fmt.Errorf("probe failed: %w", waitErr)
	}
	var raw struct {
		Major      int    `json:"major"`
		Minor      int    `json:"minor"`
		Micro      int    `json:"micro"`
		Executable string `json:"executable"`
	}
	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	if err := dec.Decode(&raw); err != nil {
		return pythonVersion{}, fmt.Errorf("probe returned invalid output")
	}
	if raw.Major != 3 {
		return pythonVersion{}, fmt.Errorf("probe returned Python %d (need Python 3)", raw.Major)
	}
	return pythonVersion{Major: raw.Major, Minor: raw.Minor, Micro: raw.Micro, Executable: raw.Executable}, nil
}

// pythonChildEnv inherits the host environment consistently with Bash,
// except for Windows launcher-manager opt-ins, which are stripped as
// defense in depth. Setting them to "0" is insufficient, so they are
// removed entirely. PYTHON_MANAGER_AUTOMATIC_INSTALL is never trusted.
func pythonChildEnv() []string {
	env := os.Environ()
	if runtime.GOOS != "windows" {
		return env
	}
	out := env[:0]
	for _, kv := range env {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		switch key {
		case "PYLAUNCHER_ALLOW_INSTALL", "PYLAUNCHER_ALWAYS_INSTALL":
			continue
		}
		out = append(out, kv)
	}
	return out
}

type limitedWriter struct {
	W *bytes.Buffer
	N int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.W.Len() >= l.N {
		return len(p), nil
	}
	room := l.N - l.W.Len()
	if len(p) > room {
		l.W.Write(p[:room])
		return len(p), nil
	}
	l.W.Write(p)
	return len(p), nil
}

var _ = io.Discard
