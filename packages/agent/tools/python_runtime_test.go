package tools

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testPythonDeps() pythonResolveDeps {
	return pythonResolveDeps{
		GOOS: "linux",
		CWD:  "/session",
		Getenv: func(string) string {
			return ""
		},
		LookPath: func(string) (string, error) {
			return "", errors.New("not found")
		},
		IsFile: func(string) bool { return false },
		EvalSym: func(s string) (string, error) {
			return s, nil
		},
		LayoutOK: func(string) bool { return true },
	}
}

func TestPythonProbeArgvIsolated(t *testing.T) {
	argv := pythonProbeArgv("/usr/bin/python3")
	if len(argv) != 6 {
		t.Fatalf("argv = %q, want 6 elements", argv)
	}
	if argv[0] != "/usr/bin/python3" {
		t.Fatalf("argv[0] = %q", argv[0])
	}
	joined := strings.Join(argv[1:], " ")
	for _, want := range []string{"-I", "-S", "-B", "-c"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv missing %q: %q", want, joined)
		}
	}
	// Bytecode suppression must be explicit. The probe code itself contains
	// a ";" but stays a single argv element: execution is direct, never a
	// concatenated shell command.
	foundB := false
	for i, a := range argv {
		if a == "-B" {
			foundB = true
		}
		if i < len(argv)-1 && (strings.Contains(a, "&&") || a == "|" || a == ";") {
			t.Fatalf("probe argv must not contain shell metacharacters: %q", argv)
		}
	}
	if !foundB {
		t.Fatalf("probe argv missing -B: %q", argv)
	}
	if !strings.Contains(argv[5], "sys.version_info") || !strings.Contains(argv[5], "json.dumps") {
		t.Fatalf("probe code must emit structured JSON: %q", argv[5])
	}
}

func TestResolveConfiguredPathHandling(t *testing.T) {
	if got := resolveConfiguredPythonPath("/session", "/opt/py/bin/python"); got != "/opt/py/bin/python" {
		t.Fatalf("absolute = %q", got)
	}
	if got := resolveConfiguredPythonPath("/session", "envs/py/bin/python"); got != filepath.Join("/session", "envs/py/bin/python") {
		t.Fatalf("relative = %q", got)
	}
	// No shell expansion: ~ and $VAR stay literal path segments.
	if got := resolveConfiguredPythonPath("/session", "~/bin/python"); strings.Contains(got, string(os.PathSeparator)+"root") || !strings.Contains(got, "~") {
		t.Fatalf("must not expand ~: %q", got)
	}
	if got := resolveConfiguredPythonPath("/session", "$HOME/bin/python"); !strings.Contains(got, "$HOME") {
		t.Fatalf("must not expand env vars: %q", got)
	}
	// Paths with spaces survive.
	if got := resolveConfiguredPythonPath("/my dir", "env dir/py"); got != filepath.Join("/my dir", "env dir/py") {
		t.Fatalf("spaces = %q", got)
	}
}

func TestResolveConfiguredMissingNoFallback(t *testing.T) {
	deps := testPythonDeps()
	deps.Configured = "/opt/missing/python"
	lookedUp := false
	deps.LookPath = func(string) (string, error) {
		lookedUp = true
		return "/usr/bin/python3", nil
	}
	probed := false
	deps.Probe = func(context.Context, string) (pythonVersion, error) {
		probed = true
		return pythonVersion{Major: 3}, nil
	}
	_, err := resolvePythonInterpreter(context.Background(), deps)
	if err == nil || !strings.Contains(err.Error(), "configured python_interpreter") {
		t.Fatalf("err = %v, want configured failure", err)
	}
	if lookedUp || probed {
		t.Fatalf("missing configured interpreter must not fall through (lookup=%v probe=%v)", lookedUp, probed)
	}
}

func TestResolveConfiguredPython2NoFallback(t *testing.T) {
	deps := testPythonDeps()
	deps.Configured = "/opt/python2/bin/python"
	deps.IsFile = func(s string) bool { return s == "/opt/python2/bin/python" }
	probes := 0
	deps.Probe = func(_ context.Context, exe string) (pythonVersion, error) {
		probes++
		if exe != "/opt/python2/bin/python" {
			t.Fatalf("probed %q", exe)
		}
		return pythonVersion{}, errors.New("probe returned Python 2 (need Python 3)")
	}
	deps.LookPath = func(string) (string, error) {
		t.Fatal("must not consult PATH after configured failure")
		return "", errors.New("not found")
	}
	_, err := resolvePythonInterpreter(context.Background(), deps)
	if err == nil || !strings.Contains(err.Error(), "configured python_interpreter") {
		t.Fatalf("err = %v", err)
	}
	if probes != 1 {
		t.Fatalf("probes = %d, want 1", probes)
	}
}

