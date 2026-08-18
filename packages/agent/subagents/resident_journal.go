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
	"unicode/utf8"

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
	residentRecordUsage        = "usage"
	residentJournalVersion     = 1
	residentInterruptedText    = "tool interrupted by resident host restart"
	residentMaxRecordBytes     = 2 << 20
	residentResultSummaryBytes = 256 << 10
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
	ID string `json:"id"`
	// SessionID identifies the child's conversation thread. RootCacheID is the
	// root cache affinity shared by every child of the same parent session.
	SessionID             string        `json:"session_id"`
	RootCacheID           string        `json:"root_cache_id,omitempty"`
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
	Version      int                `json:"version"`
	Type         string             `json:"type"`
	Time         time.Time          `json:"time"`
	Spec         *ResidentChildSpec `json:"spec,omitempty"`
	TurnID       string             `json:"turn_id,omitempty"`
	Prompt       string             `json:"prompt,omitempty"`
	Outcome      string             `json:"outcome,omitempty"`
	Message      json.RawMessage    `json:"message,omitempty"`
	ToolID       string             `json:"tool_id,omitempty"`
	ToolName     string             `json:"tool_name,omitempty"`
	ToolArgs     json.RawMessage    `json:"tool_args,omitempty"`
	ToolResult   json.RawMessage    `json:"tool_result,omitempty"`
	Usage        *provider.Usage    `json:"usage,omitempty"`
	ContextUsed  int                `json:"context_used,omitempty"`
	ContextMax   int                `json:"context_max,omitempty"`
	Subscription bool               `json:"subscription,omitempty"`
	raw          json.RawMessage    `json:"-"`
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
	case core.EvUsage:
		j.usageMu.RLock()
		contextUsed, contextMax, subscription := j.contextUsed, j.contextMax, j.subscription
		j.usageMu.RUnlock()
		if current := value.Usage.PromptTokens(); current > 0 {
			contextUsed = current
		}
		usage := value.Cumulative
		record = residentRecord{Type: residentRecordUsage, Usage: &usage, ContextUsed: contextUsed, ContextMax: contextMax, Subscription: subscription}
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
	if record.Type == residentRecordUsage {
		durable, metadataPersisted := j.usageRecordPersistence(record)
		if durable {
			var err error
			if !metadataPersisted {
				err = j.projectUsageMetadata()
				if err == nil {
					j.markUsageMetadataPersisted(record)
				}
			}
			j.mu.Unlock()
			if err != nil {
				return err
			}
			j.publishAgentEvent(event)
			return nil
		}
	}
	err := j.appendSync(record)
	if usageEvent, ok := event.(core.EvUsage); err == nil && ok {
		j.usageMu.Lock()
		j.usage = usageEvent.Cumulative
		j.usageRecordPersisted = true
		j.usageMetadataPersisted = false
		if current := usageEvent.Usage.PromptTokens(); current > 0 {
			j.contextUsed = current
		}
		j.usageMu.Unlock()
		err = j.projectUsageMetadata()
		if err == nil {
			j.markUsageMetadataPersisted(record)
		}
	}
	j.mu.Unlock()
	if err == nil {
		switch message := event.(type) {
		case core.EvAssistantMessage:
			j.recordAssistantSummary(message.Message)
		case core.EvToolCall:
			// Core emits tool calls immediately after the assistant message that
			// requested them. That text is an intermediate thought, not this
			// turn's final response.
			j.clearLatestSummary()
		}
		j.publishAgentEvent(event)
	}
	return err
}

// ResidentMetadata is a bounded, rebuildable projection. The transcript is
// the authority for acceptance; callers must never infer acceptance solely
// from this file.
type ResidentMetadata struct {
	Version         int            `json:"version"`
	ID              string         `json:"id"`
	SessionID       string         `json:"session_id"`
	RootCacheID     string         `json:"root_cache_id,omitempty"`
	ParentSessionID string         `json:"parent_session_id,omitempty"`
	State           ResidentState  `json:"state"`
	UpdatedAt       time.Time      `json:"updated_at"`
	Usage           provider.Usage `json:"usage,omitempty"`
	ContextUsed     int            `json:"context_used,omitempty"`
	ContextMax      int            `json:"context_max,omitempty"`
	Subscription    bool           `json:"subscription,omitempty"`
}

