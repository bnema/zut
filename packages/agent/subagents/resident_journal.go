package subagents

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

const (
	residentMetadataName       = "metadata.json"
	residentTranscriptName     = "transcript.jsonl"
	residentResultName         = "result.json"
	residentPatchName          = "patch.diff"
	residentRecordAccepted     = "child.accepted"
	residentRecordTurnAccepted = "turn.accepted"
	residentRecordTurnStarted  = "turn.started"
	residentRecordTurnFinished = "turn.finished"
	residentRecordInterrupted  = "child.interrupted"
	residentRecordFailed       = "child.failed"
	residentRecordUser         = "message.user"
	residentRecordAssistant    = "message.assistant"
	residentRecordToolCall     = "tool.call"
	residentRecordToolResult   = "tool.result"
	residentJournalVersion     = 1
	residentMaxRecordBytes     = 2 << 20
)

// Artifact references are logical names; they do not disclose journal paths.
func AgentRef(id string) string   { return "subagent://" + id }
func HistoryRef(id string) string { return AgentRef(id) + "/history" }
func ResultRef(id string) string  { return AgentRef(id) + "/result" }
func PatchRef(id string) string   { return AgentRef(id) + "/patch" }

// ResidentState is the durable lifecycle used by the in-process child
// runtime. It deliberately has no process-liveness states.
type ResidentState string

const (
	ResidentQueued      ResidentState = "queued"
	ResidentRunning     ResidentState = "running"
	ResidentIdle        ResidentState = "idle"
	ResidentCompleted   ResidentState = "completed"
	ResidentFailed      ResidentState = "failed"
	ResidentStopped     ResidentState = "stopped"
	ResidentInterrupted ResidentState = "interrupted"
)

// ResidentChildSpec is the non-secret configuration required to reconstruct
// one resident child. Credentials are intentionally excluded; the selected
// provider endpoint and TLS mode are retained so explicit resume reconstructs
// the same transport contract.
type ResidentChildSpec struct {
	ID                    string        `json:"id"`
	SessionID             string        `json:"session_id"`
	InitialTurnID         string        `json:"initial_turn_id,omitempty"`
	ParentSessionID       string        `json:"parent_session_id,omitempty"`
	Profile               string        `json:"profile,omitempty"`
	Provider              string        `json:"provider"`
	BaseURL               string        `json:"base_url,omitempty"`
	InsecureTLS           bool          `json:"insecure_tls,omitempty"`
	Model                 string        `json:"model"`
	Reasoning             string        `json:"reasoning,omitempty"`
	FastMode              bool          `json:"fast_mode,omitempty"`
	Tools                 []string      `json:"tools,omitempty"`
	RepositoryRoot        string        `json:"repository_root,omitempty"`
	Workspace             string        `json:"workspace,omitempty"`
	WorkspaceMode         WorkspaceMode `json:"workspace_mode,omitempty"`
	WorkspaceBase         string        `json:"workspace_base,omitempty"`
	WorkspaceCapture      CaptureMode   `json:"workspace_capture,omitempty"`
	SystemPrompt          string        `json:"system_prompt,omitempty"`
	SystemPromptMode      string        `json:"system_prompt_mode,omitempty"`
	InheritProjectContext *bool         `json:"inherit_project_context,omitempty"`
	InheritSkills         *bool         `json:"inherit_skills,omitempty"`
	Permissions           []string      `json:"permissions,omitempty"`
	Required              bool          `json:"required,omitempty"`
}

type residentRecord struct {
	Version    int                `json:"version"`
	Type       string             `json:"type"`
	Time       time.Time          `json:"time"`
	Spec       *ResidentChildSpec `json:"spec,omitempty"`
	TurnID     string             `json:"turn_id,omitempty"`
	Prompt     string             `json:"prompt,omitempty"`
	Outcome    string             `json:"outcome,omitempty"`
	Message    json.RawMessage    `json:"message,omitempty"`
	ToolID     string             `json:"tool_id,omitempty"`
	ToolName   string             `json:"tool_name,omitempty"`
	ToolArgs   json.RawMessage    `json:"tool_args,omitempty"`
	ToolResult json.RawMessage    `json:"tool_result,omitempty"`
}