func TestResolveVirtualEnvCandidateConstruction(t *testing.T) {
	if got := pythonVenvCandidate("/s", "/opt/venv", "linux"); got != "/opt/venv/bin/python" {
		t.Fatalf("unix = %q", got)
	}
	if got := pythonVenvCandidate("/s", "/opt/venv", "windows"); got != filepath.Join("/opt/venv", "Scripts", "python.exe") {
		t.Fatalf("windows = %q", got)
	}
	if got := pythonVenvCandidate("/s cwd", "rel env", "linux"); got != filepath.Join("/s cwd", "rel env/bin/python") {
		t.Fatalf("relative root with spaces = %q", got)
	}
}

func TestResolveVirtualEnvTakesPriorityNoFallback(t *testing.T) {
	deps := testPythonDeps()
	deps.Getenv = func(k string) string {
		if k == "VIRTUAL_ENV" {
			return "/opt/venv"
		}
		return ""
	}
	deps.IsFile = func(s string) bool { return false }
	_, err := resolvePythonInterpreter(context.Background(), deps)
	if err == nil || !strings.Contains(err.Error(), "active VIRTUAL_ENV") {
		t.Fatalf("err = %v, want VIRTUAL_ENV failure without fallback", err)
	}
}

func TestResolveLocalVenvInvalidReported(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".venv"), 0o755); err != nil {
		t.Fatal(err)
	}
	deps := testPythonDeps()
	deps.CWD = dir
	deps.LookPath = func(string) (string, error) {
		t.Fatal("present .venv must not fall through to PATH")
		return "", errors.New("not found")
	}
	_, err := resolvePythonInterpreter(context.Background(), deps)
	if err == nil || !strings.Contains(err.Error(), ".venv") {
		t.Fatalf("err = %v, want .venv failure", err)
	}
}

func TestResolvePathFallbackPython2ThenPython3(t *testing.T) {
	deps := testPythonDeps()
	deps.LookPath = func(name string) (string, error) {
		if name == "python3" {
			return "/usr/bin/python3", nil
		}
		if name == "python" {
			return "/usr/bin/python", nil
		}
		return "", errors.New("not found")
	}
	deps.Probe = func(_ context.Context, exe string) (pythonVersion, error) {
		if exe == "/usr/bin/python3" {
			return pythonVersion{}, errors.New("probe returned Python 2 (need Python 3)")
		}
		return pythonVersion{Major: 3, Minor: 12, Micro: 1}, nil
	}
	got, err := resolvePythonInterpreter(context.Background(), deps)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/usr/bin/python" || got.Version.String() != "3.12.1" {
		t.Fatalf("got %+v", got)
	}
}

func TestResolvePathMissingGivesGuidance(t *testing.T) {
	deps := testPythonDeps()
	_, err := resolvePythonInterpreter(context.Background(), deps)
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"existing Python 3", "python_interpreter", "never installs"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %q", want, err)
		}
	}
}

