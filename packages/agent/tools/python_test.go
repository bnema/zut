package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
)

func pythonArgsJSON(t *testing.T, code string, timeout int64) json.RawMessage {
	t.Helper()
	return mustJSON(t, map[string]any{"code": code, "timeout": timeout})
}

func stubPythonTool(resolved resolvedPython, resolveErr error, outcome pythonExecOutcome, execErr error) *PythonTool {
	return &PythonTool{
		CWD: "/session",
		resolveHook: func(context.Context, string, string) (resolvedPython, error) {
			return resolved, resolveErr
		},
		execHook: func(context.Context, string, string, string, func(string)) (pythonExecOutcome, error) {
			return outcome, execErr
		},
	}
}

func TestPythonSchemaRequiresCodeAndTimeout(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			Type        string `json:"type"`
			Minimum     *int64 `json:"minimum"`
			Maximum     *int64 `json:"maximum"`
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal((&PythonTool{}).Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	timeout, ok := schema.Properties["timeout"]
	if !ok || timeout.Type != "integer" || timeout.Minimum == nil || *timeout.Minimum != 1 || timeout.Maximum == nil || *timeout.Maximum != maxPythonTimeoutSeconds {
		t.Fatalf("timeout schema = %+v", timeout)
	}
	if _, ok := schema.Properties["code"]; !ok {
		t.Fatal("code property missing")
	}
	required := map[string]bool{}
	for _, name := range schema.Required {
		required[name] = true
	}
	if !required["code"] || !required["timeout"] {
		t.Fatalf("required = %v", schema.Required)
	}
	if (&PythonTool{}).Name() != "python" {
		t.Fatalf("name = %q", (&PythonTool{}).Name())
	}
}

func TestPythonExecArgvDirect(t *testing.T) {
	argv := pythonExecArgv("/usr/bin/python3")
	if len(argv) != 4 || argv[0] != "/usr/bin/python3" || argv[1] != "-u" || argv[2] != "-B" || argv[3] != "-" {
		t.Fatalf("argv = %q, want [exe -u -B -]", argv)
	}
}

func TestPythonRejectsInvalidArgs(t *testing.T) {
	tool := stubPythonTool(resolvedPython{}, nil, pythonExecOutcome{}, nil)
	cases := []struct {
		name string
		raw  json.RawMessage
		want string
	}{
		{"blank code", mustJSON(t, map[string]any{"code": "  ", "timeout": 5}), "code is required"},
		{"missing timeout", mustJSON(t, map[string]any{"code": "print(1)"}), "timeout is required"},
		{"zero timeout", mustJSON(t, map[string]any{"code": "print(1)", "timeout": 0}), "timeout must be between 1 and"},
		{"negative timeout", mustJSON(t, map[string]any{"code": "print(1)", "timeout": -2}), "timeout must be between 1 and"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tool.Execute(context.Background(), tc.raw, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestPythonDeniedNeverResolves(t *testing.T) {
	resolveCalls := 0
	execCalls := 0
	mk := func(sandbox *Sandbox) *PythonTool {
		return &PythonTool{
			CWD:     "/session",
			Sandbox: sandbox,
			resolveHook: func(context.Context, string, string) (resolvedPython, error) {
				resolveCalls++
				return resolvedPython{Path: "/usr/bin/python3"}, nil
			},
			execHook: func(context.Context, string, string, string, func(string)) (pythonExecOutcome, error) {
				execCalls++
				return pythonExecOutcome{}, nil
			},
		}
	}
	jailed := NewSandbox("/session")
	jailed.Lock()
	if _, err := mk(jailed).Execute(context.Background(), pythonArgsJSON(t, "print(1)", 5), nil); err == nil || !strings.Contains(err.Error(), "cannot be confined") {
		t.Fatalf("jail err = %v", err)
	}
	restricted := NewSandbox("/session")
	restricted.SetPermissions(&PermissionSet{})
	if _, err := mk(restricted).Execute(context.Background(), pythonArgsJSON(t, "print(1)", 5), nil); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("permission err = %v", err)
	}
	if resolveCalls != 0 || execCalls != 0 {
		t.Fatalf("denied calls must never resolve or execute (resolve=%d exec=%d)", resolveCalls, execCalls)
	}
}

func TestPythonMissingInterpreterActionable(t *testing.T) {
	tool := &PythonTool{
		CWD: "/session",
		resolveHook: func(context.Context, string, string) (resolvedPython, error) {
			return resolvedPython{}, missingPythonError()
		},
		execHook: func(context.Context, string, string, string, func(string)) (pythonExecOutcome, error) {
			t.Fatal("missing interpreter must not execute")
			return pythonExecOutcome{}, nil
		},
	}
	_, err := tool.Execute(context.Background(), pythonArgsJSON(t, "print(1)", 5), nil)
	if err == nil || !strings.Contains(err.Error(), "existing Python 3") || !strings.Contains(err.Error(), "never installs") {
		t.Fatalf("err = %v, want actionable guidance", err)
	}
}

func TestPythonSuccessResult(t *testing.T) {
	interp := resolvedPython{Path: "/usr/bin/python3", Version: pythonVersion{Major: 3, Minor: 12, Micro: 1}}
	tool := stubPythonTool(interp, nil, pythonExecOutcome{Captured: "hi\n", ExitCode: 0}, nil)
	res, err := tool.Execute(context.Background(), pythonArgsJSON(t, "print('hi')", 5), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatal("unexpected IsError")
	}
	text := res.Content[0].(provider.TextBlock).Text
	for _, want := range []string{"python 3.12.1", "/usr/bin/python3", "hi", "[exit 0]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("result missing %q:\n%s", want, text)
		}
	}
	details := res.Details.(map[string]any)
	if details["exit_code"] != 0 || details["interpreter"] != "/usr/bin/python3" || details["python_version"] != "3.12.1" || details["bytes_truncated"] != false || details["lines_truncated"] != false {
		t.Fatalf("details = %#v", details)
	}
	if path, _ := details["full_output_path"].(string); path != "" || strings.Contains(text, "full output") {
		t.Fatalf("untruncated result must not persist a full-output file: %q", path)
	}
}

func TestPythonFailureStatuses(t *testing.T) {
	interp := resolvedPython{Path: "/py", Version: pythonVersion{Major: 3}}
	for _, tc := range []struct {
		name    string
		outcome pythonExecOutcome
		want    string
	}{
		{"nonzero", pythonExecOutcome{Captured: "boom\n", ExitCode: 3}, "[exit 3]"},
		{"timeout", pythonExecOutcome{Captured: "", ExitCode: -1, TimedOut: true}, "[timed out after 5 seconds]"},
		{"cancelled", pythonExecOutcome{Captured: "", ExitCode: -1, Cancelled: true}, "[cancelled]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := stubPythonTool(interp, nil, tc.outcome, nil)
			res, err := tool.Execute(context.Background(), pythonArgsJSON(t, "x", 5), nil)
			if err != nil {
				t.Fatal(err)
			}
			if !res.IsError {
				t.Fatal("want IsError")
			}
			if got := res.Content[0].(provider.TextBlock).Text; !strings.Contains(got, tc.want) {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestPythonTruncationMarkedWithFullOutput(t *testing.T) {
	interp := resolvedPython{Path: "/py", Version: pythonVersion{Major: 3, Minor: 1, Micro: 0}}
	long := strings.Repeat("x\n", maxPythonLines+10)
	tool := stubPythonTool(interp, nil, pythonExecOutcome{Captured: long, ExitCode: 0}, nil)
	res, err := tool.Execute(context.Background(), pythonArgsJSON(t, "x", 5), nil)
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(provider.TextBlock).Text
	if !strings.Contains(text, "truncated at 2000 lines") {
		t.Fatalf("lines truncation not marked:\n%s", text[len(text)-500:])
	}
	details := res.Details.(map[string]any)
	if details["lines_truncated"] != true {
		t.Fatalf("details = %#v", res.Details)
	}
	fullPath, _ := details["full_output_path"].(string)
	if fullPath == "" || !strings.Contains(text, "full output: "+fullPath) {
		t.Fatalf("truncated result must reference a full-output file: %q\n%s", fullPath, text)
	}
	t.Cleanup(func() { _ = os.Remove(fullPath) })
	persisted, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(persisted) != long {
		t.Fatal("full-output file must hold the byte-capped buffer beyond line trimming")
	}

	big := strings.Repeat("y", maxPythonBytes+100)
	// The stub bypasses capture, so trim the fixture the way the real
	// runner would: byte-capped, then line-trimmed for display.
	big = big[:maxPythonBytes]
	tool = stubPythonTool(interp, nil, pythonExecOutcome{Captured: big, ExitCode: 0, BytesTrunc: true}, nil)
	res, err = tool.Execute(context.Background(), pythonArgsJSON(t, "x", 5), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Content[0].(provider.TextBlock).Text; !strings.Contains(got, "truncated at 51200 bytes") {
		t.Fatalf("bytes truncation not marked")
	}
	if path, _ := res.Details.(map[string]any)["full_output_path"].(string); path == "" {
		t.Fatal("byte-truncated result must persist a full-output file")
	} else {
		t.Cleanup(func() { _ = os.Remove(path) })
	}
}

func TestPythonProgressStreamsMergedOutput(t *testing.T) {
	interp := resolvedPython{Path: "/py", Version: pythonVersion{Major: 3}}
	var calls []string
	tool := &PythonTool{
		CWD: "/session",
		resolveHook: func(context.Context, string, string) (resolvedPython, error) {
			return interp, nil
		},
		execHook: func(_ context.Context, _, _, _ string, progress func(string)) (pythonExecOutcome, error) {
			progress("out\n")
			progress("err\n")
			return pythonExecOutcome{Captured: "out\nerr\n", ExitCode: 0}, nil
		},
	}
	res, err := tool.Execute(context.Background(), pythonArgsJSON(t, "x", 5), func(s string) {
		calls = append(calls, s)
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, "") != "out\nerr\n" {
		t.Fatalf("progress = %q", calls)
	}
	if got := res.Content[0].(provider.TextBlock).Text; !strings.Contains(got, "out") || !strings.Contains(got, "err") {
		t.Fatalf("merged output missing:\n%s", got)
	}
}

func TestAppendPythonChunkBoundaries(t *testing.T) {
	var buf bytes.Buffer
	if appendPythonChunk(&buf, bytes.Repeat([]byte("y"), maxPythonBytes)) {
		t.Fatal("output exactly filling the budget must not report truncation")
	}
	if buf.Len() != maxPythonBytes {
		t.Fatalf("buffer len = %d, want %d", buf.Len(), maxPythonBytes)
	}
	// One byte over discards exactly that byte.
	if !appendPythonChunk(&buf, []byte("!")) {
		t.Fatal("overflow byte must report truncation")
	}
	if buf.Len() != maxPythonBytes || buf.Bytes()[buf.Len()-1] == '!' {
		t.Fatal("overflow byte must not be retained")
	}
	// A chunk straddling the boundary keeps its fitting prefix and
	// reports the discarded remainder.
	buf.Reset()
	if appendPythonChunk(&buf, bytes.Repeat([]byte("a"), maxPythonBytes-1)) {
		t.Fatal("sub-budget write must not report truncation")
	}
	if !appendPythonChunk(&buf, []byte("bc")) {
		t.Fatal("straddling write must report its discarded remainder")
	}
	if buf.Len() != maxPythonBytes || buf.Bytes()[buf.Len()-1] != 'b' {
		t.Fatal("straddling write must retain only its fitting prefix")
	}
	if appendPythonChunk(&buf, nil) {
		t.Fatal("empty chunk must not report truncation")
	}
}

func TestPythonTimeoutBounds(t *testing.T) {
	if _, err := pythonTimeoutDuration(0); err == nil {
		t.Fatal("want error for 0")
	}
	if _, err := pythonTimeoutDuration(maxPythonTimeoutSeconds + 1); err == nil {
		t.Fatal("want error above maximum")
	}
	if d, err := pythonTimeoutDuration(2); err != nil || d != 2*time.Second {
		t.Fatalf("d = %s, err = %v", d, err)
	}
}

func TestPythonResolutionSharesTimeoutBudget(t *testing.T) {
	interp := resolvedPython{Path: "/py", Version: pythonVersion{Major: 3}}
	tool := &PythonTool{
		CWD: "/session",
		resolveHook: func(ctx context.Context, _ string, _ string) (resolvedPython, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("resolution must share the invocation deadline")
			}
			return interp, nil
		},
		execHook: func(ctx context.Context, _, _, _ string, _ func(string)) (pythonExecOutcome, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("execution must share the invocation deadline")
			}
			return pythonExecOutcome{ExitCode: 0}, nil
		},
	}
	if _, err := tool.Execute(context.Background(), pythonArgsJSON(t, "x", 5), nil); err != nil {
		t.Fatal(err)
	}
}

func TestPythonExecErrorPropagates(t *testing.T) {
	tool := stubPythonTool(
		resolvedPython{Path: "/py", Version: pythonVersion{Major: 3}},
		nil, pythonExecOutcome{}, errors.New("start: boom"),
	)
	if _, err := tool.Execute(context.Background(), pythonArgsJSON(t, "x", 5), nil); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}