// ResidentResult is the bounded latest-turn projection. The transcript
// remains authoritative for detailed content; this file allows status and
// resource readers to locate a terminal outcome without reading it all.
type ResidentResult struct {
	Version      int           `json:"version"`
	ID           string        `json:"id"`
	TurnID       string        `json:"turn_id"`
	State        ResidentState `json:"state"`
	Summary      string        `json:"summary,omitempty"`
	ErrorCode    string        `json:"error_code,omitempty"`
	PatchRef     string        `json:"patch_ref,omitempty"`
	ChangedFiles []string      `json:"changed_files,omitempty"`
	CreatedAt    time.Time     `json:"created_at"`
}

// ResidentJournal serializes durable child records. It is intentionally a
// narrow storage primitive; the manager owns scheduling and provider work.
type ResidentJournal struct {
	mu                     sync.Mutex
	eventMu                sync.RWMutex
	eventObserver          func(core.AgentEvent)
	summaryMu              sync.RWMutex
	latestSummary          string
	usageMu                sync.RWMutex
	usage                  provider.Usage
	usageRecordPersisted   bool
	usageMetadataPersisted bool
	contextUsed            int
	contextMax             int
	subscription           bool
	dir                    string
	file                   *os.File
	lease                  *residentLease
}

// ResidentUsageSnapshot is the bounded usage projection shared by durable and
// live resident state.
type ResidentUsageSnapshot struct {
	Usage        provider.Usage
	ContextUsed  int
	ContextMax   int
	Subscription bool
}

// ConfigureUsage records resolved model metadata and restores the latest
// durable usage baseline when a resident child is reconstructed.
func (j *ResidentJournal) ConfigureUsage(contextMax int, subscription bool) ResidentUsageSnapshot {
	if j == nil {
		return ResidentUsageSnapshot{}
	}
	j.usageMu.Lock()
	if metadata, err := ReadResidentMetadata(filepath.Join(j.dir, residentMetadataName)); err == nil {
		j.usage = metadata.Usage
		j.contextUsed = metadata.ContextUsed
	}
	j.contextMax = contextMax
	j.subscription = subscription
	snapshot := ResidentUsageSnapshot{Usage: j.usage, ContextUsed: j.contextUsed, ContextMax: j.contextMax, Subscription: j.subscription}
	j.usageMu.Unlock()
	return snapshot
}

func (j *ResidentJournal) usageSnapshot() ResidentUsageSnapshot {
	if j == nil {
		return ResidentUsageSnapshot{}
	}
	j.usageMu.RLock()
	defer j.usageMu.RUnlock()
	return ResidentUsageSnapshot{Usage: j.usage, ContextUsed: j.contextUsed, ContextMax: j.contextMax, Subscription: j.subscription}
}

func (j *ResidentJournal) usageRecordPersistence(record residentRecord) (durable, metadataPersisted bool) {
	if record.Usage == nil {
		return false, false
	}
	j.usageMu.RLock()
	defer j.usageMu.RUnlock()
	matches := j.usage == *record.Usage &&
		j.contextUsed == record.ContextUsed &&
		j.contextMax == record.ContextMax &&
		j.subscription == record.Subscription
	return j.usageRecordPersisted && matches, j.usageMetadataPersisted && matches
}

func (j *ResidentJournal) markUsageMetadataPersisted(record residentRecord) {
	if record.Usage == nil {
		return
	}
	j.usageMu.Lock()
	if j.usage == *record.Usage &&
		j.contextUsed == record.ContextUsed &&
		j.contextMax == record.ContextMax &&
		j.subscription == record.Subscription {
		j.usageMetadataPersisted = true
	}
	j.usageMu.Unlock()
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
	if !residentChildID(childID) {
		return nil, errors.New("resident journal: invalid child ID")
	}
	dir := filepath.Join(root, childID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("resident journal directory: %w", err)
	}
	_ = os.Chmod(dir, 0o700)
	lease, err := acquireResidentLease(dir)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, residentTranscriptName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		_ = lease.Close()
		return nil, fmt.Errorf("resident journal transcript: %w", err)
	}
	_ = f.Chmod(0o600)
	return &ResidentJournal{dir: dir, file: f, lease: lease}, nil
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
	return writeResidentMetadata(j.dir, j.metadata(spec, ResidentQueued, record.Time))
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
	return writeResidentMetadata(j.dir, j.metadata(spec, ResidentFailed, now))
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
	return writeResidentMetadata(j.dir, j.metadata(spec, ResidentQueued, now))
}