// RecordAgentEvent persists finalized provider-neutral transcript events. It
// deliberately excludes streamed deltas and hidden reasoning, keeping the
// journal authoritative for resumable visible history without an fsync per
// token.
func (j *ResidentJournal) RecordAgentEvent(event core.AgentEvent) error {
	if j == nil || event == nil {
		return nil
	}
	var record residentRecord
	switch value := event.(type) {
	case core.EvUserMessage:
		message, err := json.Marshal(value.Message)
		if err != nil {
			return fmt.Errorf("resident journal encode user message: %w", err)
		}
		record = residentRecord{Type: residentRecordUser, Message: message}
	case core.EvAssistantMessage:
		message, err := json.Marshal(value.Message)
		if err != nil {
			return fmt.Errorf("resident journal encode assistant message: %w", err)
		}
		record = residentRecord{Type: residentRecordAssistant, Message: message}
	case core.EvToolCall:
		record = residentRecord{Type: residentRecordToolCall, ToolID: value.ID, ToolName: value.Name, ToolArgs: append(json.RawMessage(nil), value.Args...)}
	case core.EvToolResult:
		result, err := json.Marshal(value.Result)
		if err != nil {
			return fmt.Errorf("resident journal encode tool result: %w", err)
		}
		record = residentRecord{Type: residentRecordToolResult, ToolID: value.ID, ToolResult: result}
	default:
		j.publishAgentEvent(event)
		return nil
	}
	record.Version = residentJournalVersion
	record.Time = time.Now().UTC()
	j.mu.Lock()
	if j.file == nil {
		j.mu.Unlock()
		return errors.New("resident journal: closed")
	}
	err := j.appendSync(record)
	j.mu.Unlock()
	if err == nil {
		j.publishAgentEvent(event)
	}
	return err
}

