package subagents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// TurnStatus is the durable outcome of one delegated turn.
type TurnStatus string

const (
	ResultSucceeded TurnStatus = "succeeded"
	ResultFailed    TurnStatus = "failed"
	ResultCanceled  TurnStatus = "canceled"
)

// ArtifactRef points to a durable file without exposing the supervisor's
// local state layout to callers.
type ArtifactRef struct {
	Name      string `json:"name,omitempty"`
	Ref       string `json:"ref"`
	MediaType string `json:"media_type,omitempty"`
	Size      int64  `json:"size,omitempty"`
}

// ResultError is intentionally free of credentials and argv details.
type ResultError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// ShutdownResultError returns the stable, user-facing error associated with
// an intentional worker shutdown. Unknown origins are not attributed.
func ShutdownResultError(origin ShutdownOrigin) *ResultError {
	switch origin.Sanitized() {
	case ShutdownOriginTargeted:
		return &ResultError{Code: "shutdown", Message: "subagent stopped by request"}
	case ShutdownOriginSession:
		return &ResultError{Code: "shutdown", Message: "subagent stopped during session shutdown"}
	case ShutdownOriginDeadline:
		return &ResultError{Code: "deadline_exceeded", Message: "subagent turn deadline exceeded; partial output is preserved in the result and history"}
	case ShutdownOriginProcess:
		return &ResultError{Code: "shutdown", Message: "subagent stopped during process shutdown"}
	default:
		return nil
	}
}