func (j *ResidentJournal) RecordTurnStarted(spec ResidentChildSpec, turnID string) error {
	if err := j.recordTurnBoundary(spec, residentRecordTurnStarted, turnID, ResidentRunning, ""); err != nil {
		return err
	}
	j.summaryMu.Lock()
	j.latestSummary = ""
	j.summaryMu.Unlock()
	return nil
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
	result := ResidentResult{Version: residentJournalVersion, ID: spec.ID, TurnID: turnID, State: state, Summary: j.latestAssistantSummary(), CreatedAt: time.Now().UTC()}
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

func (j *ResidentJournal) recordAssistantSummary(message provider.Message) {
	summary := residentAssistantSummary(message)
	if summary == "" {
		return
	}
	j.summaryMu.Lock()
	j.latestSummary = summary
	j.summaryMu.Unlock()
}

func residentAssistantSummary(message provider.Message) string {
	var text strings.Builder
	for _, content := range message.Content {
		if block, ok := content.(provider.TextBlock); ok {
			text.WriteString(block.Text)
		}
	}
	return truncateResidentResultSummary(text.String())
}

func (j *ResidentJournal) latestAssistantSummary() string {
	if j == nil {
		return ""
	}
	j.summaryMu.RLock()
	summary := j.latestSummary
	j.summaryMu.RUnlock()
	return summary
}

func (j *ResidentJournal) clearLatestSummary() {
	if j == nil {
		return
	}
	j.summaryMu.Lock()
	j.latestSummary = ""
	j.summaryMu.Unlock()
}

func truncateResidentResultSummary(text string) string {
	text = strings.TrimSpace(text)
	if len(text) <= residentResultSummaryBytes {
		return text
	}
	limit := residentResultSummaryBytes - len("…")
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return strings.TrimSpace(text[:limit]) + "…"
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
	return writeResidentMetadata(j.dir, j.metadata(spec, state, now))
}

func (j *ResidentJournal) metadata(spec ResidentChildSpec, state ResidentState, updatedAt time.Time) ResidentMetadata {
	usage := j.usageSnapshot()
	return ResidentMetadata{
		Version: residentJournalVersion, ID: spec.ID, SessionID: spec.SessionID,
		RootCacheID: spec.RootCacheID, ParentSessionID: spec.ParentSessionID, State: state, UpdatedAt: updatedAt,
		Usage: usage.Usage, ContextUsed: usage.ContextUsed, ContextMax: usage.ContextMax, Subscription: usage.Subscription,
	}
}

// projectUsageMetadata updates the rebuildable projection while j.mu holds the
// lifecycle ordering lock. The transcript remains authoritative if this write
// fails and reconciliation can reconstruct the same values.
func (j *ResidentJournal) projectUsageMetadata() error {
	metadata, err := ReadResidentMetadata(filepath.Join(j.dir, residentMetadataName))
	if err != nil {
		return err
	}
	usage := j.usageSnapshot()
	metadata.Usage = usage.Usage
	metadata.ContextUsed = usage.ContextUsed
	metadata.ContextMax = usage.ContextMax
	metadata.Subscription = usage.Subscription
	return writeResidentMetadata(j.dir, metadata)
}

func (j *ResidentJournal) appendSync(record residentRecord) error {
	if j == nil || j.file == nil {
		return errors.New("resident journal: closed")
	}
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
	if j == nil {
		return nil
	}
	j.mu.Lock()
	file := j.file
	lease := j.lease
	j.file = nil
	j.lease = nil
	j.mu.Unlock()
	var errs []error
	if file != nil {
		errs = append(errs, file.Close())
	}
	if lease != nil {
		errs = append(errs, lease.Close())
	}
	return errors.Join(errs...)
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
			record.raw = append(record.raw[:0], line...)
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

// Result reads this child's bounded latest-turn projection.
func (j *ResidentJournal) Result() (ResidentResult, error) {
	if j == nil {
		return ResidentResult{}, errors.New("resident journal: unavailable")
	}
	return ReadResidentResult(filepath.Join(j.dir, residentResultName))
}

// ReconcileResidentJournal rebuilds the durable projection from the
// authoritative acceptance record. Work that was queued or running when the
// host disappeared is recorded as interrupted; it is never made runnable by
// reconciliation and must receive an explicit new resume prompt later.
//
// It first takes exclusive resident ownership. A busy lease means another
// host may still append records, so this function intentionally performs no
// transcript inspection or recovery.
func ReconcileResidentJournal(dir string) (ResidentMetadata, error) {
	metadata, _, err := reconcileResidentJournalWithSpec(dir)
	return metadata, err
}

// reconcileResidentJournalWithSpec returns the accepted specification from the
// same ownership interval as reconciliation. Callers must not use a prior
// snapshot to resume a child because another host could have changed the
// journal after that snapshot was built.
func reconcileResidentJournalWithSpec(dir string) (ResidentMetadata, ResidentChildSpec, error) {
	journal, err := OpenResidentJournal(filepath.Dir(dir), filepath.Base(dir))
	if err != nil {
		return ResidentMetadata{}, ResidentChildSpec{}, err
	}
	metadata, reconcileErr := reconcileOwnedResidentJournal(journal)
	var spec ResidentChildSpec
	if reconcileErr == nil {
		records, err := ReadResidentJournal(filepath.Join(journal.Dir(), residentTranscriptName))
		if err != nil || len(records) == 0 || records[0].Spec == nil {
			reconcileErr = errors.New("resident journal: invalid accepted child record")
		} else {
			spec = *records[0].Spec
		}
	}
	if closeErr := journal.Close(); closeErr != nil {
		reconcileErr = errors.Join(reconcileErr, closeErr)
	}
	if reconcileErr != nil {
		return ResidentMetadata{}, ResidentChildSpec{}, reconcileErr
	}
	return metadata, spec, nil
}

// reconcileOwnedResidentJournal requires the resident journal lease held by
// journal. It is the sole reconciliation implementation so reads and writes
// share one ownership interval.
func reconcileOwnedResidentJournal(journal *ResidentJournal) (ResidentMetadata, error) {
	if journal == nil {
		return ResidentMetadata{}, errors.New("resident journal: unavailable")
	}
	dir := journal.Dir()
	records, err := ReadResidentJournal(filepath.Join(dir, residentTranscriptName))
	if err != nil {
		return ResidentMetadata{}, err
	}
	if repaired, ok := repairLegacyFalseRecovery(records); ok {
		if err := journal.rewriteTranscript(repaired); err != nil {
			return ResidentMetadata{}, err
		}
		records = repaired
	}
	if len(records) == 0 || records[0].Type != residentRecordAccepted || records[0].Spec == nil {
		return ResidentMetadata{}, errors.New("resident journal: missing accepted child record")
	}
	spec := *records[0].Spec
	if spec.ID == "" || spec.SessionID == "" {
		return ResidentMetadata{}, errors.New("resident journal: invalid accepted child record")
	}
	state := ResidentQueued
	lastStateAt := records[0].Time
	lastFinishedTurn, lastFinishedOutcome, lastAssistantSummary := "", "", ""
	usage := ResidentUsageSnapshot{}
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
			lastStateAt = record.Time
		case residentRecordTurnStarted:
			if record.TurnID == "" || (seenTurns[record.TurnID] != residentRecordTurnAccepted && record.TurnID != spec.InitialTurnID) {
				return ResidentMetadata{}, errors.New("resident journal: turn started without acceptance")
			}
			seenTurns[record.TurnID] = residentRecordTurnStarted
			state = ResidentRunning
			lastStateAt = record.Time
			lastAssistantSummary = ""
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
			lastStateAt = record.Time
		case residentRecordAssistant:
			message, err := core.DecodeMessageJSON(record.Message)
			if err != nil {
				return ResidentMetadata{}, fmt.Errorf("resident journal: invalid assistant message: %w", err)
			}
			if summary := residentAssistantSummary(message); summary != "" {
				lastAssistantSummary = summary
			}
		case residentRecordInterrupted:
			if record.TurnID != "" && seenTurns[record.TurnID] != residentRecordTurnAccepted && seenTurns[record.TurnID] != residentRecordTurnStarted {
				return ResidentMetadata{}, errors.New("resident journal: turn interrupted without acceptance")
			}
			if record.TurnID != "" {
				seenTurns[record.TurnID] = residentRecordInterrupted
			}
			state = ResidentInterrupted
			lastStateAt = record.Time
		case residentRecordFailed:
			state = ResidentFailed
			lastStateAt = record.Time
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
		case residentRecordUsage:
			if record.Usage == nil {
				return ResidentMetadata{}, errors.New("resident journal: invalid usage record")
			}
			usage = ResidentUsageSnapshot{Usage: *record.Usage, ContextUsed: record.ContextUsed, ContextMax: record.ContextMax, Subscription: record.Subscription}
		}
	}
	if lastStateAt.IsZero() {
		lastStateAt = time.Now().UTC()
	}
	metadata := ResidentMetadata{
		Version: residentJournalVersion, ID: spec.ID, SessionID: spec.SessionID,
		RootCacheID: spec.RootCacheID, ParentSessionID: spec.ParentSessionID, State: state, UpdatedAt: lastStateAt,
		Usage: usage.Usage, ContextUsed: usage.ContextUsed, ContextMax: usage.ContextMax, Subscription: usage.Subscription,
	}
	needsInterruption := state == ResidentQueued || state == ResidentRunning
	needsToolRepair := len(toolCalls) != len(toolResults)
	if needsInterruption || needsToolRepair {
		// A recorded repair is new observable work. A no-op reconciliation,
		// however, must retain the last durable lifecycle time.
		metadata.UpdatedAt = time.Now().UTC()
		dangling := make([]string, 0, len(toolCalls))
		for toolID := range toolCalls {
			if _, finished := toolResults[toolID]; !finished {
				dangling = append(dangling, toolID)
			}
		}
		sort.Strings(dangling)
		for _, toolID := range dangling {
			result, marshalErr := json.Marshal(core.ToolResult{IsError: true, Content: []provider.Content{provider.TextBlock{Text: residentInterruptedText}}})
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
			if err := rebuildResidentResult(dir, spec, metadata, lastFinishedTurn, lastFinishedOutcome, lastAssistantSummary); err != nil {
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
	if err := rebuildResidentResult(dir, spec, metadata, lastFinishedTurn, lastFinishedOutcome, lastAssistantSummary); err != nil {
		return ResidentMetadata{}, err
	}
	return metadata, nil
}

// repairLegacyFalseRecovery removes the exact false-recovery sequence produced
// before resident journals had host ownership: a synthetic interruption result,
// followed only by interruption records, before the live owner records the real
// result for the same call. It deliberately refuses broader or ambiguous
// duplicate histories.
func repairLegacyFalseRecovery(records []residentRecord) ([]residentRecord, bool) {
	for start, record := range records {
		if record.Type != residentRecordToolResult || !isSyntheticInterruption(record.ToolResult) {
			continue
		}
		turnID := activeResidentTurn(records[:start])
		if turnID == "" {
			continue
		}
		end := -1
		interrupted := false
		for index := start + 1; index < len(records); index++ {
			next := records[index]
			if next.Type == residentRecordInterrupted && next.TurnID == turnID {
				interrupted = true
			}
			if next.Type == residentRecordToolResult && next.ToolID == record.ToolID && !isSyntheticInterruption(next.ToolResult) {
				end = index
				break
			}
			if next.Type != residentRecordInterrupted && (next.Type != residentRecordToolResult || next.ToolID != record.ToolID || !isSyntheticInterruption(next.ToolResult)) {
				break
			}
		}
		if end < 0 || !interrupted {
			continue
		}
		repaired := make([]residentRecord, 0, len(records)-(end-start))
		repaired = append(repaired, records[:start]...)
		for _, candidate := range records[start:end] {
			if candidate.Type == residentRecordInterrupted && candidate.TurnID != turnID {
				return nil, false
			}
		}
		repaired = append(repaired, records[end:]...)
		return repaired, true
	}
	return nil, false
}

func activeResidentTurn(records []residentRecord) string {
	active := ""
	for _, record := range records {
		switch record.Type {
		case residentRecordTurnStarted:
			active = record.TurnID
		case residentRecordTurnFinished, residentRecordInterrupted:
			if record.TurnID == active {
				active = ""
			}
		}
	}
	return active
}

func isSyntheticInterruption(raw json.RawMessage) bool {
	var result struct {
		Content []struct {
			Text string
		}
		IsError bool
	}
	if err := json.Unmarshal(raw, &result); err != nil || !result.IsError || len(result.Content) != 1 {
		return false
	}
	return result.Content[0].Text == residentInterruptedText
}

// rewriteTranscript replaces the authoritative transcript while the caller
// owns its lease. The old transcript is retained as a private recovery backup.
func (j *ResidentJournal) rewriteTranscript(records []residentRecord) error {
	if j == nil {
		return errors.New("resident journal: unavailable")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil || j.lease == nil {
		return errors.New("resident journal: closed")
	}
	transcript := filepath.Join(j.dir, residentTranscriptName)
	backup, err := os.CreateTemp(j.dir, ".transcript-backup-*")
	if err != nil {
		return fmt.Errorf("resident journal backup: %w", err)
	}
	backupName := backup.Name()
	if err := backup.Chmod(0o600); err != nil {
		_ = backup.Close()
		_ = os.Remove(backupName)
		return fmt.Errorf("resident journal backup permissions: %w", err)
	}
	source, err := os.Open(transcript)
	if err != nil {
		_ = backup.Close()
		_ = os.Remove(backupName)
		return fmt.Errorf("resident journal backup source: %w", err)
	}
	_, copyErr := io.Copy(backup, source)
	closeSourceErr := source.Close()
	if copyErr == nil {
		copyErr = backup.Sync()
	}
	if closeErr := backup.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = os.Remove(backupName)
		return fmt.Errorf("resident journal backup write: %w", errors.Join(copyErr, closeSourceErr))
	}

	tmp, err := os.CreateTemp(j.dir, ".transcript-repair-*")
	if err != nil {
		return fmt.Errorf("resident journal repair: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("resident journal repair permissions: %w", err)
	}
	encoder := json.NewEncoder(tmp)
	for _, record := range records {
		if len(record.raw) > 0 {
			if _, err := tmp.Write(record.raw); err != nil {
				_ = tmp.Close()
				return fmt.Errorf("resident journal repair write: %w", err)
			}
			continue
		}
		if err := encoder.Encode(record); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("resident journal repair encode: %w", err)
		}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("resident journal repair sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("resident journal repair close: %w", err)
	}
	if err := j.file.Close(); err != nil {
		return fmt.Errorf("resident journal repair close transcript: %w", err)
	}
	j.file = nil
	if err := os.Rename(tmpName, transcript); err != nil {
		return fmt.Errorf("resident journal repair rename: %w", err)
	}
	if err := syncDirectory(j.dir); err != nil {
		return fmt.Errorf("resident journal repair directory sync: %w", err)
	}
	file, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("resident journal repair reopen: %w", err)
	}
	j.file = file
	return nil
}

func rebuildResidentResult(dir string, spec ResidentChildSpec, metadata ResidentMetadata, turnID, outcome, summary string) error {
	if turnID == "" {
		return nil
	}
	resultPath := filepath.Join(dir, residentResultName)
	if existing, err := ReadResidentResult(resultPath); err == nil && existing.ID == spec.ID && existing.TurnID == turnID && existing.State == metadata.State {
		return nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		if removeErr := os.Remove(resultPath); removeErr != nil {
			return fmt.Errorf("resident journal remove corrupt result projection: %w", removeErr)
		}
	}
	result := ResidentResult{Version: residentJournalVersion, ID: spec.ID, TurnID: turnID, State: metadata.State, Summary: summary, CreatedAt: metadata.UpdatedAt}
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
	if _, err := tmp.Write(patch); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, residentPatchName))
}

func residentChildID(childID string) bool {
	childID = strings.TrimSpace(childID)
	return childID != "" && childID != "." && childID != ".." && filepath.Base(childID) == childID
}

func writeResidentProjection(dir, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, name)
	if residentProjectionCurrent(path, data) {
		return nil
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
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("resident projection rename: %w", err)
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("resident projection directory sync: %w", err)
	}
	return nil
}
