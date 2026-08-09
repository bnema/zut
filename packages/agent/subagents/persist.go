package subagents

// On-disk persistence for subagent agents.
//
// Every Spawn writes a meta.json next to the agent's events.jsonl and
// session.json. The file captures identity, resumable lifecycle state, and
// compact dashboard state. On a fresh zut launch, Supervisor.Reload() walks
// <root>/agents/*/meta.json and re-registers every agent without replaying
// its full history. The event log remains the durable transcript source and
// is hydrated only when a caller requests that agent's transcript.
//
// Metadata writes are atomic (tmp + rename). The supervisor is the sole
// writer, so its lifecycle snapshots cannot race with another host process.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// agentMeta is the durable identity record for one agent. Only fields
// the supervisor needs to rebuild an Agent after a restart live here.
// Adding a field leaves it zero when absent; removing or renaming a field
// changes the persisted contract.
//
// Historical fields like `branch` and `isolated` are silently dropped
// by encoding/json's permissive decoder when an older meta.json is
// loaded; we don't need to keep them in the struct.
type agentMeta struct {
	ID               string          `json:"id"`
	Task             string          `json:"task"`
	OriginalTask     string          `json:"original_task,omitempty"`
	Dir              string          `json:"dir"`
	RepositoryRoot   string          `json:"repository_root,omitempty"`
	Started          time.Time       `json:"started"`
	ParentID         string          `json:"parent_id,omitempty"`
	BatchID          string          `json:"batch_id,omitempty"`
	RootSessionID    string          `json:"root_session_id,omitempty"`
	Model            string          `json:"model,omitempty"`
	Provider         string          `json:"provider,omitempty"`
	BaseURL          string          `json:"base_url,omitempty"`
	InsecureTLS      bool            `json:"insecure_tls,omitempty"`
	Reasoning        string          `json:"reasoning,omitempty"`
	FastMode         *bool           `json:"fast_mode,omitempty"`
	Subagent         string          `json:"subagent,omitempty"`
	WorkspaceMode    WorkspaceMode   `json:"workspace_mode,omitempty"`
	WorkspacePath    string          `json:"workspace_path,omitempty"`
	WorkspaceBase    string          `json:"workspace_base,omitempty"`
	WorkspaceCapture CaptureMode     `json:"workspace_capture,omitempty"`
	MaxTurns         int             `json:"max_turns,omitempty"`
	LifetimeTurns    int             `json:"lifetime_turns,omitempty"`
	CurrentRunTurns  int             `json:"current_run_turns,omitempty"`
	Timeout          time.Duration   `json:"timeout,omitempty"`
	Tools            []string        `json:"tools,omitempty"`
	WebSearchPolicy  WebSearchPolicy `json:"web_search_policy,omitempty"`
	Status           Status          `json:"status,omitempty"`
	Activity         string          `json:"activity,omitempty"`
	Finished         time.Time       `json:"finished,omitempty"`
	Error            string          `json:"error,omitempty"`
	ProcessState     ProcessState    `json:"process_state,omitempty"`
	TurnState        TurnState       `json:"turn_state,omitempty"`
	CurrentTurnID    string          `json:"current_turn_id,omitempty"`
	Attempt          int             `json:"attempt,omitempty"`
	ProcessPID       int             `json:"process_pid,omitempty"`
	UpdatedAt        time.Time       `json:"updated_at,omitempty"`
	LastActivity     time.Time       `json:"last_activity,omitempty"`
	ResultRef        string          `json:"result_ref,omitempty"`
	PatchRef         string          `json:"patch_ref,omitempty"`
	ChangedFiles     []string        `json:"changed_files,omitempty"`
	InboxPath        string          `json:"inbox_path"`
	EventLogPath     string          `json:"event_log_path"`
	SessionPath      string          `json:"session_path"`
	// ResumePrompt is a follow-up accepted by subagent_resume but not yet
	// acknowledged by the child with a turn.started event. It is durable so a
	// host exit while the worker is queued does not lose the manager signal.
	ResumePrompt   string               `json:"resume_prompt,omitempty"`
	ResumePromptAt time.Time            `json:"resume_prompt_at,omitempty"`
	ResumePromptID string               `json:"resume_prompt_id,omitempty"`
	ResumeQueue    []queuedResumePrompt `json:"resume_queue,omitempty"`

	// SessionID, when non-empty, scopes the agent to a particular
	// host zut session: the dashboard only shows agents whose
	// SessionID matches the active session. Older meta.json files
	// (and agents spawned outside of any session, e.g. by tests or
	// scripted callers that didn't call SetActiveSession) have an
	// empty SessionID and are visible from every session as a
	// unscoped-agent fallback.
	SessionID string `json:"session_id,omitempty"`
}

func metaPath(stateDir string) string { return filepath.Join(stateDir, "meta.json") }

