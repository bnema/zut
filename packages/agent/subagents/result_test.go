package subagents

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTurnResultValidationAndBoundedOutput(t *testing.T) {
	result := &TurnResult{
		Version:    ProtocolVersion,
		AgentID:    "agent-1",
		TurnID:     "turn-1",
		Status:     ResultSucceeded,
		Output:     "one\ntwo\nthree",
		Structured: json.RawMessage(`{"ok":true}`),
	}
	if err := result.Validate(100, 3); err != nil {
		t.Fatal(err)
	}
	bounded := result.Bounded(32, 2)
	if bounded == result || !strings.Contains(bounded.Output, "truncated") {
		t.Fatalf("bounded result = %#v", bounded)
	}
	if result.Output != "one\ntwo\nthree" {
		t.Fatal("Bounded mutated the original result")
	}
}

func TestTurnResultValidatesShutdownOrigin(t *testing.T) {
	result := &TurnResult{
		Version: ProtocolVersion, AgentID: "a", TurnID: "t", Status: ResultCanceled,
		ShutdownOrigin: ShutdownOriginSession,
	}
	if err := result.Validate(0, 0); err != nil {
		t.Fatalf("known shutdown origin was rejected: %v", err)
	}
	result.ShutdownOrigin = ShutdownOrigin("private detail")
	if err := result.Validate(0, 0); err == nil {
		t.Fatal("unknown shutdown origin was accepted")
	}
}

func TestTurnResultBoundedHonorsLineAndUTF8Limits(t *testing.T) {
	result := &TurnResult{
		Version: ProtocolVersion, AgentID: "a", TurnID: "t", Status: ResultSucceeded,
		Output: "😀\n第二行\n第三行",
	}
	bounded := result.Bounded(10, 2)
	if countLines(bounded.Output) > 2 {
		t.Fatalf("bounded lines = %d: %q", countLines(bounded.Output), bounded.Output)
	}
	if len([]byte(bounded.Output)) > 10 {
		t.Fatalf("bounded bytes = %d: %q", len([]byte(bounded.Output)), bounded.Output)
	}
	if !utf8.ValidString(bounded.Output) {
		t.Fatal("bounded output is not valid UTF-8")
	}
}

func TestTruncateUTF8CutsOnlyIncompleteTrailingRune(t *testing.T) {
	invalidPrefix := string([]byte{0xff, 'a', 'b', 'c'})
	tests := []struct {
		name     string
		value    string
		maxBytes int
		want     string
	}{
		{
			name:     "multibyte",
			value:    "prefix😀suffix",
			maxBytes: len([]byte("prefix")) + 2,
			want:     "prefix",
		},
		{
			name:     "invalid byte before incomplete rune",
			value:    invalidPrefix + string([]byte{0xe2, 0x82}) + "tail",
			maxBytes: len([]byte(invalidPrefix)) + 2,
			want:     invalidPrefix,
		},
		{
			name:     "invalid byte before ascii cut",
			value:    invalidPrefix + "😀",
			maxBytes: len([]byte(invalidPrefix)),
			want:     invalidPrefix,
		},
		{
			name:     "invalid trailing byte is preserved",
			value:    string([]byte{'a', 0xff, 0x80}) + "😀",
			maxBytes: 3,
			want:     string([]byte{'a', 0xff, 0x80}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := truncateUTF8(tt.value, tt.maxBytes); got != tt.want {
				t.Fatalf("truncateUTF8() = %q (%x), want %q (%x)", got, got, tt.want, tt.want)
			}
		})
	}
}