// TurnResult is the protocol-level result for a delegated turn. Output is a
// bounded inline preview; the full session/history remains available through
// HistoryRef and the complete JSON result through ResultRef.
type TurnResult struct {
	Version        int             `json:"version"`
	AgentID        string          `json:"agent_id,omitempty"`
	TurnID         string          `json:"turn_id,omitempty"`
	Status         TurnStatus      `json:"status"`
	Summary        string          `json:"summary,omitempty"`
	Output         string          `json:"output,omitempty"`
	Structured     json.RawMessage `json:"structured,omitempty"`
	Artifacts      []ArtifactRef   `json:"artifacts,omitempty"`
	ChangedFiles   []string        `json:"changed_files,omitempty"`
	Usage          map[string]any  `json:"usage,omitempty"`
	Error          *ResultError    `json:"error,omitempty"`
	ShutdownOrigin ShutdownOrigin  `json:"shutdown_origin,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

func (r *TurnResult) Validate(maxBytes, maxLines int) error {
	if r == nil {
		return errors.New("subagents: nil turn result")
	}
	if r.Version == 0 {
		r.Version = ProtocolVersion
	}
	if r.Version != ProtocolVersion {
		return fmt.Errorf("subagents: unsupported result version %d", r.Version)
	}
	if r.Status != ResultSucceeded && r.Status != ResultFailed && r.Status != ResultCanceled {
		return fmt.Errorf("subagents: invalid result status %q", r.Status)
	}
	if r.AgentID == "" {
		return errors.New("subagents: result missing agent id")
	}
	if r.ShutdownOrigin != "" && r.ShutdownOrigin.Sanitized() == "" {
		return errors.New("subagents: result has invalid shutdown origin")
	}
	if r.TurnID == "" {
		return errors.New("subagents: result missing turn id")
	}
	if len(r.Structured) > 0 && !json.Valid(r.Structured) {
		return errors.New("subagents: result structured payload is invalid JSON")
	}
	if maxBytes > 0 && len([]byte(r.Output)) > maxBytes {
		return fmt.Errorf("subagents: result output exceeds %d bytes", maxBytes)
	}
	if maxLines > 0 && countLines(r.Output) > maxLines {
		return fmt.Errorf("subagents: result output exceeds %d lines", maxLines)
	}
	return nil
}

// Bounded returns a copy with inline output capped by bytes and lines. It
// never mutates the durable result's caller-owned value.
func (r *TurnResult) Bounded(maxBytes, maxLines int) *TurnResult {
	out := cloneTurnResult(r)
	if out == nil {
		return nil
	}
	if len([]byte(out.Summary)) > 4*1024 {
		out.Summary = truncateUTF8(out.Summary, 4*1024)
	}
	out.Output = boundInlineText(out.Output, maxBytes, maxLines)
	return out
}

// boundInlineText applies the shared line, byte, and UTF-8-safe bounds used
// for inline assistant output and durable turn-result previews.
func boundInlineText(value string, maxBytes, maxLines int) string {
	const marker = "...[output truncated]"
	if maxLines > 0 {
		lines := strings.Split(value, "\n")
		if len(lines) > maxLines {
			if maxLines == 1 {
				value = marker
			} else {
				value = strings.Join(lines[:maxLines-1], "\n") + "\n" + marker
			}
		}
	}
	if maxBytes > 0 && len([]byte(value)) > maxBytes {
		if maxBytes <= len(marker) {
			return truncateUTF8(value, maxBytes)
		}
		return truncateUTF8(value, maxBytes-len(marker)) + marker
	}
	return value
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	data := []byte(value)
	if len(data) <= maxBytes {
		return value
	}

	cut := maxBytes
	// Only inspect the short suffix that can belong to a UTF-8 rune ending
	// at the cut. Validating the entire prefix would make this quadratic for
	// invalid input and would incorrectly discard valid bytes before an
	// earlier invalid byte.
	start := cut - 1
	minimum := cut - utf8.UTFMax
	if minimum < 0 {
		minimum = 0
	}
	for start >= minimum && isUTF8Continuation(data[start]) {
		start--
	}
	if start >= minimum {
		if size := utf8LeadSize(data[start]); size > cut-start && isUTF8Prefix(data[start:cut], size) {
			cut = start
		}
	}
	return string(data[:cut])
}

func isUTF8Continuation(b byte) bool {
	return b&0xc0 == 0x80
}

func utf8LeadSize(b byte) int {
	switch {
	case b < utf8.RuneSelf:
		return 1
	case b >= 0xc2 && b <= 0xdf:
		return 2
	case b >= 0xe0 && b <= 0xef:
		return 3
	case b >= 0xf0 && b <= 0xf4:
		return 4
	default:
		return 0
	}
}

func isUTF8Prefix(data []byte, size int) bool {
	if len(data) <= 1 {
		return true
	}
	if !isUTF8Continuation(data[1]) {
		return false
	}
	switch data[0] {
	case 0xe0:
		if data[1] < 0xa0 {
			return false
		}
	case 0xed:
		if data[1] > 0x9f {
			return false
		}
	case 0xf0:
		if data[1] < 0x90 {
			return false
		}
	case 0xf4:
		if data[1] > 0x8f {
			return false
		}
	}
	for _, b := range data[2:] {
		if !isUTF8Continuation(b) {
			return false
		}
	}
	return len(data) < size
}

func validateTurnResultAgent(result *TurnResult, agentID string) error {
	if result == nil {
		return errors.New("subagents: nil turn result")
	}
	if agentID != "" && result.AgentID != agentID {
		return fmt.Errorf("subagents: turn result belongs to agent %q, want %q", result.AgentID, agentID)
	}
	return nil
}

func decodeTurnResultEvent(ev Event, agentID string, maxBytes, maxLines int) (*TurnResult, error) {
	data, err := json.Marshal(ev.Data)
	if err != nil {
		return nil, fmt.Errorf("subagents: encode turn result event: %w", err)
	}
	var result TurnResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("subagents: decode turn result event: %w", err)
	}
	if result.AgentID == "" {
		result.AgentID = firstNonEmpty(ev.AgentID, agentID)
	}
	if err := validateTurnResultAgent(&result, agentID); err != nil {
		return nil, err
	}
	if result.TurnID == "" {
		result.TurnID = ev.TurnID
	}
	if result.Version == 0 {
		result.Version = ProtocolVersion
	}
	if result.CreatedAt.IsZero() {
		result.CreatedAt = ev.Time
		if result.CreatedAt.IsZero() {
			result.CreatedAt = time.Now().UTC()
		}
	}
	resultPtr := result.Bounded(maxBytes, maxLines)
	if err := resultPtr.Validate(0, 0); err != nil {
		return nil, err
	}
	return resultPtr, nil
}

func cloneTurnResult(r *TurnResult) *TurnResult {
	if r == nil {
		return nil
	}
	copy := *r
	copy.Structured = append(json.RawMessage(nil), r.Structured...)
	copy.Artifacts = append([]ArtifactRef(nil), r.Artifacts...)
	copy.ChangedFiles = append([]string(nil), r.ChangedFiles...)
	if r.Usage != nil {
		copy.Usage = make(map[string]any, len(r.Usage))
		for key, value := range r.Usage {
			copy.Usage[key] = value
		}
	}
	if r.Error != nil {
		errCopy := *r.Error
		copy.Error = &errCopy
	}
	return &copy
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return 1 + strings.Count(s, "\n")
}

func AgentRef(id string) string   { return "subagent://" + id }
func HistoryRef(id string) string { return AgentRef(id) + "/history" }
func ResultRef(id string) string  { return AgentRef(id) + "/result" }
func PatchRef(id string) string   { return AgentRef(id) + "/patch" }

const maxResultFileBytes = 8 * 1024 * 1024

const (
	workspaceCaptureErrorCode    = "workspace_capture_failed"
	workspaceCaptureErrorMessage = "workspace capture failed"
)

func resultPath(stateDir string) string { return filepath.Join(stateDir, "result.json") }
func patchPath(stateDir string) string  { return filepath.Join(stateDir, "patch.diff") }

// writeSyncedFile writes a complete temporary file and closes it after the
// contents have been synced. Keeping this separate makes it harder for an
// atomic rename caller to accidentally publish an unflushed or open file.
func writeSyncedFile(path string, data []byte, perm os.FileMode) (err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer func() {
		closeErr := file.Close()
		if closeErr == nil {
			return
		}
		closeErr = fmt.Errorf("close: %w", closeErr)
		if err == nil {
			err = closeErr
		} else {
			err = errors.Join(err, closeErr)
		}
	}()

	for written := 0; written < len(data); {
		n, writeErr := file.Write(data[written:])
		written += n
		if writeErr != nil {
			return fmt.Errorf("write: %w", writeErr)
		}
		if n == 0 {
			return fmt.Errorf("write: %w", io.ErrShortWrite)
		}
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	return nil
}

func writeTurnResult(stateDir string, result *TurnResult) (err error) {
	if result == nil {
		return errors.New("subagents: nil result")
	}
	if result.CreatedAt.IsZero() {
		result.CreatedAt = time.Now().UTC()
	}
	if result.Version == 0 {
		result.Version = ProtocolVersion
	}
	if err := result.Validate(0, 0); err != nil {
		return err
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("subagents result marshal: %w", err)
	}
	if len(data) > maxResultFileBytes {
		return fmt.Errorf("subagents result exceeds %d bytes", maxResultFileBytes)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("subagents result dir: %w", err)
	}
	tmp := resultPath(stateDir) + ".tmp"
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err := writeSyncedFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("subagents result write: %w", err)
	}
	if err := os.Rename(tmp, resultPath(stateDir)); err != nil {
		return fmt.Errorf("subagents result rename: %w", err)
	}
	return nil
}

func (f *Supervisor) captureWorkspace(a *Agent) {
	if a == nil || a.workspaceCapture == nil {
		return
	}
	stateDir := a.stateDirectory(f.cfg.Root)
	if _, err := os.Stat(stateDir); err != nil {
		recordWorkspaceCaptureFailure(a)
		return
	}
	capture, err := a.workspaceCapture()
	if err != nil {
		recordWorkspaceCaptureFailure(a)
		return
	}
	a.lifecycleMu.Lock()
	a.changedFiles = append([]string(nil), capture.ChangedFiles...)
	a.lifecycleMu.Unlock()
	if len(capture.Patch) == 0 {
		return
	}
	path := patchPath(stateDir)
	tmp := path + ".tmp"
	if err := writeSyncedFile(tmp, capture.Patch, 0o600); err != nil {
		_ = os.Remove(tmp)
		recordWorkspaceCaptureFailure(a)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		recordWorkspaceCaptureFailure(a)
		return
	}
	a.lifecycleMu.Lock()
	a.patchRef = PatchRef(a.ID)
	a.lifecycleMu.Unlock()
}

// recordWorkspaceCaptureFailure records a stable, non-sensitive error in the
// result already received from the child (or creates the durable fallback).
// The underlying filesystem/capture error can contain private paths or data,
// so it is deliberately not copied into ResultError.
func recordWorkspaceCaptureFailure(a *Agent) {
	if a == nil {
		return
	}
	result := a.Result()
	if result == nil || validateTurnResultAgent(result, a.ID) != nil {
		result = &TurnResult{AgentID: a.ID}
	}
	result.Version = ProtocolVersion
	if result.AgentID == "" {
		result.AgentID = a.ID
	}
	if result.TurnID == "" {
		turnID := a.CurrentTurnID()
		if turnID == "" {
			turnID = fmt.Sprintf("turn-%d", a.AttemptValue())
		}
		result.TurnID = turnID
	}
	if result.CreatedAt.IsZero() {
		result.CreatedAt = time.Now().UTC()
	}
	result.Status = ResultFailed
	result.Error = &ResultError{
		Code:    workspaceCaptureErrorCode,
		Message: workspaceCaptureErrorMessage,
	}
	a.setResult(result)
}

func (f *Supervisor) ensureResult(a *Agent, status Status, runErr error) {
	if a == nil {
		return
	}
	attempt := a.AttemptValue()
	shutdownOrigin := a.shutdownOriginValue()
	if errors.Is(runErr, context.DeadlineExceeded) {
		shutdownOrigin = ShutdownOriginDeadline
	}
	a.mu.Lock()
	partialOutput := a.lastAssistant
	a.mu.Unlock()
	result := a.Result()
	if result != nil {
		if err := validateTurnResultAgent(result, a.ID); err != nil {
			result = nil
		}
	}
	if result != nil {
		// A child may omit agent/turn metadata in a result event; the
		// supervisor supplies it before validating the durable fallback.
		if result.AgentID == "" {
			result.AgentID = a.ID
		}
		if result.TurnID == "" {
			result.TurnID = firstNonEmpty(a.CurrentTurnID(), fmt.Sprintf("turn-%d", attempt))
		}
		if result.Version == 0 {
			result.Version = ProtocolVersion
		}
		if result.CreatedAt.IsZero() {
			result.CreatedAt = time.Now().UTC()
		}
		if err := result.Validate(0, 0); err != nil {
			result = nil
		}
	}
	if result != nil && result.Output == "" && partialOutput != "" &&
		(result.Status == ResultFailed || result.Status == ResultCanceled || status == StatusFailed || status == StatusKilled) {
		// A canceled or failed worker turn may emit its terminal result before
		// its provider turns accumulated text into a final assistant message.
		// The event stream has already projected that text onto the Agent, so
		// retain it in the durable result rather than returning an empty failure.
		result.Output = partialOutput
	}
	if result == nil {
		turnID := a.CurrentTurnID()
		if turnID == "" {
			turnID = fmt.Sprintf("turn-%d", attempt)
		}
		output := partialOutput
		resultStatus := ResultSucceeded
		switch {
		case status == StatusFailed && errors.Is(runErr, context.Canceled):
			resultStatus = ResultCanceled
		case status == StatusFailed:
			resultStatus = ResultFailed
		case status == StatusKilled:
			resultStatus = ResultCanceled
		}
		result = &TurnResult{
			Version:   ProtocolVersion,
			AgentID:   a.ID,
			TurnID:    turnID,
			Status:    resultStatus,
			Output:    output,
			CreatedAt: time.Now().UTC(),
		}
	}
	if result.AgentID == "" {
		result.AgentID = a.ID
	}
	if result.TurnID == "" {
		result.TurnID = firstNonEmpty(a.CurrentTurnID(), fmt.Sprintf("turn-%d", attempt))
	}
	if result.Version == 0 {
		result.Version = ProtocolVersion
	}
	if result.CreatedAt.IsZero() {
		result.CreatedAt = time.Now().UTC()
	}
	if shutdownOrigin != "" {
		result.ShutdownOrigin = shutdownOrigin
		result.Error = ShutdownResultError(shutdownOrigin)
		if shutdownOrigin == ShutdownOriginDeadline {
			result.Status = ResultFailed
		} else {
			result.Status = ResultCanceled
		}
	}
	if runErr != nil {
		if status == StatusKilled || errors.Is(runErr, context.Canceled) {
			result.Status = ResultCanceled
		} else {
			result.Status = ResultFailed
		}
		if errors.Is(runErr, context.DeadlineExceeded) {
			result.Status = ResultFailed
			result.ShutdownOrigin = ShutdownOriginDeadline
		} else if result.Error == nil {
			result.Error = &ResultError{Code: "runner_failed", Message: truncate(runErr.Error(), 500)}
		}
	}
	if len(result.ChangedFiles) == 0 {
		a.lifecycleMu.Lock()
		result.ChangedFiles = append([]string(nil), a.changedFiles...)
		a.lifecycleMu.Unlock()
	}
	if result.Summary == "" && result.Output != "" {
		result.Summary = strings.Split(strings.TrimSpace(result.Output), "\n")[0]
	}
	result = result.Bounded(a.maxOutputBytes, a.maxOutputLines)
	stateDir := a.stateDirectory(f.cfg.Root)
	if err := writeTurnResult(stateDir, result); err != nil {
		a.recordPersistenceError(fmt.Errorf("write turn result: %w", err))
		failed := cloneTurnResult(result)
		failed.Status = ResultFailed
		failed.Error = &ResultError{Code: "result_persistence_failed", Message: "failed to persist delegated turn result"}
		a.setResult(failed)
		a.resolveRequirement(0, nil, fmt.Sprintf("write turn result: %v", err), true)
		return
	}
	a.setResult(result)
	a.lifecycleMu.Lock()
	a.resultRef = ResultRef(a.ID)
	a.lifecycleMu.Unlock()
	a.recordTrace(TraceEvent{Type: "result.available", TurnID: result.TurnID, Data: map[string]any{"ref": ResultRef(a.ID)}})
	a.resolveRequirement(0, result, "", true)
}

// ReadResult returns the durable structured result for an agent.
func (f *Supervisor) ReadResult(id string) (*TurnResult, error) {
	a := f.Get(id)
	if a == nil {
		return nil, fmt.Errorf("subagents: no such agent %q", id)
	}
	if result := a.Result(); result != nil {
		if err := validateTurnResultAgent(result, a.ID); err != nil {
			return nil, err
		}
		return result, nil
	}
	result, err := readTurnResult(a.stateDirectory(f.cfg.Root))
	if err != nil {
		return nil, err
	}
	if err := validateTurnResultAgent(result, a.ID); err != nil {
		return nil, err
	}
	return result.Bounded(f.cfg.Policy.MaxOutputBytes, f.cfg.Policy.MaxOutputLines), nil
}

func (f *Supervisor) ResultReference(id string) string  { return ResultRef(id) }
func (f *Supervisor) HistoryReference(id string) string { return HistoryRef(id) }
func (f *Supervisor) PatchReference(id string) string   { return PatchRef(id) }

func readTurnResult(stateDir string) (*TurnResult, error) {
	file, err := os.Open(resultPath(stateDir))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxResultFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxResultFileBytes {
		return nil, fmt.Errorf("subagents result exceeds %d bytes", maxResultFileBytes)
	}
	var result TurnResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("subagents result parse: %w", err)
	}
	if err := result.Validate(0, 0); err != nil {
		return nil, err
	}
	return &result, nil
}