// writeAgentMeta serialises a's identity into stateDir/meta.json. The
// write is atomic (tmp + rename) so a crash mid-write can't leave a
// half-parsable file that fails Reload.
func writeAgentMeta(stateDir string, a *Agent) error {
	if a == nil {
		return errors.New("subagent meta: nil agent")
	}
	a.persistMu.Lock()
	defer a.persistMu.Unlock()

	fastMode := a.FastMode
	webSearchPolicy := childWebSearchPolicy(a.WebSearchPolicy, a.Subagent, a.Tools)
	a.mu.Lock()
	status := a.status
	activity := a.activity
	finished := a.finished
	resumePrompt := a.resumePromptText
	resumePromptAt := a.resumePromptAt
	resumePromptID := a.resumePromptID
	resumeQueue := append([]queuedResumePrompt(nil), a.resumePromptQueue...)
	errText := ""
	if a.lastErr != nil {
		errText = a.lastErr.Error()
	}
	a.mu.Unlock()
	a.lifecycleMu.Lock()
	processState := a.processState
	turnState := a.turnState
	currentTurnID := a.currentTurnID
	lifetimeTurns := a.LifetimeTurns
	currentRunTurns := a.CurrentRunTurns
	attempt := a.Attempt
	processPID := a.ProcessPID
	updatedAt := a.updatedAt
	lastActivity := a.lastActivity
	resultRef := a.resultRef
	patchRef := a.patchRef
	changedFiles := append([]string(nil), a.changedFiles...)
	a.lifecycleMu.Unlock()
	m := agentMeta{
		ID:               a.ID,
		Task:             a.Task,
		OriginalTask:     a.OriginalTask,
		Dir:              a.Dir,
		RepositoryRoot:   a.RepositoryRoot,
		Started:          a.Started,
		ParentID:         a.ParentID,
		BatchID:          a.BatchID,
		RootSessionID:    a.RootSessionID,
		Model:            a.Model,
		Provider:         a.Provider,
		BaseURL:          a.BaseURL,
		InsecureTLS:      a.InsecureTLS,
		Reasoning:        a.Reasoning,
		FastMode:         &fastMode,
		Subagent:         a.Subagent,
		WorkspaceMode:    a.WorkspaceMode,
		WorkspacePath:    a.WorkspacePath,
		WorkspaceBase:    a.WorkspaceBase,
		WorkspaceCapture: a.WorkspaceCapture,
		MaxTurns:         a.MaxTurns,
		LifetimeTurns:    lifetimeTurns,
		CurrentRunTurns:  currentRunTurns,
		Timeout:          a.Timeout,
		Tools:            append([]string(nil), a.Tools...),
		WebSearchPolicy:  webSearchPolicy,
		Status:           status,
		Activity:         activity,
		Finished:         finished,
		Error:            errText,
		ProcessState:     processState,
		TurnState:        turnState,
		CurrentTurnID:    currentTurnID,
		Attempt:          attempt,
		ProcessPID:       processPID,
		UpdatedAt:        updatedAt,
		LastActivity:     lastActivity,
		ResultRef:        resultRef,
		PatchRef:         patchRef,
		ChangedFiles:     changedFiles,
		InboxPath:        a.InboxPath,
		EventLogPath:     a.EventLogPath,
		SessionPath:      a.SessionPath,
		ResumePrompt:     resumePrompt,
		ResumePromptAt:   resumePromptAt,
		ResumePromptID:   resumePromptID,
		ResumeQueue:      resumeQueue,
		SessionID:        a.SessionID,
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("subagent meta marshal: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("subagents meta dir: %w", err)
	}
	final := metaPath(stateDir)
	// Use a unique temporary file while holding the per-agent persistence
	// lock. A fixed meta.json.tmp is vulnerable to a concurrent writer (or a
	// stale file after a crash) replacing another writer's snapshot.
	tmpFile, err := os.CreateTemp(stateDir, ".meta.json.tmp-*")
	if err != nil {
		return fmt.Errorf("subagents meta temp: %w", err)
	}
	tmp := tmpFile.Name()
	removeTemp := true
	defer func() {
		_ = tmpFile.Close()
		if removeTemp {
			_ = os.Remove(tmp)
		}
	}()
	if err := tmpFile.Chmod(0o600); err != nil {
		return fmt.Errorf("subagents meta permissions: %w", err)
	}
	if _, err := tmpFile.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("subagents meta write: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		return fmt.Errorf("subagents meta sync: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("subagents meta close: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("subagent meta rename: %w", err)
	}
	removeTemp = false
	// Sync the containing directory where the platform supports it so the
	// rename itself survives a host crash, not just the temporary file bytes.
	if err := syncDirectory(stateDir); err != nil {
		return fmt.Errorf("subagents meta directory sync: %w", err)
	}
	return nil
}

// readAgentMeta loads one meta.json. Returns os.ErrNotExist (wrapped)
// when the file is missing so callers can distinguish "no such agent"
// from "corrupt metadata".
func readAgentMeta(stateDir string) (agentMeta, error) {
	var m agentMeta
	b, err := os.ReadFile(metaPath(stateDir))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("subagent meta parse %s: %w", stateDir, err)
	}
	if m.ID == "" {
		return m, fmt.Errorf("subagent meta %s: missing id", stateDir)
	}
	return m, nil
}

func safeAgentID(id string) bool {
	return id != "" && id != "." && id != ".." && filepath.Base(id) == id && !strings.ContainsAny(id, `/\\`)
}

func (f *Supervisor) sanitizeReloadMeta(stateDir, root string, m *agentMeta) error {
	if m == nil {
		return errors.New("subagents reload: nil metadata")
	}
	if m.EventLogPath == "" {
		m.EventLogPath = filepath.Join(stateDir, "events.jsonl")
	}
	if m.SessionPath == "" {
		m.SessionPath = filepath.Join(stateDir, "session.json")
	}
	if filepath.Base(m.EventLogPath) != "events.jsonl" || !pathWithin(m.EventLogPath, stateDir) {
		return fmt.Errorf("subagents reload %s: event log path escapes agent state", stateDir)
	}
	if filepath.Base(m.SessionPath) != "session.json" || !pathWithin(m.SessionPath, stateDir) {
		return fmt.Errorf("subagents reload %s: session path escapes agent state", stateDir)
	}
	if m.InboxPath == "" || !validRuntimeInboxPath(m.InboxPath, m.ID) {
		// Old metadata may point at a runtime directory from a previous
		// environment. Recompute that transient path rather than ever
		// dialing an arbitrary socket supplied by metadata.
		expectedInbox, err := inboxSocketPath(root, m.ID)
		if err != nil {
			return fmt.Errorf("subagents reload %s: inbox path: %w", stateDir, err)
		}
		m.InboxPath = expectedInbox
	}
	return nil
}

func validRuntimeInboxPath(path, id string) bool {
	if !filepath.IsAbs(path) || (filepath.Base(path) != id+".sock" && filepath.Base(path) != shortHash(id)+".sock") {
		return false
	}
	dir := filepath.Dir(path)
	if !strings.HasPrefix(filepath.Base(dir), "zut-subagents-") {
		return false
	}
	bases := []string{os.TempDir(), "/tmp"}
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(runtimeDir) {
		bases = append(bases, runtimeDir)
	}
	for _, base := range bases {
		if pathWithin(dir, filepath.Clean(base)) {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// Reload scans <root>/agents/*/meta.json and re-registers every
// previously-spawned agent as a StatusDetached entry. Agents already
// present in memory are left alone (Reload is idempotent and safe to
// call after Spawn, though in practice the cli invokes it exactly
// once just after New()).
//
// The reloaded agents have no live Runner; the user can:
//   - view their transcript (the dashboard reads from EventLogPath),
//   - resume them via Supervisor.Resume (starts a fresh subprocess on the
//     same worktree / session / inbox path),
//   - remove them (worktree + meta + events log gone).
//
// Reload returns the number of agents loaded plus any per-directory
// error encountered. Malformed entries are skipped rather than
// failing the whole reload — one bad meta.json shouldn't hide the
// rest of the subagents.
func (f *Supervisor) Reload() (loaded int, errs []error) {
	root := filepath.Clean(f.cfg.Root)
	if root == "." {
		return 0, nil
	}
	loaded, errs = f.reloadRoot(root)
	errs = append(errs, f.reloadBatches(root)...)
	return loaded, errs
}

func (f *Supervisor) reloadRoot(root string) (loaded int, errs []error) {
	agentsDir := filepath.Join(root, "agents")
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, []error{fmt.Errorf("subagents reload %s: %w", root, err)}
	}

	// Sort by directory name so the load order is stable across runs.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		stateDir := filepath.Join(agentsDir, name)
		m, err := readAgentMeta(stateDir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			errs = append(errs, err)
			continue
		}
		if filepath.Base(stateDir) != m.ID || !safeAgentID(m.ID) {
			errs = append(errs, fmt.Errorf("subagents reload %s: unsafe or mismatched agent id", stateDir))
			continue
		}
		if err := f.sanitizeReloadMeta(stateDir, root, &m); err != nil {
			errs = append(errs, err)
			continue
		}

		f.mu.Lock()
		if _, exists := f.agents[m.ID]; exists {
			f.mu.Unlock()
			continue
		}
		a := f.buildDetachedAgent(m)
		f.agents[m.ID] = a
		f.order = append(f.order, m.ID)
		f.mu.Unlock()
		loaded++
	}
	return loaded, errs
}

// buildDetachedAgent constructs an Agent from compact metadata with no running
// Runner. Its transcript stays unloaded until a caller requests it, while the
// persisted lifecycle summary keeps the dashboard immediately useful.
//
// The returned Agent has a closed `done` channel because Wait should return
// instantly: there is nothing to wait for.
func (f *Supervisor) buildDetachedAgent(m agentMeta) *Agent {
	// Metadata without a repository identity uses the current RepoRoot;
	// records with one retain their persisted repository so a restart from
	// another checkout cannot silently redirect the child.
	dir := m.Dir
	if m.WorkspaceMode != WorkspaceWorktree && m.RepositoryRoot == "" && f.cfg.RepoRoot != "" {
		// Preserve the best-effort migration only for records without an
		// explicit repository identity.
		dir = f.cfg.RepoRoot
	}
	if dir == "" {
		dir = firstNonEmpty(m.RepositoryRoot, f.cfg.RepoRoot)
	}
	fastMode := f.cfg.FastMode
	if m.FastMode != nil {
		fastMode = *m.FastMode
	}
	turnState := m.TurnState
	if turnState == "" {
		turnState = TurnIdle
	}
	lastActivity := m.LastActivity
	if lastActivity.IsZero() {
		lastActivity = m.Started
	}
	persistedStatus := m.Status
	status := StatusDetached
	activity := strings.TrimSpace(m.Activity)
	processState := m.ProcessState
	legacyEventState := persistedStatus == ""
	if legacyEventState {
		activity = "detached"
		processState = ProcessDetached
	}
	if processState == "" {
		processState = ProcessDetached
	}
	if activity == "" {
		switch persistedStatus {
		case StatusDone:
			activity = "done"
		case StatusFailed:
			activity = "failed"
		case StatusKilled:
			activity = "cancelled"
		default:
			activity = "detached"
		}
	}
	if persistedStatus == StatusRunning || persistedStatus == StatusPending {
		activity = "detached (resume to continue)"
		processState = ProcessDetached
	}
	if inboxLive(m.InboxPath) {
		processState = ProcessAlive
		activity = "live (detached)"
	}
	var lastErr error
	if m.Error != "" {
		lastErr = errors.New(m.Error)
	}
	agentStateDir := f.agentStateDir(m.ID)
	if m.EventLogPath != "" {
		// EventLogPath is <state-dir>/events.jsonl; keep the reloaded
		// agent's state scoped to its own directory. Using the parent
		// agents/ directory here would make Remove delete sibling agents
		// and would make resumed result/patch writes collide.
		agentStateDir = filepath.Dir(m.EventLogPath)
	}
	a := &Agent{
		ID:                m.ID,
		Task:              m.Task,
		OriginalTask:      firstNonEmpty(m.OriginalTask, m.Task),
		Dir:               dir,
		RepositoryRoot:    firstNonEmpty(m.RepositoryRoot, f.cfg.RepoRoot),
		Started:           m.Started,
		ParentID:          m.ParentID,
		BatchID:           m.BatchID,
		RootSessionID:     m.RootSessionID,
		Model:             m.Model,
		Provider:          m.Provider,
		BaseURL:           m.BaseURL,
		InsecureTLS:       m.InsecureTLS,
		Reasoning:         m.Reasoning,
		FastMode:          fastMode,
		Subagent:          m.Subagent,
		WorkspaceMode:     m.WorkspaceMode,
		WorkspacePath:     m.WorkspacePath,
		WorkspaceBase:     m.WorkspaceBase,
		WorkspaceCapture:  m.WorkspaceCapture,
		MaxTurns:          m.MaxTurns,
		LifetimeTurns:     m.LifetimeTurns,
		CurrentRunTurns:   m.CurrentRunTurns,
		Timeout:           m.Timeout,
		Tools:             append([]string(nil), m.Tools...),
		WebSearchPolicy:   childWebSearchPolicy(m.WebSearchPolicy, m.Subagent, m.Tools),
		Attempt:           m.Attempt,
		ProcessPID:        m.ProcessPID,
		InboxPath:         m.InboxPath,
		EventLogPath:      m.EventLogPath,
		SessionPath:       m.SessionPath,
		resumePromptText:  m.ResumePrompt,
		resumePromptAt:    m.ResumePromptAt,
		resumePromptID:    m.ResumePromptID,
		resumePromptQueue: append([]queuedResumePrompt(nil), m.ResumeQueue...),
		SessionID:         m.SessionID,
		inbox:             NewInbox(m.InboxPath),
		status:            status,
		activity:          activity,
		finished:          m.Finished,
		lastErr:           lastErr,
		legacyEventState:  legacyEventState,
		processState:      processState,
		turnState:         turnState,
		currentTurnID:     m.CurrentTurnID,
		updatedAt:         m.UpdatedAt,
		lastActivity:      lastActivity,
		stateDir:          agentStateDir,
		resultRef:         m.ResultRef,
		patchRef:          m.PatchRef,
		changedFiles:      append([]string(nil), m.ChangedFiles...),
		maxOutputBytes:    f.cfg.Policy.MaxOutputBytes,
		maxOutputLines:    f.cfg.Policy.MaxOutputLines,
		done:              make(chan struct{}),
		turnResults:       make(chan *TurnResult, 16),
	}
	if a.updatedAt.IsZero() {
		a.updatedAt = lastActivity
	}
	a.closeDone()

	// The append-only log may contain a terminal event from an earlier
	// attempt. A live socket is stronger evidence for the current process;
	// keep the reloaded entry detached/live so Stop and Remove cannot treat a
	// resumed worker as already finished.
	if inboxLive(a.InboxPath) {
		a.mu.Lock()
		a.status = StatusDetached
		a.activity = "live (detached)"
		a.mu.Unlock()
		a.setProcessState(ProcessAlive)
	}
	resultDir := f.agentStateDir(a.ID)
	if a.EventLogPath != "" {
		resultDir = filepath.Dir(a.EventLogPath)
	}
	if result, err := readTurnResult(resultDir); err == nil && validateTurnResultAgent(result, a.ID) == nil {
		a.setResult(result.Bounded(f.cfg.Policy.MaxOutputBytes, f.cfg.Policy.MaxOutputLines))
	}
	return a
}

// LoadTranscript hydrates one reloaded agent's durable event history. Reload
// deliberately skips this work so startup stays bounded by metadata size;
// callers request it only when opening the transcript or otherwise reading it.
func (f *Supervisor) LoadTranscript(id string) error {
	a := f.Get(id)
	if a == nil {
		return fmt.Errorf("subagent: no such agent %q", id)
	}
	return a.loadTranscript()
}

func (a *Agent) loadTranscript() error {
	if a == nil {
		return nil
	}
	a.transcriptMu.Lock()
	defer a.transcriptMu.Unlock()

	a.mu.Lock()
	if a.transcriptLoaded {
		a.mu.Unlock()
		return nil
	}
	path := a.EventLogPath
	legacyEventState := a.legacyEventState
	a.mu.Unlock()
	var summaryUpdatedAt, summaryLastActivity time.Time
	if !legacyEventState {
		a.lifecycleMu.Lock()
		summaryUpdatedAt = a.updatedAt
		summaryLastActivity = a.lastActivity
		a.lifecycleMu.Unlock()
	}
	if path == "" {
		a.mu.Lock()
		a.transcriptLoaded = true
		a.mu.Unlock()
		return nil
	}

	events, err := ReadEventLog(path)
	if err != nil {
		return fmt.Errorf("read subagent transcript: %w", err)
	}
	if prompt, acceptedAt := a.ResumePromptInfo(); prompt != "" && resumePromptAcknowledged(events, prompt, acceptedAt) {
		a.clearResumePrompt()
	}
	a.mu.Lock()
	if a.transcriptLoaded {
		a.mu.Unlock()
		return nil
	}
	a.transcript = nil
	a.lastAssistant = ""
	a.outputBytes = 0
	a.outputLines = 0
	a.outputTruncated = false
	a.mu.Unlock()

	if legacyEventState {
		// Metadata written before the dashboard summary existed has no
		// terminal status to trust. Preserve its historical event-derived
		// behavior once the user explicitly opens its transcript.
		replayEventsIntoAgent(a, events)
	} else {
		replayTranscriptIntoAgent(a, events)
		a.lifecycleMu.Lock()
		a.updatedAt = summaryUpdatedAt
		a.lastActivity = summaryLastActivity
		a.lifecycleMu.Unlock()
	}
	a.mu.Lock()
	a.transcriptLoaded = true
	a.mu.Unlock()
	return nil
}

// replayEventsIntoAgent re-derives an agent's transcript and last
// known status hint from its event log. Mirrors applyEventToSink in
// runner.go but writes directly to the Agent fields (no Sink because
// the agent isn't being driven by a runner yet).
//
// Status precedence: explicit lifecycle events (agent_stopped) win
// over inferred ones (assistant_message → idle). If the log contains
// no terminator we keep status=StatusDetached so the user can resume.
func resumePromptAcknowledged(events []Event, prompt string, acceptedAt time.Time) bool {
	if prompt == "" || acceptedAt.IsZero() {
		return false
	}
	userDurable := false
	for _, ev := range events {
		if ev.Time.Before(acceptedAt) {
			continue
		}
		if ev.Type == "user_message" && userMessageText(ev) == prompt {
			// The worker emits this event only after the matching session
			// message has been appended and flushed. A crash here leaves the
			// prompt pending, which is intentionally safe to replay.
			userDurable = true
			continue
		}
		if userDurable && isDelegatedTurnStart(ev) {
			return true
		}
	}
	return false
}

func replayEventsIntoAgent(a *Agent, evs []Event) {
	terminal := false
	for _, ev := range evs {
		replayEventTranscript(a, ev)
		switch ev.Type {
		case EventTurnStarted:
			if !isDelegatedTurnStart(ev) {
				continue
			}
			a.setTurnState(TurnRunning, eventTurnID(ev))
		case EventTurnResult, "turn_result":
			if result, err := decodeTurnResultEvent(ev, a.ID, a.maxOutputBytes, a.maxOutputLines); err == nil {
				a.setResult(result)
				switch result.Status {
				case ResultFailed:
					a.setTurnState(TurnFailed, result.TurnID)
				case ResultCanceled:
					a.setTurnState(TurnCanceled, result.TurnID)
				default:
					a.setTurnState(TurnSucceeded, result.TurnID)
				}
			}
		case EventAgentIdle, "agent_idle":
			a.setTurnState(TurnIdle, ev.TurnID)
		case EventAgentExited, "agent_exited":
			terminal = true
			a.mu.Lock()
			a.status = StatusDone
			a.activity = "done (offline)"
			a.mu.Unlock()
		case "agent_stopped":
			terminal = true
			reason, _ := ev.Data["reason"].(string)
			a.mu.Lock()
			switch reason {
			case "cancelled":
				a.status = StatusKilled
				a.activity = "cancelled (offline)"
			case "shutdown":
				a.status = StatusDone
				a.activity = "shutdown (offline)"
			case "exit":
				if code, ok := ev.Data["code"].(float64); ok && code != 0 {
					a.status = StatusFailed
					a.activity = fmt.Sprintf("exit %d (offline)", int(code))
				} else {
					a.status = StatusDone
					a.activity = "done (offline)"
				}
			default:
				a.status = StatusDone
				a.activity = "stopped (offline)"
			}
			a.mu.Unlock()
		}
	}
	if !terminal {
		// Non-terminal log means the previous parent died mid-run.
		// Leave status=StatusDetached but record a hint so the
		// dashboard shows something useful.
		a.mu.Lock()
		if a.activity == "detached" && len(a.transcript) > 0 {
			a.activity = "detached (resume to continue)"
		}
		a.mu.Unlock()
	}
}

// replayTranscriptIntoAgent rebuilds only the visible transcript. Newer
// metadata already contains the authoritative dashboard lifecycle summary,
// so opening history must not overwrite it with an earlier event attempt.
func replayTranscriptIntoAgent(a *Agent, evs []Event) {
	for _, ev := range evs {
		replayEventTranscript(a, ev)
	}
}

func replayEventTranscript(a *Agent, ev Event) {
	if isAssistantStreamBoundary(ev.Type) {
		a.resetStreamingAssistant()
	}
	switch ev.Type {
	case "message.delta":
		if delta, _ := ev.Data["delta"].(string); delta != "" {
			a.appendAssistantDelta(delta)
		}
	case "assistant_message", "user_message":
		var text []string
		if c, ok := ev.Data["content"].([]any); ok {
			for _, blk := range c {
				m, _ := blk.(map[string]any)
				if t, _ := m["type"].(string); t == "text" {
					if txt, _ := m["text"].(string); txt != "" {
						text = append(text, txt)
					}
				}
			}
		}
		message := strings.Join(text, "\n")
		if ev.Type == "assistant_message" {
			a.appendAssistantMessage(message)
		} else {
			a.appendUserMessage(message)
		}
	case "stdout":
		if txt, _ := ev.Data["text"].(string); txt != "" {
			a.appendTranscript(txt)
		}
	case "stderr":
		if txt, _ := ev.Data["text"].(string); txt != "" {
			a.appendTranscript("stderr: " + txt)
		}
	case "error":
		if msg, _ := ev.Data["message"].(string); msg != "" {
			a.appendTranscript("error: " + msg)
		}
	}
}

// Resume re-attaches a Runner to a previously-spawned agent. Durable
// session and event files are kept, while the transient inbox path is
// recalculated for the current runtime environment. Use this to
// continue a subagent session across zut restarts:
//
//	subagentMgr.Reload()
//	a, err := subagentMgr.Resume(ctx, "alpha-12345")
//	subagentMgr.SendUserTurn(a.ID, "where were we?")
//
// The agent must be in a non-running state (Detached, Done, Failed,
// Killed). Resuming a still-running agent returns an error so two
// runners don't race for the same session.
func (f *Supervisor) Resume(ctx context.Context, id string) (*Agent, error) {
	return f.resume(ctx, id, true, "")
}

// ResumeSession continues the existing child session without replaying the
// original task. It is the explicit canonical spelling for Resume.
func (f *Supervisor) ResumeSession(ctx context.Context, id string) (*Agent, error) {
	return f.resume(ctx, id, true, "")
}

// ResumeWithPrompt continues an agent with a manager follow-up while retaining
// its existing session. An idle live worker receives the turn through its
// inbox; an active live worker queues the follow-up for the next idle turn; a
// terminal worker is restarted with the follow-up as its initial prompt,
// avoiding a race with inbox listener startup.
func (f *Supervisor) ResumeWithPrompt(ctx context.Context, id, prompt string) (*Agent, error) {
	return f.ResumeWithPromptBefore(ctx, id, prompt, nil)
}

// ResumeWithPromptBefore is ResumeWithPrompt with a pre-delivery hook. The
// hook runs after the target Agent is known but before a queued or direct
// follow-up is made deliverable. For a restarted worker it runs after the new
// Agent is built and before its runner is scheduled. For queued and direct
// delivery, the hook runs while operationMu is held and must not block or call
// back into Supervisor methods. A non-nil cleanup is called when the operation
// is rejected before delivery.
func (f *Supervisor) ResumeWithPromptBefore(ctx context.Context, id, prompt string, before func(*Agent, string) func()) (*Agent, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("subagent: follow-up prompt is required")
	}

	f.operationMu.Lock()
	existing := f.Get(id)
	if existing == nil {
		f.operationMu.Unlock()
		return nil, fmt.Errorf("subagent: no such agent %q", id)
	}
	status := existing.Status()
	if status == StatusRunning {
		if existing.ProcessState() != ProcessAlive {
			f.operationMu.Unlock()
			return nil, fmt.Errorf("subagent: agent %s is still working; wait for its worker to become available before sending a follow-up", existing.ID)
		}
		if existing.inbox == nil {
			f.operationMu.Unlock()
			return nil, fmt.Errorf("subagent: agent %s has no inbox", existing.ID)
		}
		if err := ctx.Err(); err != nil {
			f.operationMu.Unlock()
			return nil, err
		}
		if existing.TurnState() != TurnRunning && existing.hasQueuedResumePrompt() {
			f.operationMu.Unlock()
			return nil, fmt.Errorf("subagent: agent %s already has a follow-up pending delivery", existing.ID)
		}
		if existing.TurnState() == TurnRunning {
			// Active turns cannot accept a command through the worker inbox yet.
			// Keep the prompt durable and let the idle transition deliver it in
			// order after the current message turn finishes.
			var undo func()
			if before != nil {
				undo = before(existing, prompt)
			}
			acceptedAt := f.cfg.Now()
			commandID := existing.queueResumePrompt(prompt, acceptedAt)
			if err := f.persistAgent(existing); err != nil {
				existing.removeQueuedResumePrompt(commandID)
				if undo != nil {
					undo()
				}
				f.operationMu.Unlock()
				return nil, fmt.Errorf("subagent: persist queued follow-up for %s: %w", existing.ID, err)
			}
			f.operationMu.Unlock()
			return existing, nil
		}
		if existing.TurnState() != TurnIdle {
			f.operationMu.Unlock()
			return nil, fmt.Errorf("subagent: agent %s is still working; wait for it to become idle before sending a follow-up", existing.ID)
		}
		turnID := existing.CurrentTurnID()
		previousRunTurns := existing.resetCurrentRunTurns()
		var undo func()
		if before != nil {
			undo = before(existing, prompt)
		}
		// Reserve the worker before writing to its inbox. A second manager call
		// must not enqueue another turn in the small interval before the worker
		// reports its turn.start event. Persist the prompt first so a host exit
		// before that event can resume and redeliver it.
		previousPrompt := existing.setResumePrompt(prompt, f.cfg.Now())
		existing.setTurnState(TurnQueued, turnID)
		inbox := existing.inbox
		command := NewCommand(CommandTurnStart, existing.ID, turnID, TurnStartPayload{Prompt: prompt, NewRun: true})
		command.MessageID = existing.resumePromptCommandID()
		if err := f.persistAgent(existing); err != nil {
			existing.setTurnState(TurnIdle, turnID)
			existing.setCurrentRunTurns(previousRunTurns)
			existing.restoreResumePrompt(previousPrompt)
			if undo != nil {
				undo()
			}
			f.operationMu.Unlock()
			return nil, fmt.Errorf("subagent: persist follow-up for %s: %w", existing.ID, err)
		}
		f.operationMu.Unlock()

		if err := inbox.SendCommandContext(ctx, command); err != nil {
			retryUnknown := false
			f.operationMu.Lock()
			if existing.Status() == StatusRunning && existing.TurnState() == TurnQueued && existing.CurrentTurnID() == turnID {
				existing.setTurnState(TurnIdle, turnID)
				existing.setCurrentRunTurns(previousRunTurns)
				if errors.Is(err, ErrDeliveryUnknown) {
					existing.rejectActiveResumePrompt()
					retryUnknown = true
				} else {
					existing.restoreResumePrompt(previousPrompt)
					if undo != nil {
						undo()
					}
				}
				if persistErr := f.persistAgent(existing); persistErr != nil {
					err = fmt.Errorf("%w; persist recovery: %v", err, persistErr)
				}
			}
			f.operationMu.Unlock()
			if retryUnknown {
				// The command keeps its durable identity. A retry is safe whether
				// the worker parsed the first write or only received a prefix.
				f.dispatchQueuedResume(ctx, existing)
			}
			return nil, fmt.Errorf("subagent: send follow-up to %s: %w", existing.ID, err)
		}
		return existing, nil
	}
	f.operationMu.Unlock()

	return f.resumeWithHook(ctx, id, true, prompt, before)
}

// dispatchQueuedResumeWithTimeout keeps the idle callback bounded. Unlike a
// manager request, an idle transition has no caller context to propagate.
func (f *Supervisor) dispatchQueuedResumeWithTimeout(a *Agent) {
	timeout := f.cfg.Policy.ReconnectTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	f.dispatchQueuedResume(ctx, a)
}

// dispatchQueuedResume delivers the oldest follow-up accepted during an
// active turn after the worker reports that its inbox can accept a new turn.
// The operation lock serializes this transition with manager resume calls and
// the agent's durable prompt state prevents a host exit from losing it.
func (f *Supervisor) dispatchQueuedResume(ctx context.Context, a *Agent) {
	if ctx == nil {
		ctx = context.Background()
	}
	if a == nil {
		return
	}
	f.operationMu.Lock()
	if a.Status() != StatusRunning || a.ProcessState() != ProcessAlive || a.TurnState() != TurnIdle || a.inbox == nil {
		f.operationMu.Unlock()
		return
	}
	prompt, acceptedAt, commandID, ok := a.startQueuedResumePrompt()
	if !ok {
		f.operationMu.Unlock()
		return
	}
	turnID := a.CurrentTurnID()
	previousRunTurns := a.resetCurrentRunTurns()
	a.setTurnState(TurnQueued, turnID)
	inbox := a.inbox
	command := NewCommand(CommandTurnStart, a.ID, turnID, TurnStartPayload{Prompt: prompt, NewRun: true})
	command.MessageID = commandID
	if err := f.persistAgent(a); err != nil {
		a.restoreResumePromptToQueue(prompt, acceptedAt)
		a.setCurrentRunTurns(previousRunTurns)
		a.setTurnState(TurnIdle, turnID)
		f.operationMu.Unlock()
		return
	}
	f.operationMu.Unlock()

	if err := inbox.SendCommandContext(ctx, command); err == nil {
		return
	}

	// A failed delivery must put the prompt back at the head of the queue.
	// Keep the worker idle so a later retry, or an explicit resume, can send
	// it without skipping any subsequent follow-ups.
	f.operationMu.Lock()
	if a.Status() == StatusRunning && a.ProcessState() == ProcessAlive && a.TurnState() == TurnQueued && a.CurrentTurnID() == turnID {
		if a.restoreResumePromptToQueue(prompt, acceptedAt) {
			a.setCurrentRunTurns(previousRunTurns)
			a.setTurnState(TurnIdle, turnID)
			if persistErr := f.persistAgent(a); persistErr != nil {
				// persistAgent records the failure on the agent. Keep the explicit
				// check here because this recovery write is the only durable copy
				// of the restored queue head.
				a.recordPersistenceError(persistErr)
			}
		}
	}
	f.operationMu.Unlock()
}

// RestartTask intentionally runs the stored original task again in a fresh
// worker attempt. Callers should use ResumeSession for normal recovery.
func (f *Supervisor) RestartTask(ctx context.Context, id string) (*Agent, error) {
	return f.resume(ctx, id, false, "")
}

func (f *Supervisor) resume(ctx context.Context, id string, resuming bool, resumePrompt string) (*Agent, error) {
	return f.resumeWithHook(ctx, id, resuming, resumePrompt, nil)
}

func (f *Supervisor) resumeWithHook(ctx context.Context, id string, resuming bool, resumePrompt string, before func(*Agent, string) func()) (*Agent, error) {
	f.operationMu.Lock()
	defer f.operationMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	existing := f.Get(id)
	if existing == nil {
		return nil, fmt.Errorf("subagent: no such agent %q", id)
	}
	existing.mu.Lock()
	st := existing.status
	existing.mu.Unlock()
	if st == StatusRunning || st == StatusPending {
		return nil, fmt.Errorf("subagent: agent %s is still %s; stop it first", existing.ID, st)
	}
	select {
	case <-existing.done:
	default:
		return nil, fmt.Errorf("subagent: agent %s is still finalizing; wait before resuming", existing.ID)
	}
	// A reloaded worker may still be alive even though this supervisor has no
	// in-process Runner (the old host can disappear after the child daemon
	// starts). Fence resume before touching the shared session/worktree: a
	// second worker must never own the same inbox and files concurrently.
	if inboxLive(existing.InboxPath) {
		return nil, fmt.Errorf("subagent: agent %s still has a live worker; send shutdown or stop it first", existing.ID)
	}
	// A fresh Reload does not hydrate its event log. Before replaying a pending
	// follow-up, hydrate it to see whether the child already acknowledged the
	// turn before the previous host exited.
	if resuming && existing.resumePrompt() != "" {
		if err := existing.loadTranscript(); err != nil {
			return nil, fmt.Errorf("subagent: replay pending follow-up: %w", err)
		}
	}
	stateDir := existing.stateDirectory(f.cfg.Root)
	lease, err := acquireAgentLease(stateDir)
	if err != nil {
		return nil, fmt.Errorf("subagents resume lease: %w", err)
	}
	leaseOwned := true
	defer func() {
		if leaseOwned {
			_ = lease.Close()
		}
	}()
	existingSnapshot := existing.Snapshot()

	// Rebuild from the meta record so we don't carry stale runner
	// state from a previous incarnation. The inbox is transient and
	// may have been persisted under an incompatible filesystem by an
	// older zut version, so always select it again on resume.
	inboxPath, err := inboxSocketPath(f.cfg.Root, existing.ID)
	if err != nil {
		return nil, fmt.Errorf("subagent inbox path: %w", err)
	}
	now := f.cfg.Now()
	fastMode := existing.FastMode
	lifetimeTurns := existing.LifetimeTurnsValue()
	currentRunTurns := existing.CurrentRunTurnsValue()
	pendingResume := existing.resumePromptState()
	pendingResumePrompt, pendingResumePromptAt, pendingResumePromptID := pendingResume.Prompt, pendingResume.AcceptedAt, pendingResume.CommandID
	pendingResumeQueue := existing.resumePromptQueueSnapshot()
	if pendingResumePrompt != "" && pendingResumePromptID == "" {
		pendingResumePromptID = uuid.NewString()
	}
	if resuming && pendingResumePrompt == "" && len(pendingResumeQueue) > 0 {
		// A host can exit after accepting an active-worker follow-up but before
		// dispatching it. Promote the oldest durable prompt before accepting a
		// new one so scheduler and completion-tracker order stay aligned.
		queued := pendingResumeQueue[0]
		pendingResumePrompt = queued.Prompt
		pendingResumePromptAt = queued.AcceptedAt
		pendingResumePromptID = queued.CommandID
		if pendingResumePromptID == "" {
			pendingResumePromptID = uuid.NewString()
		}
		pendingResumeQueue = pendingResumeQueue[1:]
	}
	hadPendingResumePrompt := pendingResumePrompt != ""
	if resumePrompt != "" {
		currentRunTurns = 0
		if pendingResumePrompt == "" {
			pendingResumePrompt = resumePrompt
			pendingResumePromptAt = now
			pendingResumePromptID = uuid.NewString()
		} else {
			pendingResumeQueue = append(pendingResumeQueue, queuedResumePrompt{Prompt: resumePrompt, AcceptedAt: now, CommandID: uuid.NewString()})
		}
	}
	if resuming && pendingResumePrompt != "" {
		// A retained follow-up is an explicit new run, including a queued
		// follow-up recovered after a host restart. Preserve lifetime turns but
		// never carry the previous run's allowance into this one.
		currentRunTurns = 0
	}
	if !resuming {
		currentRunTurns = 0
	}
	m := agentMeta{
		ID: existing.ID, Task: existing.Task,
		OriginalTask: existing.OriginalTask,
		Dir:          existing.Dir, RepositoryRoot: existing.RepositoryRoot, Started: existing.Started,
		ParentID: existing.ParentID, BatchID: existing.BatchID, RootSessionID: existing.RootSessionID,
		Model: existing.Model, Provider: existing.Provider,
		BaseURL: existing.BaseURL, InsecureTLS: existing.InsecureTLS,
		Reasoning: existing.Reasoning, FastMode: &fastMode,
		Subagent:      existing.Subagent,
		WorkspaceMode: existing.WorkspaceMode, WorkspacePath: existing.WorkspacePath,
		WorkspaceBase: existing.WorkspaceBase, WorkspaceCapture: existing.WorkspaceCapture,
		MaxTurns: existing.MaxTurns, LifetimeTurns: lifetimeTurns, CurrentRunTurns: currentRunTurns, Timeout: existing.Timeout, Tools: append([]string(nil), existing.Tools...),
		WebSearchPolicy: childWebSearchPolicy(existing.WebSearchPolicy, existing.Subagent, existing.Tools),
		CurrentTurnID:   existingSnapshot.CurrentTurnID, Attempt: existingSnapshot.Attempt,
		SessionID: existing.SessionID,
		InboxPath: inboxPath, EventLogPath: existing.EventLogPath,
		SessionPath:    existing.SessionPath,
		ResumePrompt:   pendingResumePrompt,
		ResumePromptAt: pendingResumePromptAt,
		ResumePromptID: pendingResumePromptID,
		ResumeQueue:    pendingResumeQueue,
	}

	if len(m.Tools) == 0 && len(f.cfg.Policy.AllowedTools) > 0 {
		m.Tools = append([]string(nil), f.cfg.Policy.AllowedTools...)
	}
	if err := f.validateSpawnOptions(SpawnRequest{MaxTurns: m.MaxTurns, Tools: m.Tools}); err != nil {
		return nil, err
	}
	workspaceMode := m.WorkspaceMode
	if workspaceMode == "" {
		workspaceMode = WorkspaceShared
	}
	if workspaceMode != WorkspaceShared && workspaceMode != WorkspaceWorktree {
		return nil, fmt.Errorf("subagents: unknown workspace mode %q", workspaceMode)
	}
	if m.WorkspaceCapture != "" && m.WorkspaceCapture != CapturePatch && m.WorkspaceCapture != CaptureDiff {
		return nil, fmt.Errorf("subagents: unknown workspace capture mode %q", m.WorkspaceCapture)
	}
	f.mu.Lock()
	allowedRoots := append([]string(nil), f.cfg.Policy.AllowedRoots...)
	f.mu.Unlock()
	repositoryRoot := firstNonEmpty(m.RepositoryRoot, m.Dir, f.cfg.RepoRoot)
	if err := validateWorkspaceRoot(repositoryRoot, workspaceMode, allowedRoots); err != nil {
		return nil, err
	}
	maxTurns := m.MaxTurns
	if maxTurns <= 0 {
		maxTurns = f.cfg.Policy.MaxTurns
	}
	existingWorkspacePath := ""
	if workspaceMode == WorkspaceWorktree {
		existingWorkspacePath = m.WorkspacePath
	}
	workspace, err := PrepareWorkspace(ctx, WorkspaceRequest{
		Mode:           workspaceMode,
		RepositoryRoot: repositoryRoot,
		StateDir:       stateDir,
		AgentID:        existing.ID,
		Base:           m.WorkspaceBase,
		Capture:        m.WorkspaceCapture,
		AllowedRoots:   allowedRoots,
		ExistingPath:   existingWorkspacePath,
	})
	if err != nil {
		return nil, err
	}
	m.Dir = workspace.Dir()
	m.RepositoryRoot = workspace.RepositoryRoot()
	m.WorkspacePath = workspace.Dir()
	m.WorkspaceMode = workspace.Mode()
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = f.cfg.Policy.DefaultTimeout
	}
	runCtx, cancel := f.workerContext()
	a := &Agent{
		ID:                m.ID,
		Task:              m.Task,
		OriginalTask:      firstNonEmpty(m.OriginalTask, m.Task),
		Dir:               m.Dir,
		RepositoryRoot:    firstNonEmpty(m.RepositoryRoot, f.cfg.RepoRoot),
		Started:           m.Started,
		ParentID:          m.ParentID,
		BatchID:           m.BatchID,
		RootSessionID:     m.RootSessionID,
		Model:             m.Model,
		Provider:          m.Provider,
		BaseURL:           m.BaseURL,
		InsecureTLS:       m.InsecureTLS,
		Reasoning:         m.Reasoning,
		FastMode:          fastMode,
		Subagent:          m.Subagent,
		SessionID:         m.SessionID,
		WorkspaceMode:     m.WorkspaceMode,
		WorkspacePath:     m.WorkspacePath,
		WorkspaceBase:     m.WorkspaceBase,
		WorkspaceCapture:  m.WorkspaceCapture,
		MaxTurns:          maxTurns,
		LifetimeTurns:     m.LifetimeTurns,
		CurrentRunTurns:   m.CurrentRunTurns,
		Timeout:           timeout,
		Attempt:           m.Attempt,
		HeartbeatInterval: f.cfg.Policy.HeartbeatInterval,
		Tools:             append([]string(nil), m.Tools...),
		WebSearchPolicy:   childWebSearchPolicy(m.WebSearchPolicy, m.Subagent, m.Tools),
		InboxPath:         m.InboxPath,
		EventLogPath:      m.EventLogPath,
		SessionPath:       m.SessionPath,
		Resuming:          resuming,
		resumePromptText:  pendingResumePrompt,
		resumePromptAt:    pendingResumePromptAt,
		resumePromptID:    m.ResumePromptID,
		resumePromptQueue: append([]queuedResumePrompt(nil), pendingResumeQueue...),
		inbox:             NewInbox(m.InboxPath),
		status:            StatusPending,
		activity:          "resuming",
		transcriptLoaded:  true,
		processState:      ProcessPending,
		turnState:         TurnQueued,
		updatedAt:         now,
		lastActivity:      now,
		currentTurnID:     m.CurrentTurnID,
		stateDir:          stateDir,
		lease:             lease,
		maxOutputBytes:    f.cfg.Policy.MaxOutputBytes,
		maxOutputLines:    f.cfg.Policy.MaxOutputLines,
		done:              make(chan struct{}),
		turnResults:       make(chan *TurnResult, 16),
	}
	// Carry the previous transcript forward so the dashboard doesn't
	// flash empty between resume and the first new event.
	prev := existing.Transcript()
	if len(prev) > 0 {
		a.appendTranscript(strings.Join(prev, "\n"))
	}
	a.ctx, a.cancel = runCtx, cancel
	a.persistFn = f.persistAgent
	a.setOnTurnIdle(func() { f.dispatchQueuedResumeWithTimeout(a) })
	a.workspaceCleanup = func() error { return workspace.Cleanup(context.Background()) }
	a.workspaceCapture = func() (WorkspaceCapture, error) { return workspace.Capture(context.Background()) }
	a.runner = f.cfg.NewRunner(a)
	if err := writeAgentMeta(a.stateDirectory(f.cfg.Root), a); err != nil {
		if a.cancel != nil {
			a.cancel()
		}
		_ = workspace.Cleanup(context.Background())
		return nil, fmt.Errorf("subagents resume metadata: %w", err)
	}

	f.mu.Lock()
	f.agents[a.ID] = a
	f.queue = append(f.queue, a)
	// Keep the agent's slot in f.order; replacing in-place avoids
	// reshuffling the dashboard's row ordering on resume.
	found := false
	for _, k := range f.order {
		if k == a.ID {
			found = true
			break
		}
	}
	if !found {
		f.order = append(f.order, a.ID)
	}
	f.mu.Unlock()
	leaseOwned = false

	// Refreshing metadata happened before queue admission, so a later
	// supervisor can reconstruct this resumed attempt even if the runner
	// has not reached its first event yet. Register the turn before the
	// scheduler can start the worker and emit a fast turn_end. A durable pending
	// prompt is consumed before the newly requested prompt, so tracker
	// registrations must preserve that scheduler order.
	if before != nil {
		if hadPendingResumePrompt {
			_ = before(a, pendingResumePrompt)
		}
		if resumePrompt != "" {
			// No rejecting operation remains before scheduling, so there is no path
			// that needs the hook's rollback after this point.
			_ = before(a, resumePrompt)
		}
	}
	f.armQueueTimeout(a)
	f.schedule()
	return a, nil
}