func TestResolveCancellationStopsDiscovery(t *testing.T) {
	deps := testPythonDeps()
	deps.LookPath = func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	}
	deps.Probe = func(context.Context, string) (pythonVersion, error) {
		t.Fatal("cancelled discovery must not probe")
		return pythonVersion{}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := resolvePythonInterpreter(ctx, deps)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestResolveProbeDeadlineRespected(t *testing.T) {
	deps := testPythonDeps()
	deps.Configured = "/opt/py/bin/python"
	deps.IsFile = func(string) bool { return true }
	deps.Probe = func(ctx context.Context, _ string) (pythonVersion, error) {
		<-ctx.Done()
		return pythonVersion{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := resolvePythonInterpreter(ctx, deps)
	if err == nil {
		t.Fatal("want deadline error")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("probe did not respect deadline")
	}
}

func TestLimitedWriterBoundsProbeOutput(t *testing.T) {
	var buf bytes.Buffer
	w := &limitedWriter{W: &buf, N: 8}
	n, err := w.Write([]byte("1234567890abcdef"))
	if err != nil || n != 16 {
		t.Fatalf("Write = %d, %v, want 16, nil", n, err)
	}
	if buf.Len() != 8 || buf.String() != "12345678" {
		t.Fatalf("bounded buffer = %q, want %q", buf.String(), "12345678")
	}
	// Further writes are dropped but report success.
	if n, _ := w.Write([]byte("more")); n != 4 || buf.Len() != 8 {
		t.Fatalf("overflow write = %d len %d", n, buf.Len())
	}
}

func TestWindowsRejectsStoreAliasBothNames(t *testing.T) {
	for _, name := range []string{"python3", "python"} {
		deps := testPythonDeps()
		deps.GOOS = "windows"
		deps.CWD = `C:\proj`
		alias := `C:\Users\test\AppData\Local\Microsoft\WindowsApps\` + name + `.exe`
		deps.LookPath = func(n string) (string, error) {
			if n == name {
				return alias, nil
			}
			return "", errors.New("not found")
		}
		deps.EvalSym = func(s string) (string, error) { return s, nil }
		deps.LayoutOK = func(string) bool { return true }
		deps.Probe = func(context.Context, string) (pythonVersion, error) {
			t.Fatalf("alias %q must be rejected before any probe", alias)
			return pythonVersion{}, nil
		}
		_, err := resolvePythonInterpreter(context.Background(), deps)
		if err == nil || !strings.Contains(err.Error(), "existing Python 3") {
			t.Fatalf("%s: err = %v, want missing-interpreter fallback", name, err)
		}
	}
}

func TestWindowsRejectsLauncherAndManagerBeforeProbe(t *testing.T) {
	cases := map[string]string{
		"launcher": `C:\Windows\py.exe`,
		"manager":  `C:\Program Files\Python\python-manager.exe`,
		"wrapper":  `C:\tools\weird-python.exe`,
	}
	for kind, exe := range cases {
		deps := testPythonDeps()
		deps.GOOS = "windows"
		deps.Configured = exe
		deps.IsFile = func(string) bool { return true }
		deps.EvalSym = func(s string) (string, error) { return s, nil }
		deps.LayoutOK = func(p string) bool {
			// Only the wrapper case reaches layout verification with false.
			return kind == "launcher" || kind == "manager"
		}
		probed := false
		deps.Probe = func(context.Context, string) (pythonVersion, error) {
			probed = true
			return pythonVersion{Major: 3}, nil
		}
		_, err := resolvePythonInterpreter(context.Background(), deps)
		if err == nil {
			t.Fatalf("%s %q accepted", kind, exe)
		}
		if probed && kind != "launcher" && kind != "manager" {
			// Wrapper must also be rejected before probing.
			t.Fatalf("%s %q probed an unverifiable wrapper", kind, exe)
		}
		if probed {
			t.Fatalf("%s %q must be rejected before any probe", kind, exe)
		}
	}
}

func TestWindowsSymlinkTargetClassified(t *testing.T) {
	deps := testPythonDeps()
	deps.GOOS = "windows"
	deps.Configured = `C:\tools\python.exe`
	deps.IsFile = func(string) bool { return true }
	deps.EvalSym = func(string) (string, error) {
		return `C:\Users\test\AppData\Local\Microsoft\WindowsApps\python.exe`, nil
	}
	deps.LayoutOK = func(string) bool { return true }
	deps.Probe = func(context.Context, string) (pythonVersion, error) {
		t.Fatal("symlink target alias must be rejected before probe")
		return pythonVersion{}, nil
	}
	_, err := resolvePythonInterpreter(context.Background(), deps)
	if err == nil || !strings.Contains(err.Error(), "Store") {
		t.Fatalf("err = %v, want Store rejection via resolved target", err)
	}
}

func TestWindowsNeverUsesPyLauncherFallback(t *testing.T) {
	deps := testPythonDeps()
	deps.GOOS = "windows"
	seen := []string{}
	deps.LookPath = func(name string) (string, error) {
		seen = append(seen, name)
		return "", errors.New("not found")
	}
	_, _ = resolvePythonInterpreter(context.Background(), deps)
	for _, name := range seen {
		if name == "py" || name == "pyw" {
			t.Fatalf("resolver must never consult the py launcher, saw %q", seen)
		}
	}
}

func TestResolvePathResultAbsolutized(t *testing.T) {
	// A relative PATH entry can yield a relative interpreter path. The
	// probe runs in the process directory while execution runs in the
	// session CWD, so resolution must anchor it: probing, classification,
	// and execution must address the same executable.
	rel := filepath.Join("rel", "bin", "python3")
	wantAbs, err := filepath.Abs(rel)
	if err != nil {
		t.Fatal(err)
	}
	deps := testPythonDeps()
	deps.LookPath = func(name string) (string, error) {
		if name == "python3" {
			return rel, nil
		}
		return "", errors.New("not found")
	}
	deps.IsFile = func(s string) bool { return s == wantAbs }
	var probed string
	deps.Probe = func(_ context.Context, exe string) (pythonVersion, error) {
		probed = exe
		return pythonVersion{Major: 3, Minor: 1, Micro: 0}, nil
	}
	got, err := resolvePythonInterpreter(context.Background(), deps)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got.Path) || got.Path != wantAbs {
		t.Fatalf("resolved path = %q, want absolutized %q", got.Path, wantAbs)
	}
	if probed != got.Path {
		t.Fatalf("probed %q but returned %q", probed, got.Path)
	}
}

func TestPythonChildEnvKeepsHostOnUnix(t *testing.T) {
	t.Setenv("ZUT_PYTHON_TEST_SENTINEL", "kept")
	found := false
	for _, kv := range pythonChildEnv() {
		if kv == "ZUT_PYTHON_TEST_SENTINEL=kept" {
			found = true
		}
	}
	if !found {
		t.Fatal("host environment must be inherited")
	}
}