func TestDecodeTurnResultEventValidatesIdentityAndStatus(t *testing.T) {
	invalid := NewEvent(EventTurnResult, map[string]any{
		"status":  "unknown",
		"turn_id": "turn-1",
	})
	if _, err := decodeTurnResultEvent(invalid, "agent-1", 100, 10); err == nil {
		t.Fatal("invalid result status was accepted")
	}
	mismatched := NewEvent(EventTurnResult, map[string]any{
		"agent_id": "other",
		"status":   string(ResultSucceeded),
		"turn_id":  "turn-1",
	})
	if _, err := decodeTurnResultEvent(mismatched, "agent-1", 100, 10); err == nil {
		t.Fatal("mismatched result agent was accepted")
	}
	valid := NewEvent(EventTurnResult, map[string]any{
		"status":          string(ResultCanceled),
		"shutdown_origin": ShutdownOriginSession,
	})
	valid.AgentID = "agent-1"
	valid.TurnID = "turn-1"
	result, err := decodeTurnResultEvent(valid, "agent-1", 100, 10)
	if err != nil || result.AgentID != "agent-1" || result.TurnID != "turn-1" || result.ShutdownOrigin != ShutdownOriginSession {
		t.Fatalf("valid result = %#v, err=%v", result, err)
	}
}