// ResidentMetadata is a bounded, rebuildable projection. The transcript is
// the authority for acceptance; callers must never infer acceptance solely
// from this file.
type ResidentMetadata struct {
	Version         int           `json:"version"`
	ID              string        `json:"id"`
	SessionID       string        `json:"session_id"`
	ParentSessionID string        `json:"parent_session_id,omitempty"`
	State           ResidentState `json:"state"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

// ResidentResult is the bounded latest-turn projection. The transcript
// remains authoritative for detailed content; this file allows status and
// resource readers to locate a terminal outcome without reading it all.
type ResidentResult struct {
	Version      int           `json:"version"`
	ID           string        `json:"id"`
	TurnID       string        `json:"turn_id"`
	State        ResidentState `json:"state"`
	ErrorCode    string        `json:"error_code,omitempty"`
	PatchRef     string        `json:"patch_ref,omitempty"`
	ChangedFiles []string      `json:"changed_files,omitempty"`
	CreatedAt    time.Time     `json:"created_at"`
}

// ResidentJournal serializes durable child records. It is intentionally a
// narrow storage primitive; the manager owns scheduling and provider work.
type ResidentJournal struct {
	mu            sync.Mutex
	eventMu       sync.RWMutex
	eventObserver func(core.AgentEvent)
	dir           string
	file          *os.File
}

// SetEventObserver publishes in-memory agent events without creating another
// durable transcript. Finalized events are observed only after journal append.
func (j *ResidentJournal) SetEventObserver(observer func(core.AgentEvent)) {
	if j == nil {
		return
	}
	j.eventMu.Lock()
	j.eventObserver = observer
	j.eventMu.Unlock()
}

func (j *ResidentJournal) publishAgentEvent(event core.AgentEvent) {
	if j == nil || event == nil {
		return
	}
	j.eventMu.RLock()
	observer := j.eventObserver
	j.eventMu.RUnlock()
	if observer != nil {
		observer(event)
	}
}

func OpenResidentJournal(root, childID string) (*ResidentJournal, error) {
	if strings.TrimSpace(childID) == "" || filepath.Base(childID) != childID {
		return nil, errors.New("resident journal: invalid child ID")
	}
	dir := filepath.Join(root, childID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("resident journal directory: %w", err)
	}
	_ = os.Chmod(dir, 0o700)
	f, err := os.OpenFile(filepath.Join(dir, residentTranscriptName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("resident journal transcript: %w", err)
	}
	_ = f.Chmod(0o600)
	return &ResidentJournal{dir: dir, file: f}, nil
}

// Dir is the child-owned state directory. It is exposed only to host factory
// code rebuilding a resident core.Agent from its authoritative transcript.
func (j *ResidentJournal) Dir() string {
	if j == nil {
		return ""
	}
	return j.dir
}

// Accept establishes the authoritative spawn commit point. The transcript is
// synced before metadata is projected so a crash cannot fabricate acceptance.
func (j *ResidentJournal) Accept(spec ResidentChildSpec, prompt string) error {
	if spec.ID == "" || spec.SessionID == "" || spec.Provider == "" || spec.Model == "" || strings.TrimSpace(prompt) == "" {
		return errors.New("resident journal: incomplete accepted child")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return errors.New("resident journal: closed")
	}
	record := residentRecord{Version: residentJournalVersion, Type: residentRecordAccepted, Time: time.Now().UTC(), Spec: &spec, Prompt: prompt}
	if err := j.appendSync(record); err != nil {
		return err
	}
	return writeResidentMetadata(j.dir, ResidentMetadata{Version: residentJournalVersion, ID: spec.ID, SessionID: spec.SessionID, ParentSessionID: spec.ParentSessionID, State: ResidentQueued, UpdatedAt: record.Time})
}

// RecordFailure makes a post-acceptance construction failure durable.
func (j *ResidentJournal) RecordFailure(spec ResidentChildSpec) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return errors.New("resident journal: closed")
	}
	now := time.Now().UTC()
	if err := j.appendSync(residentRecord{Version: residentJournalVersion, Type: residentRecordFailed, Time: now}); err != nil {
		return err
	}
	return writeResidentMetadata(j.dir, ResidentMetadata{Version: residentJournalVersion, ID: spec.ID, SessionID: spec.SessionID, ParentSessionID: spec.ParentSessionID, State: ResidentFailed, UpdatedAt: now})
}

// AcceptFollowUp establishes a durable follow-up before a live child is told
// about it. A restart therefore has enough information to classify the prompt
// as unstarted or interrupted, but never to replay it automatically.
func (j *ResidentJournal) AcceptFollowUp(spec ResidentChildSpec, turnID, prompt string) error {
	if strings.TrimSpace(turnID) == "" || strings.TrimSpace(prompt) == "" {
		return errors.New("resident journal: incomplete accepted turn")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return errors.New("resident journal: closed")
	}
	now := time.Now().UTC()
	if err := j.appendSync(residentRecord{Version: residentJournalVersion, Type: residentRecordTurnAccepted, Time: now, TurnID: turnID, Prompt: prompt}); err != nil {
		return err
	}
	return writeResidentMetadata(j.dir, ResidentMetadata{Version: residentJournalVersion, ID: spec.ID, SessionID: spec.SessionID, ParentSessionID: spec.ParentSessionID, State: ResidentQueued, UpdatedAt: now})
}

func (j *ResidentJournal) RecordTurnStarted(spec ResidentChildSpec, turnID string) error {
	return j.recordTurnBoundary(spec, residentRecordTurnStarted, turnID, ResidentRunning, "")
}

func (j *ResidentJournal) RecordTurnFinished(spec ResidentChildSpec, turnID string, err error) error {
	return j.RecordTurnFinishedWithCapture(spec, turnID, err, nil)
}

// RecordTurnFinishedWithCapture publishes bounded workspace artifacts before
// the terminal projection. The transcript remains the lifecycle authority.
func (j *ResidentJournal) RecordTurnFinishedWithCapture(spec ResidentChildSpec, turnID string, turnErr error, capture *WorkspaceCapture) error {
	state, outcome := ResidentIdle, "completed"
	if turnErr != nil {
		state, outcome = ResidentFailed, "failed"
	}
	result := ResidentResult{Version: residentJournalVersion, ID: spec.ID, TurnID: turnID, State: state, CreatedAt: time.Now().UTC()}
	if turnErr != nil {
		result.ErrorCode = "turn_failed"
	}
	if capture != nil {
		result.ChangedFiles = append([]string(nil), capture.ChangedFiles...)
		if len(capture.Patch) > 0 {
			if err := writeResidentPatch(j.dir, capture.Patch); err != nil {
				return fmt.Errorf("resident journal write workspace patch: %w", err)
			}
			result.PatchRef = PatchRef(spec.ID)
		}
	}
	if recordErr := j.recordTurnBoundary(spec, residentRecordTurnFinished, turnID, state, outcome); recordErr != nil {
		return recordErr
	}
	return writeResidentResult(j.dir, result)
}

func (j *ResidentJournal) RecordTurnInterrupted(spec ResidentChildSpec, turnID string) error {
	return j.recordTurnBoundary(spec, residentRecordInterrupted, turnID, ResidentInterrupted, "interrupted")
}

func (j *ResidentJournal) recordTurnBoundary(spec ResidentChildSpec, recordType, turnID string, state ResidentState, outcome string) error {
	if strings.TrimSpace(turnID) == "" {
		return errors.New("resident journal: missing turn ID")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return errors.New("resident journal: closed")
	}
	now := time.Now().UTC()
	if err := j.appendSync(residentRecord{Version: residentJournalVersion, Type: recordType, Time: now, TurnID: turnID, Outcome: outcome}); err != nil {
		return err
	}
	return writeResidentMetadata(j.dir, ResidentMetadata{Version: residentJournalVersion, ID: spec.ID, SessionID: spec.SessionID, ParentSessionID: spec.ParentSessionID, State: state, UpdatedAt: now})
}

func (j *ResidentJournal) appendSync(record residentRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("resident journal encode: %w", err)
	}
	if len(data) > residentMaxRecordBytes {
		return errors.New("resident journal: record too large")
	}
	data = append(data, '\n')
	if _, err := j.file.Write(data); err != nil {
		return fmt.Errorf("resident journal append: %w", err)
	}
	if err := j.file.Sync(); err != nil {
		return fmt.Errorf("resident journal sync: %w", err)
	}
	return nil
}

func (j *ResidentJournal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return nil
	}
	err := j.file.Close()
	j.file = nil
	return err
}

func ReadResidentJournal(path string) ([]residentRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	reader := bufio.NewReader(f)
	var records []residentRecord
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > residentMaxRecordBytes {
			return nil, errors.New("resident journal: record too large")
		}
		if len(line) > 0 && line[len(line)-1] == '\n' {
			var record residentRecord
			if decodeErr := json.Unmarshal(line, &record); decodeErr != nil {
				return nil, fmt.Errorf("resident journal decode: %w", decodeErr)
			}
			records = append(records, record)
		}
		if err == io.EOF {
			return records, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func ReadResidentMetadata(path string) (ResidentMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ResidentMetadata{}, err
	}
	var metadata ResidentMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return ResidentMetadata{}, err
	}
	return metadata, nil
}

func ReadResidentResult(path string) (ResidentResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ResidentResult{}, err
	}
	var result ResidentResult
	if err := json.Unmarshal(data, &result); err != nil {
		return ResidentResult{}, err
	}
	if result.Version != residentJournalVersion || result.ID == "" || result.TurnID == "" {
		return ResidentResult{}, errors.New("resident result: invalid projection")
	}
	return result, nil
}

// ReconcileResidentJournal rebuilds the durable projection from the
// authoritative acceptance record. Work that was queued or running when the
// host disappeared is recorded as interrupted; it is never made runnable by
// reconciliation and must receive an explicit new resume prompt later.
func ReconcileResidentJournal(dir string) (ResidentMetadata, error) {
	records, err := ReadResidentJournal(filepath.Join(dir, residentTranscriptName))
	if err != nil {
		return ResidentMetadata{}, err
	}
	if len(records) == 0 || records[0].Type != residentRecordAccepted || records[0].Spec == nil {
		return ResidentMetadata{}, errors.New("resident journal: missing accepted child record")
	}
	spec := *records[0].Spec
	if spec.ID == "" || spec.SessionID == "" {
		return ResidentMetadata{}, errors.New("resident journal: invalid accepted child record")
	}
	state := ResidentQueued
	lastFinishedTurn, lastFinishedOutcome := "", ""
	seenTurns := make(map[string]string)
	// The initial prompt is accepted atomically with child.accepted rather than
	// through a separate turn.accepted record. Seed it so interruption and
	// repeated reconciliation obey the same turn-state invariant as follow-ups.
	if spec.InitialTurnID != "" {
		seenTurns[spec.InitialTurnID] = residentRecordTurnAccepted
	}
	toolCalls := make(map[string]struct{})
	toolResults := make(map[string]struct{})
	for index, record := range records {
		if index > 0 && record.Type == residentRecordAccepted {
			return ResidentMetadata{}, errors.New("resident journal: duplicate accepted child record")
		}
		switch record.Type {
		case residentRecordTurnAccepted:
			if record.TurnID == "" || seenTurns[record.TurnID] != "" {
				return ResidentMetadata{}, errors.New("resident journal: invalid accepted turn record")
			}
			seenTurns[record.TurnID] = residentRecordTurnAccepted
		case residentRecordTurnStarted:
			if record.TurnID == "" || (seenTurns[record.TurnID] != residentRecordTurnAccepted && record.TurnID != spec.InitialTurnID) {
				return ResidentMetadata{}, errors.New("resident journal: turn started without acceptance")
			}
			seenTurns[record.TurnID] = residentRecordTurnStarted
			state = ResidentRunning
		case residentRecordTurnFinished:
			if record.TurnID == "" || seenTurns[record.TurnID] != residentRecordTurnStarted {
				return ResidentMetadata{}, errors.New("resident journal: turn finished without start")
			}
			seenTurns[record.TurnID] = residentRecordTurnFinished
			lastFinishedTurn, lastFinishedOutcome = record.TurnID, record.Outcome
			if record.Outcome == "failed" {
				state = ResidentFailed
			} else {
				state = ResidentIdle
			}
		case residentRecordInterrupted:
			if record.TurnID != "" && seenTurns[record.TurnID] != residentRecordTurnAccepted && seenTurns[record.TurnID] != residentRecordTurnStarted {
				return ResidentMetadata{}, errors.New("resident journal: turn interrupted without acceptance")
			}
			if record.TurnID != "" {
				seenTurns[record.TurnID] = residentRecordInterrupted
			}
			state = ResidentInterrupted
		case residentRecordFailed:
			state = ResidentFailed
		case residentRecordToolCall:
			if record.ToolID == "" || record.ToolName == "" || !json.Valid(record.ToolArgs) {
				return ResidentMetadata{}, errors.New("resident journal: invalid tool call record")
			}
			if _, duplicate := toolCalls[record.ToolID]; duplicate {
				return ResidentMetadata{}, errors.New("resident journal: duplicate tool call")
			}
			toolCalls[record.ToolID] = struct{}{}
		case residentRecordToolResult:
			if record.ToolID == "" || !json.Valid(record.ToolResult) {
				return ResidentMetadata{}, errors.New("resident journal: invalid tool result record")
			}
			if _, exists := toolCalls[record.ToolID]; !exists {
				return ResidentMetadata{}, errors.New("resident journal: orphan tool result")
			}
			if _, duplicate := toolResults[record.ToolID]; duplicate {
				return ResidentMetadata{}, errors.New("resident journal: duplicate tool result")
			}
			toolResults[record.ToolID] = struct{}{}
		}
	}
	metadata := ResidentMetadata{Version: residentJournalVersion, ID: spec.ID, SessionID: spec.SessionID, ParentSessionID: spec.ParentSessionID, State: state, UpdatedAt: time.Now().UTC()}
	needsInterruption := state == ResidentQueued || state == ResidentRunning
	needsToolRepair := len(toolCalls) != len(toolResults)
	if needsInterruption || needsToolRepair {
		journal, err := OpenResidentJournal(filepath.Dir(dir), filepath.Base(dir))
		if err != nil {
			return ResidentMetadata{}, err
		}
		defer journal.Close()
		dangling := make([]string, 0, len(toolCalls))
		for toolID := range toolCalls {
			if _, finished := toolResults[toolID]; !finished {
				dangling = append(dangling, toolID)
			}
		}
		sort.Strings(dangling)
		for _, toolID := range dangling {
			result, marshalErr := json.Marshal(core.ToolResult{IsError: true, Content: []provider.Content{provider.TextBlock{Text: "tool interrupted by resident host restart"}}})
			if marshalErr != nil {
				return ResidentMetadata{}, fmt.Errorf("resident journal encode interrupted tool result: %w", marshalErr)
			}
			if err := journal.appendSync(residentRecord{Version: residentJournalVersion, Type: residentRecordToolResult, Time: metadata.UpdatedAt, ToolID: toolID, ToolResult: result}); err != nil {
				return ResidentMetadata{}, err
			}
		}
		if !needsInterruption {
			if err := writeResidentMetadata(dir, metadata); err != nil {
				return ResidentMetadata{}, err
			}
			if err := rebuildResidentResult(dir, spec, metadata, lastFinishedTurn, lastFinishedOutcome); err != nil {
				return ResidentMetadata{}, err
			}
			return metadata, nil
		}
		interrupted := false
		turnIDs := make([]string, 0, len(seenTurns))
		for turnID, turnState := range seenTurns {
			if turnState != residentRecordTurnAccepted && turnState != residentRecordTurnStarted {
				continue
			}
			turnIDs = append(turnIDs, turnID)
		}
		sort.Strings(turnIDs)
		for _, turnID := range turnIDs {
			if err := journal.RecordTurnInterrupted(spec, turnID); err != nil {
				return ResidentMetadata{}, err
			}
			seenTurns[turnID] = residentRecordInterrupted
			interrupted = true
		}
		if !interrupted {
			if err := journal.appendSync(residentRecord{Version: residentJournalVersion, Type: residentRecordInterrupted, Time: metadata.UpdatedAt}); err != nil {
				return ResidentMetadata{}, err
			}
		}
		metadata.State = ResidentInterrupted
	}
	if err := writeResidentMetadata(dir, metadata); err != nil {
		return ResidentMetadata{}, err
	}
	if err := rebuildResidentResult(dir, spec, metadata, lastFinishedTurn, lastFinishedOutcome); err != nil {
		return ResidentMetadata{}, err
	}
	return metadata, nil
}

func rebuildResidentResult(dir string, spec ResidentChildSpec, metadata ResidentMetadata, turnID, outcome string) error {
	if turnID == "" {
		return nil
	}
	if _, err := ReadResidentResult(filepath.Join(dir, residentResultName)); err == nil || !errors.Is(err, os.ErrNotExist) {
		return err
	}
	result := ResidentResult{Version: residentJournalVersion, ID: spec.ID, TurnID: turnID, State: metadata.State, CreatedAt: metadata.UpdatedAt}
	if outcome == "failed" {
		result.ErrorCode = "turn_failed"
	}
	return writeResidentResult(dir, result)
}

func writeResidentMetadata(dir string, metadata ResidentMetadata) error {
	return writeResidentProjection(dir, residentMetadataName, metadata)
}

func writeResidentResult(dir string, result ResidentResult) error {
	return writeResidentProjection(dir, residentResultName, result)
}

func writeResidentPatch(dir string, patch []byte) error {
	tmp, err := os.CreateTemp(dir, ".patch-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(patch); err == nil {
		err = tmp.Sync()
		if closeErr := tmp.Close(); closeErr != nil {
			err = closeErr
		}
	} else {
		_ = tmp.Close()
	}
	if err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, residentPatchName))
}

func writeResidentProjection(dir, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".metadata-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("resident metadata permissions: %w", err)
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("resident metadata write: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("resident projection rename: %w", err)
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("resident projection directory sync: %w", err)
	}
	return nil
}