func TestTurnResultReferencesAndPersistence(t *testing.T) {
	if got := AgentRef("a"); got != "subagent://a" {
		t.Fatal(got)
	}
	if got := HistoryRef("a"); got != "subagent://a/history" {
		t.Fatal(got)
	}
	if got := ResultRef("a"); got != "subagent://a/result" {
		t.Fatal(got)
	}
	if got := PatchRef("a"); got != "subagent://a/patch" {
		t.Fatal(got)
	}
	state := t.TempDir()
	want := &TurnResult{AgentID: "a", TurnID: "t", Status: ResultFailed, Error: &ResultError{Code: "x", Message: "failed"}}
	if err := writeTurnResult(state, want); err != nil {
		t.Fatal(err)
	}
	got, err := readTurnResult(state)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != want.Status || got.Error == nil || got.Error.Code != "x" {
		t.Fatalf("got = %#v", got)
	}
	info, err := os.Stat(filepath.Join(state, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("result permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestWriteTurnResultCleansTemporaryFileAfterRenameFailure(t *testing.T) {
	state := t.TempDir()
	if err := os.Mkdir(resultPath(state), 0o700); err != nil {
		t.Fatal(err)
	}
	result := &TurnResult{AgentID: "a", TurnID: "t", Status: ResultSucceeded}
	if err := writeTurnResult(state, result); err == nil {
		t.Fatal("writeTurnResult succeeded when the result path was a directory")
	}
	if _, err := os.Stat(resultPath(state) + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temporary result file still exists, stat error = %v", err)
	}
}

func TestEnsureResultRecordsAvailabilityAfterDurableWrite(t *testing.T) {
	state := t.TempDir()
	trace := NewMemoryTraceWriter()
	t.Cleanup(func() { _ = trace.Close() })
	agent := &Agent{ID: "agent-1", stateDir: state, trace: trace, maxOutputBytes: 1024, maxOutputLines: 10}
	(&Supervisor{}).ensureResult(agent, StatusDone, nil)
	if err := trace.Flush(); err != nil {
		t.Fatal(err)
	}
	view := ProjectTrace(trace.Events())[agent.ID]
	if view.Result == nil || !view.Result.Available || view.Result.Ref != ResultRef(agent.ID) {
		t.Fatalf("result trace = %#v", view.Result)
	}
	if _, err := os.Stat(resultPath(state)); err != nil {
		t.Fatalf("durable result missing: %v", err)
	}
}

func TestCaptureWorkspaceFailureIsDurableAndPrivate(t *testing.T) {
	makeAgent := func(stateDir string, capture func() (WorkspaceCapture, error)) *Agent {
		return &Agent{ID: "agent-1", stateDir: stateDir, workspaceCapture: capture}
	}
	assertFailure := func(t *testing.T, supervisor *Supervisor, agent *Agent, stateDir string) {
		t.Helper()
		supervisor.captureWorkspace(agent)
		supervisor.ensureResult(agent, StatusDone, nil)
		checkCaptureFailureResult(t, agent, stateDir)
	}

	t.Run("stat", func(t *testing.T) {
		stateDir := filepath.Join(t.TempDir(), "missing")
		agent := makeAgent(stateDir, func() (WorkspaceCapture, error) {
			t.Fatal("workspace capture should not run when the state directory cannot be stat'ed")
			return WorkspaceCapture{}, nil
		})
		assertFailure(t, &Supervisor{}, agent, stateDir)
	})

	t.Run("capture", func(t *testing.T) {
		stateDir := t.TempDir()
		agent := makeAgent(stateDir, func() (WorkspaceCapture, error) {
			return WorkspaceCapture{}, errors.New("capture leaked /private/secret")
		})
		assertFailure(t, &Supervisor{}, agent, stateDir)
	})

	t.Run("temp write", func(t *testing.T) {
		stateDir := t.TempDir()
		if err := os.Mkdir(patchPath(stateDir)+".tmp", 0o700); err != nil {
			t.Fatal(err)
		}
		agent := makeAgent(stateDir, func() (WorkspaceCapture, error) {
			return WorkspaceCapture{Patch: []byte("patch")}, nil
		})
		assertFailure(t, &Supervisor{}, agent, stateDir)
	})

	t.Run("rename", func(t *testing.T) {
		stateDir := t.TempDir()
		if err := os.Mkdir(patchPath(stateDir), 0o700); err != nil {
			t.Fatal(err)
		}
		agent := makeAgent(stateDir, func() (WorkspaceCapture, error) {
			return WorkspaceCapture{Patch: []byte("patch")}, nil
		})
		assertFailure(t, &Supervisor{}, agent, stateDir)
		if _, err := os.Stat(patchPath(stateDir) + ".tmp"); !os.IsNotExist(err) {
			t.Fatalf("workspace temporary patch still exists, stat error = %v", err)
		}
	})
}

func TestCaptureWorkspaceSuccessPersistsPatch(t *testing.T) {
	stateDir := t.TempDir()
	agent := &Agent{
		ID:       "agent-1",
		stateDir: stateDir,
		workspaceCapture: func() (WorkspaceCapture, error) {
			return WorkspaceCapture{Patch: []byte("patch"), ChangedFiles: []string{"file.txt"}}, nil
		},
	}
	(&Supervisor{}).captureWorkspace(agent)

	patch, err := os.ReadFile(patchPath(stateDir))
	if err != nil {
		t.Fatal(err)
	}
	if string(patch) != "patch" {
		t.Fatalf("patch = %q, want %q", patch, "patch")
	}
	agent.lifecycleMu.Lock()
	patchRef := agent.patchRef
	changedFiles := append([]string(nil), agent.changedFiles...)
	agent.lifecycleMu.Unlock()
	if patchRef != PatchRef(agent.ID) || len(changedFiles) != 1 || changedFiles[0] != "file.txt" {
		t.Fatalf("capture metadata = ref %q, changed files %#v", patchRef, changedFiles)
	}
}

func checkCaptureFailureResult(t *testing.T, agent *Agent, stateDir string) {
	t.Helper()
	result := agent.Result()
	if result == nil || result.Status != ResultFailed || result.Error == nil {
		t.Fatalf("capture failure result = %#v", result)
	}
	if result.Error.Code != workspaceCaptureErrorCode || result.Error.Message != workspaceCaptureErrorMessage {
		t.Fatalf("capture failure error = %#v", result.Error)
	}
	if strings.Contains(result.Error.Message, stateDir) || strings.Contains(result.Error.Message, "secret") {
		t.Fatalf("capture failure leaked private data: %#v", result.Error)
	}
	durable, err := readTurnResult(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if durable.Error == nil || durable.Error.Code != workspaceCaptureErrorCode || durable.Status != ResultFailed {
		t.Fatalf("durable capture failure = %#v", durable)
	}
}

func TestEnsureResultReconcilesChildStatusWithRunnerError(t *testing.T) {
	tests := []struct {
		name               string
		status             Status
		runErr             error
		childCode          string
		shutdownOrigin     ShutdownOrigin
		wantShutdownOrigin ShutdownOrigin
		wantState          TurnStatus
		wantCode           string
		wantOutput         string
	}{
		{name: "runner failure", status: StatusFailed, runErr: errors.New("runner failed"), wantState: ResultFailed, wantCode: "runner_failed"},
		{name: "runner cancellation", status: StatusFailed, runErr: context.Canceled, wantState: ResultCanceled, wantCode: "runner_failed"},
		{name: "deadline preserves recovery guidance", status: StatusFailed, runErr: context.DeadlineExceeded, wantShutdownOrigin: ShutdownOriginDeadline, wantState: ResultFailed, wantCode: "deadline_exceeded", wantOutput: "partial answer"},
		{name: "deadline overrides child error", status: StatusFailed, runErr: context.DeadlineExceeded, childCode: "context_limit", wantShutdownOrigin: ShutdownOriginDeadline, wantState: ResultFailed, wantCode: "deadline_exceeded", wantOutput: "partial answer"},
		{name: "killed", status: StatusKilled, runErr: context.Canceled, wantState: ResultCanceled, wantCode: "runner_failed"},
		{name: "session shutdown", status: StatusKilled, runErr: context.Canceled, shutdownOrigin: ShutdownOriginSession, wantShutdownOrigin: ShutdownOriginSession, wantState: ResultCanceled, wantCode: "shutdown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stateDir := t.TempDir()
			childStatus := ResultSucceeded
			var childError *ResultError
			if tt.childCode != "" {
				childStatus = ResultFailed
				childError = &ResultError{Code: tt.childCode, Message: "child error"}
			}
			agent := &Agent{
				ID:             "agent-1",
				stateDir:       stateDir,
				lastAssistant:  tt.wantOutput,
				shutdownOrigin: tt.shutdownOrigin,
				result: &TurnResult{
					AgentID: "agent-1", TurnID: "turn-1", Status: childStatus, Error: childError,
				},
			}
			supervisor := &Supervisor{}
			supervisor.ensureResult(agent, tt.status, tt.runErr)
			result := agent.Result()
			if result == nil || result.Status != tt.wantState {
				t.Fatalf("result status = %#v, want %q", result, tt.wantState)
			}
			if result.Error == nil {
				t.Fatal("runner error was not recorded")
			}
			if result.Error.Code != tt.wantCode {
				t.Fatalf("result error code = %q, want %q", result.Error.Code, tt.wantCode)
			}
			if result.Output != tt.wantOutput {
				t.Fatalf("result output = %q, want %q", result.Output, tt.wantOutput)
			}
			durable, err := readTurnResult(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			if durable.Status != tt.wantState {
				t.Fatalf("durable result status = %q, want %q", durable.Status, tt.wantState)
			}
			if durable.Output != tt.wantOutput {
				t.Fatalf("durable result output = %q, want %q", durable.Output, tt.wantOutput)
			}
			if durable.Error == nil || durable.Error.Code != tt.wantCode {
				t.Fatalf("durable result error = %#v, want code %q", durable.Error, tt.wantCode)
			}
			if durable.ShutdownOrigin != tt.wantShutdownOrigin {
				t.Fatalf("durable shutdown origin = %q, want %q", durable.ShutdownOrigin, tt.wantShutdownOrigin)
			}
			if tt.wantShutdownOrigin != "" {
				wantErr := ShutdownResultError(tt.wantShutdownOrigin)
				if *durable.Error != *wantErr {
					t.Fatalf("durable result error = %#v, want canonical %#v", durable.Error, wantErr)
				}
			}
		})
	}
}

func TestEnsureResultDoesNotBackfillSuccessfulEmptyOutput(t *testing.T) {
	stateDir := t.TempDir()
	agent := &Agent{
		ID:            "agent-1",
		stateDir:      stateDir,
		lastAssistant: "streamed text",
		result: &TurnResult{
			AgentID: "agent-1", TurnID: "turn-1", Status: ResultSucceeded,
		},
	}

	(&Supervisor{}).ensureResult(agent, StatusDone, nil)

	if result := agent.Result(); result == nil || result.Output != "" {
		t.Fatalf("successful result = %#v, want intentionally empty output", result)
	}
}
