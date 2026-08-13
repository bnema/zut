// Package subagents implements zut's subagent supervisor.
//
// A Supervisor manages headless, long-lived zut subprocesses and keeps their
// process/turn state, event logs, sessions, results, and optional worktree
// artifacts durable across supervisor restarts.
//
// Shared workspaces preserve the historical behavior, while an explicit
// worktree request gives a child an isolated checkout whose patch is captured
// before cleanup. The Runner abstraction lets tests inject a fake process;
// production uses `zut --subagent-worker ...`.
package subagents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Status is the high-level lifecycle state of an Agent.
type Status string

const (
	StatusPending  Status = "pending"  // created, not started yet
	StatusRunning  Status = "running"  // Runner.Run is in flight
	StatusDone     Status = "done"     // Runner.Run returned nil
	StatusFailed   Status = "failed"   // Runner.Run returned an error
	StatusKilled   Status = "killed"   // Stop() called before completion
	StatusDetached Status = "detached" // reloaded from disk; no live runner
)

// Config configures a Supervisor.
type Config struct {
	// Context is the supervisor lifetime. Child workers use it as their
	// parent context rather than inheriting an individual tool/model-turn
	// context. A nil value means context.Background().
	Context context.Context

	// Root is the directory under which per-agent state files live.
	// Typically <ZutHome>/subagents, but tests pass a tempdir.
	Root string

	// Policy owns concurrency, timeout, output, and path safety limits.
	Policy SubagentPolicy

	// RepoRoot is the repository used by spawned agents. Shared-mode children
	// run here; worktree-mode children use an isolated checkout created from
	// this root.
	RepoRoot string

	// Provider is the active host provider. It is used only to decide whether
	// an explicit child provider may inherit the host endpoint settings.
	Provider string

	// FastMode is copied to every newly spawned child and persisted so
	// child subprocesses use the same OpenAI service-tier setting.
	FastMode bool

	// WebSearchPolicy is the already-resolved capability of the parent
	// session. Generic children may inherit it only when the subagent policy
	// also permits web_search; named profiles resolve their own explicit
	// tools opt-in at SpawnReq.
	WebSearchPolicy WebSearchPolicy

	// BaseURL and InsecureTLS are the effective provider connection
	// settings inherited by newly spawned children. They are copied to
	// each Agent and persisted so a reload/resume does not silently fall
	// back to the child's independent provider configuration.
	BaseURL     string
	InsecureTLS bool

	// NewRunner produces the Runner for an Agent. If nil, the default
	// `zut --subagent-worker ...` exec runner is used. Tests inject a fake
	// here.
	NewRunner func(a *Agent) Runner

	// ResolveCredential resolves the credential inherited by a child
	// process. The default runner transfers it over the child's stdin,
	// keeping command-backed credentials out of argv and the environment.
	ResolveCredential func(ctx context.Context, provider string) (Credential, error)

	// TraceDir enables local execution tracing. When non-empty, the supervisor
	// creates one private bundle beneath this directory and is its sole writer.
	// An empty directory leaves tracing disabled.
	TraceDir  string
	TraceMode TraceMode

	// Now is a clock seam for tests; defaults to time.Now.
	Now func() time.Time
}

// Credential is the resolved authentication state inherited by a subagent child.
type Credential struct {
	Value     string `json:"value"`
	Method    string `json:"method"`
	AccountID string `json:"account_id,omitempty"`
}

// Runner executes one agent task. Run blocks until the task finishes,
// is cancelled via ctx, or hits an unrecoverable error.
//
// Run should report progress by writing short human-readable strings
// to the activity channel and final transcript text to transcript.
// Both channels are non-blocking sinks owned by the Supervisor; if the
// dashboard isn't reading, sends are dropped.
type Runner interface {
	Run(ctx context.Context, sink Sink) error
}

// Sink is how a Runner reports activity and transcript back to the
// supervisor. All methods are safe to call from any goroutine and
// never block.
type Sink interface {
	// Activity sets the one-line "what is this agent doing right now"
	// string shown in the dashboard.
	Activity(msg string)
	// Transcript appends a chunk of agent output (typically a final
	// assistant message) to the agent's running transcript.
	Transcript(chunk string)
}

// Supervisor supervises a set of Agents.
type Supervisor struct {
	cfg Config

	mu             sync.Mutex
	operationMu    sync.Mutex
	lifetimeCtx    context.Context
	lifetimeCancel context.CancelFunc
	agents         map[string]*Agent
	order          []string // creation order for stable listing
	queue          []*Agent
	active         int
	activeByParent map[string]int
	activeByBatch  map[string]int
	batches        map[string]*Batch
	trace          *TraceWriter

	// activeSession is the host session id the dashboard is
	// currently scoped to. When non-empty, SnapshotAll filters out
	// agents whose SessionID doesn't match (unscoped
	// agents with SessionID == "" are always shown). Spawn stamps
	// new agents with this value so they appear only in the session
	// that created them. When empty (the default), the historical
	// "show everything" behaviour is preserved — important for
	// tests and any scripted use of the Supervisor that doesn't bother
	// with sessions.
	activeSession string
}

// New constructs a Supervisor from cfg. Missing config fields are filled
// with defaults.
func New(cfg Config) *Supervisor {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NewRunner == nil {
		cfg.NewRunner = func(a *Agent) Runner {
			return &execRunner{
				agent:             a,
				resolveCredential: cfg.ResolveCredential,
				GracePeriod:       cfg.Policy.CancelGracePeriod,
			}
		}
	}
	cfg.Policy.normalize()
	trace := NewMemoryTraceWriter()
	if cfg.TraceDir != "" {
		// Tracing is observational only: inability to create its optional local
		// bundle must not prevent the supervisor from running workers. The
		// in-memory trace remains the sole status projection in that case.
		if bundle, err := NewTraceWriter(filepath.Join(cfg.TraceDir, uuid.NewString()), cfg.TraceMode); err == nil {
			trace = bundle
		} else {
			trace.Record(TraceEvent{Type: "trace.bundle.failed", Data: map[string]any{"error_kind": "create"}})
		}
	}
	lifetimeParent := cfg.Context
	if lifetimeParent == nil {
		lifetimeParent = context.Background()
	}
	lifetimeCtx, lifetimeCancel := context.WithCancel(lifetimeParent)
	return &Supervisor{
		cfg:            cfg,
		lifetimeCtx:    lifetimeCtx,
		lifetimeCancel: lifetimeCancel,
		agents:         map[string]*Agent{},
		activeByParent: map[string]int{},
		activeByBatch:  map[string]int{},
		batches:        map[string]*Batch{},
		trace:          trace,
	}
}

// workerContext derives a child worker context from the supervisor lifetime.
// Turn timeouts are enforced by the worker while a delegated turn is active;
// the process remains cancellable while idle so it can accept follow-ups.
func (f *Supervisor) workerContext() (context.Context, context.CancelFunc) {
	base := f.lifetimeCtx
	if base == nil {
		base = context.Background()
	}
	return context.WithCancel(base)
}

// SetRepoRoot updates the shared working directory for subsequently
// spawned agents. Existing agents keep the directory they were started
// with; callers use this when the host session changes cwd.
func (f *Supervisor) SetRepoRoot(root string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg.RepoRoot = root
}

// RepoRoot returns the working directory used by subsequently spawned
// agents.
func (f *Supervisor) RepoRoot() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg.RepoRoot
}

// MaxTurns returns the maximum prompt-level turns allowed for subsequently
// spawned agents.
func (f *Supervisor) MaxTurns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg.Policy.MaxTurns
}

// SetFastMode updates the fast-mode setting for subsequently spawned
// children. Existing agents keep the setting captured at their spawn
// boundary so resumes remain deterministic.
func (f *Supervisor) SetFastMode(enabled bool) {
	f.mu.Lock()
	f.cfg.FastMode = enabled
	f.mu.Unlock()
}

// FastMode reports the setting used for subsequently spawned children.
func (f *Supervisor) FastMode() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg.FastMode
}

// SetWebSearchPolicy updates the parent capability used for subsequently
// spawned generic children. Existing agents retain their persisted decision.
func (f *Supervisor) SetWebSearchPolicy(policy WebSearchPolicy) {
	f.mu.Lock()
	f.cfg.WebSearchPolicy = policy
	f.mu.Unlock()
}

// WebSearchPolicy reports the parent capability used for new generic
// children.
func (f *Supervisor) WebSearchPolicy() WebSearchPolicy {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg.WebSearchPolicy
}

// SetProvider updates the provider used by subsequently spawned children.
// Existing agents keep their persisted provider selection.
func (f *Supervisor) SetProvider(provider string) {
	f.mu.Lock()
	f.cfg.Provider = strings.TrimSpace(provider)
	f.mu.Unlock()
}

// SetProviderSettings updates the effective connection settings used by
// subsequently spawned children. Existing agents keep their persisted values
// so a model/provider switch cannot silently change a running or resumed
// worker's endpoint.
func (f *Supervisor) SetProviderSettings(baseURL string, insecureTLS bool) {
	f.mu.Lock()
	f.cfg.BaseURL = strings.TrimSpace(baseURL)
	f.cfg.InsecureTLS = insecureTLS
	f.mu.Unlock()
}

// ProviderSettings returns the settings used by subsequently spawned
// children. It is primarily useful to host integrations that need to verify
// the manager follows the active provider selection.
func (f *Supervisor) ProviderSettings() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg.BaseURL, f.cfg.InsecureTLS
}

// SetActiveSession scopes the dashboard view (and Spawn stamping)
// to a particular host zut session id. Pass empty to clear the
// scope and revert to "show every agent" (the original behaviour).
//
// Existing in-memory agents keep their SessionID; only the filter
// applied at snapshot time changes. So swapping the active session
// with /sessions instantly re-narrows the dashboard without
// touching any agent state.
func (f *Supervisor) SetActiveSession(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activeSession = id
}

// ActiveSession returns the current scope, mostly for tests and
// diagnostics. Empty means "no scope; show everything".
func (f *Supervisor) ActiveSession() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.activeSession
}

// agentStateDir is the per-agent state directory laid out as:
//
//	<root>/agents/<id>/
//	  events.jsonl   durable event log (runner-owned)
//	  session.json   persistent agent session (child-owned)
//	  meta.json      static metadata (id, task)
//
// The transient unix socket inbox lives in a probed runtime directory,
// which may be on a different filesystem from this durable state.
func (f *Supervisor) agentStateDir(id string) string {
	return filepath.Join(f.cfg.Root, "agents", id)
}

func (f *Supervisor) uniqueAgentIDLocked(base string) string {
	id := base
	for suffix := 2; ; suffix++ {
		_, inMemory := f.agents[id]
		_, onDiskErr := os.Stat(f.agentStateDir(id))
		if !inMemory && onDiskErr != nil {
			return id
		}
		id = fmt.Sprintf("%s-%d", base, suffix)
	}
}

// SpawnRequest configures a Spawn. Only Task is required; the rest
// are optional. Model + Provider, when set, get baked into the
// child argv as --model / --provider so the agent runs against the
// chosen model regardless of the parent's current selection. A named
// Subagent is passed to the child as --subagent, where it resolves the
// markdown profile and applies its instructions. Reasoning is passed
// as --reasoning when set. FastMode overrides the host setting when a
// selected profile declares a fastMode preference.
type SpawnRequest struct {
	Task     string
	Model    string // optional override; child resolves default if empty
	Provider string // optional override; usually paired with Model
	BaseURL  string // optional provider endpoint override
	// InsecureTLS enables the child to skip TLS verification for its
	// effective custom provider endpoint.
	InsecureTLS bool
	Reasoning   string // optional reasoning/thinking level override
	// FastMode is an optional profile override. A non-nil value enables or
	// disables fast mode for this child; nil inherits Config.FastMode.
	FastMode *bool
	Subagent string // optional named markdown profile

	// WebSearchPolicy is an internal child capability override. Generic
	// requests normally inherit the supervisor's parent decision; named
	// requests use Allow only when their profile explicitly lists web_search.
	WebSearchPolicy WebSearchPolicy

	// ParentID is metadata used for per-parent scheduling limits. A
	// non-empty RequesterAgentID identifies an untrusted child request;
	// recursive spawning is rejected in v1.
	ParentID         string
	BatchID          string
	RootSessionID    string
	RequesterAgentID string

	// Required keeps this delegated turn on the parent's completion path. The
	// supervisor persists its outcome while the manager remains asynchronous.
	Required         bool
	Timeout          time.Duration
	MaxTurns         int
	Tools            []string
	WorkspaceMode    WorkspaceMode
	WorkspaceBase    string
	WorkspaceCapture CaptureMode
}

// Spawn creates a new Agent for the given task, allocates its on-disk
// state directory (events log, inbox socket path, session file path), and
// queues it for the manager-owned scheduler. The returned Agent is pending
// when the concurrency budget is full and transitions to running when a slot
// opens. This is the historical signature; callers that want to override the
// child's model or workspace use SpawnReq instead.
func (f *Supervisor) Spawn(ctx context.Context, task string) (*Agent, error) {
	return f.SpawnReq(ctx, SpawnRequest{Task: task})
}

// unqualifiedModelID removes a redundant provider prefix from a model ID.
// Model IDs themselves may contain slashes, so only the explicitly selected
// provider's exact prefix is removed.
func unqualifiedModelID(provider, model string) string {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" {
		return model
	}
	return strings.TrimPrefix(model, provider+"/")
}

// SpawnReq is the full-fat variant of Spawn that accepts a SpawnRequest.
// Existing callers can keep using Spawn; new code that wants to pin the
// child's model, policy, or workspace uses this.
func (f *Supervisor) SpawnReq(ctx context.Context, req SpawnRequest) (*Agent, error) {
	task := strings.TrimSpace(req.Task)
	if task == "" {
		return nil, errors.New("subagents: empty task")
	}
	if req.RequesterAgentID != "" {
		return nil, errors.New("subagents: child-originated spawning is disabled")
	}
	if err := f.validateSpawnRequest(req); err != nil {
		return nil, err
	}
	req.Model = unqualifiedModelID(req.Provider, req.Model)
	if ctx == nil {
		ctx = context.Background()
	}

	now := f.cfg.Now()
	baseID := newAgentID(task, now)

	f.mu.Lock()
	id := f.uniqueAgentIDLocked(baseID)
	dir := f.cfg.RepoRoot
	fastMode := f.cfg.FastMode
	configProvider := f.cfg.Provider
	configBaseURL := f.cfg.BaseURL
	configInsecureTLS := f.cfg.InsecureTLS
	sessionID := f.activeSession
	f.mu.Unlock()
	fastModeOverridesHost := false
	if req.FastMode != nil {
		fastModeOverridesHost = !fastMode && *req.FastMode
		fastMode = *req.FastMode
	}
	childProvider := strings.TrimSpace(req.Provider)
	inheritProviderSettings := childProvider == "" || configProvider == "" || strings.EqualFold(childProvider, strings.TrimSpace(configProvider))
	baseURL := strings.TrimSpace(req.BaseURL)
	if baseURL == "" && inheritProviderSettings {
		baseURL = strings.TrimSpace(configBaseURL)
	}
	insecureTLS := req.InsecureTLS || (inheritProviderSettings && configInsecureTLS)

	stateDir := f.agentStateDir(id)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("subagents state dir: %w", err)
	}
	lease, err := acquireAgentLease(stateDir)
	if err != nil {
		return nil, fmt.Errorf("subagents agent lease: %w", err)
	}
	leaseOwned := true
	defer func() {
		if leaseOwned {
			_ = lease.Close()
		}
	}()
	logPath := filepath.Join(stateDir, "events.jsonl")
	sessionPath := filepath.Join(stateDir, "session.json")
	// Keep the transient unix socket outside the durable state root.
	// ZUT_HOME may live on a shared or network filesystem that supports
	// regular state files but cannot host unix socket nodes.
	inboxPath, err := inboxSocketPath(f.cfg.Root, id)
	if err != nil {
		_ = lease.Close()
		leaseOwned = false
		_ = os.RemoveAll(stateDir)
		return nil, fmt.Errorf("subagents inbox path: %w", err)
	}

	workspaceMode := req.WorkspaceMode
	if workspaceMode == "" {
		workspaceMode = WorkspaceShared
	}
	workspace, err := PrepareWorkspace(ctx, WorkspaceRequest{
		Mode:           workspaceMode,
		RepositoryRoot: dir,
		StateDir:       stateDir,
		AgentID:        id,
		Base:           req.WorkspaceBase,
		Capture:        req.WorkspaceCapture,
		AllowedRoots:   append([]string(nil), f.cfg.Policy.AllowedRoots...),
	})
	if err != nil {
		_ = lease.Close()
		leaseOwned = false
		_ = os.RemoveAll(stateDir)
		return nil, err
	}
	dir = workspace.Dir()
	maxTurns := req.MaxTurns
	if maxTurns <= 0 {
		maxTurns = f.cfg.Policy.MaxTurns
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = f.cfg.Policy.DefaultTimeout
	}
	runCtx, cancel := f.workerContext()
	tools := append([]string(nil), req.Tools...)
	if len(tools) == 0 && len(f.cfg.Policy.AllowedTools) > 0 {
		tools = append([]string(nil), f.cfg.Policy.AllowedTools...)
	}
	webSearchPolicy := f.resolveWebSearchPolicy(req)

	a := &Agent{
		ID:                id,
		Task:              task,
		OriginalTask:      task,
		Dir:               dir,
		RepositoryRoot:    workspace.RepositoryRoot(),
		WorkspacePath:     workspace.Dir(),
		Started:           now,
		ParentID:          strings.TrimSpace(req.ParentID),
		BatchID:           strings.TrimSpace(req.BatchID),
		RootSessionID:     strings.TrimSpace(req.RootSessionID),
		Model:             strings.TrimSpace(req.Model),
		Provider:          strings.TrimSpace(req.Provider),
		BaseURL:           baseURL,
		InsecureTLS:       insecureTLS,
		Reasoning:         strings.TrimSpace(req.Reasoning),
		FastMode:          fastMode,
		Subagent:          strings.TrimSpace(req.Subagent),
		SessionID:         sessionID,
		WorkspaceMode:     workspaceMode,
		WorkspaceBase:     strings.TrimSpace(req.WorkspaceBase),
		WorkspaceCapture:  req.WorkspaceCapture,
		MaxTurns:          maxTurns,
		MaxSteps:          f.cfg.Policy.MaxSteps,
		Timeout:           timeout,
		HeartbeatInterval: f.cfg.Policy.HeartbeatInterval,
		Tools:             tools,
		WebSearchPolicy:   webSearchPolicy,
		InboxPath:         inboxPath,
		EventLogPath:      logPath,
		SessionPath:       sessionPath,
		inbox:             NewInbox(inboxPath),
		status:            StatusPending,
		activity:          "queued",
		transcriptLoaded:  true,
		processState:      ProcessPending,
		turnState:         TurnQueued,
		updatedAt:         now,
		lastActivity:      now,
		requirement:       newRequirement(req.Required, 1, now),
		stateDir:          stateDir,
		lease:             lease,
		maxOutputBytes:    f.cfg.Policy.MaxOutputBytes,
		maxOutputLines:    f.cfg.Policy.MaxOutputLines,
		done:              make(chan struct{}),
	}
	a.fastModeOverridesHost = fastModeOverridesHost
	if a.RootSessionID == "" {
		a.RootSessionID = sessionID
	}
	a.ctx, a.cancel = runCtx, cancel
	a.persistFn = f.persistAgent
	a.setOnTurnIdle(func() { f.dispatchQueuedResumeWithTimeout(a) })
	a.workspaceCleanup = func() error { return workspace.Cleanup(context.Background()) }
	a.workspaceCapture = func() (WorkspaceCapture, error) { return workspace.Capture(context.Background()) }
	a.trace = f.trace
	a.runner = f.cfg.NewRunner(a)
	if err := writeAgentMeta(stateDir, a); err != nil {
		if a.cancel != nil {
			a.cancel()
		}
		_ = workspace.Cleanup(context.Background())
		_ = lease.Close()
		leaseOwned = false
		_ = os.RemoveAll(stateDir)
		return nil, fmt.Errorf("subagents initial metadata: %w", err)
	}

	f.mu.Lock()
	f.agents[id] = a
	f.order = append(f.order, id)
	f.queue = append(f.queue, a)
	f.mu.Unlock()
	leaseOwned = false
	f.recordTrace(TraceEvent{Type: "agent.spawned", AgentID: a.ID, Data: map[string]any{
		"task_length": len(task), "workspace_mode": string(workspaceMode),
	}})

	// Persisted metadata was written before queue admission, so a later
	// supervisor can reconstruct work that never reached a process.
	f.armQueueTimeout(a)
	f.schedule()
	return a, nil
}

// SendUserTurn is sugar for the common "send the next user turn"
// case. It quotes nothing and forwards verbatim; callers are
// expected to have already trimmed and expanded the text.
// CancelTurn requests cancellation of the current turn while keeping the
// worker process alive for a later follow-up.
func traceErrorKind(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	default:
		return "error"
	}
}

func (f *Supervisor) recordTrace(event TraceEvent) {
	if f == nil || f.trace == nil {
		return
	}
	f.trace.Record(event)
}

// TraceDir returns the opt-in bundle directory, or an empty string when tracing
// is disabled.
func (f *Supervisor) TraceDir() string {
	if f == nil || f.trace == nil {
		return ""
	}
	return f.trace.Dir()
}

// TraceViews projects the supervisor's factual trace into the diagnostic state
// for each observed agent. It intentionally does not expose lifecycle labels.
func (f *Supervisor) TraceViews() map[string]AgentTraceView {
	if f == nil || f.trace == nil {
		return map[string]AgentTraceView{}
	}
	return f.trace.Views()
}

// Close flushes and closes the optional trace bundle. It does not stop agents;
// their lifetime remains controlled by the supervisor context and Stop.
func (f *Supervisor) Close() error { return f.CloseContext(context.Background()) }

// CloseContext flushes and closes the optional trace bundle within ctx.
func (f *Supervisor) CloseContext(ctx context.Context) error {
	if f == nil || f.trace == nil {
		return nil
	}
	return f.trace.CloseContext(ctx)
}

func (f *Supervisor) CancelTurn(id string) error {
	a := f.Get(id)
	if a == nil {
		return fmt.Errorf("subagents: no such agent %q", id)
	}
	if a.inbox == nil {
		return fmt.Errorf("subagents: agent %s has no inbox", a.ID)
	}
	a.setTurnState(TurnCanceling, a.CurrentTurnID())
	f.recordTrace(TraceEvent{Type: "turn.cancel.requested", AgentID: a.ID, TurnID: a.CurrentTurnID()})
	return a.inbox.SendCommand(NewCommand(CommandTurnCancel, a.ID, a.CurrentTurnID(), TurnCancelPayload{Reason: "user"}))
}

func (f *Supervisor) SendUserTurn(id, text string) error {
	a := f.Get(id)
	if a == nil {
		return fmt.Errorf("subagents: no such agent %q", id)
	}
	if a.inbox == nil {
		return fmt.Errorf("subagents: agent %s has no inbox", a.ID)
	}
	f.recordTrace(TraceEvent{Type: "turn.requested", AgentID: a.ID, Data: map[string]any{"prompt_length": len(text)}})
	return a.inbox.SendCommand(NewCommand(CommandTurnStart, a.ID, a.CurrentTurnID(), TurnStartPayload{Prompt: text}))
}

func (f *Supervisor) run(a *Agent) {
	a.mu.Lock()
	killedBeforeStart := a.status == StatusKilled
	ctxErr := error(nil)
	if a.ctx != nil {
		ctxErr = a.ctx.Err()
	}
	a.mu.Unlock()
	var err error
	if killedBeforeStart || ctxErr != nil || !atomic.CompareAndSwapInt32(&a.launchState, 0, 1) {
		if ctxErr != nil {
			err = ctxErr
		} else {
			err = context.Canceled
		}
	} else {
		f.recordTrace(TraceEvent{Type: "agent.started", AgentID: a.ID})
		a.setActivity("starting")
		if a.runner == nil {
			err = errors.New("subagents: agent has no runner")
		} else {
			err = a.runner.Run(a.ctx, agentSink{a: a})
		}
	}

	a.mu.Lock()
	a.finished = f.cfg.Now()
	startupErr := a.startupErr
	terminalErr := err
	switch {
	case a.status == StatusKilled:
		// Stop requested a terminal state; preserve it.
	case startupErr != nil:
		a.status = StatusFailed
		a.activity = "startup timeout: worker did not become ready"
		a.lastErr = startupErr
		terminalErr = startupErr
	case errors.Is(err, context.Canceled):
		a.status = StatusKilled
		a.activity = "cancelled"
		a.lastErr = err
	case errors.Is(err, context.DeadlineExceeded):
		a.status = StatusFailed
		a.activity = "timeout"
		a.lastErr = err
	case err != nil:
		a.status = StatusFailed
		a.activity = "error: " + truncate(err.Error(), 120)
		a.lastErr = err
	default:
		a.status = StatusDone
		a.activity = "done"
	}
	finalStatus := a.status
	a.mu.Unlock()
	terminalType := "agent.finished"
	switch {
	case finalStatus == StatusKilled:
		terminalType = "agent.cancelled"
	case terminalErr != nil:
		terminalType = "agent.failed"
	}
	f.recordTrace(TraceEvent{Type: terminalType, AgentID: a.ID, Data: map[string]any{"status": string(finalStatus), "error": traceErrorKind(terminalErr)}})

	switch finalStatus {
	case StatusKilled:
		a.setProcessState(ProcessKilled)
		a.setTurnState(TurnCanceled, a.CurrentTurnID())
	case StatusFailed:
		a.setProcessState(ProcessExited)
		a.setTurnState(TurnFailed, a.CurrentTurnID())
	default:
		a.setProcessState(ProcessExited)
		if a.TurnState() == TurnRunning || a.TurnState() == TurnIdle {
			a.setTurnState(TurnSucceeded, a.CurrentTurnID())
		}
	}

	// The process slot is no longer occupied once Runner.Run has returned.
	// Release it before potentially slow artifact capture and workspace
	// cleanup so queued work can start without waiting on git/filesystem I/O.
	f.releaseCapacity(a)

	// Capture isolated output and write the structured result before the
	// temporary workspace is removed. This ordering is the recovery
	// invariant: a user can always inspect a durable result after failure.
	f.captureWorkspace(a)
	f.ensureResult(a, finalStatus, terminalErr)
	if a.cancel != nil {
		a.cancel()
	}
	if a.inbox != nil {
		_ = a.inbox.Close()
	}
	a.setProcessPID(0)
	f.persistAgent(a)
	if a.workspaceCleanup != nil {
		_ = a.workspaceCleanup()
	}
	_ = a.releaseLease()
	a.closeDone()
}

// List returns a snapshot of every agent in creation order. The
// returned slice is a copy; callers may iterate without holding the
// subagent lock. Agent fields are read under their own mutex during
// formatting in Snapshot.
func (f *Supervisor) List() []*Agent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*Agent, 0, len(f.order))
	for _, id := range f.order {
		out = append(out, f.agents[id])
	}
	return out
}

// Get returns the agent with the given (possibly truncated) id, or
// nil. Matching is prefix-based so the user can type the first few
// characters of a long id.
func (f *Supervisor) Get(id string) *Agent {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.agents[id]; ok {
		return a
	}
	// Prefix match.
	var hits []*Agent
	for k, a := range f.agents {
		if strings.HasPrefix(k, id) {
			hits = append(hits, a)
		}
	}
	if len(hits) == 1 {
		return hits[0]
	}
	return nil
}

var errDetachedStopTimeout = errors.New("detached worker stop timeout")

const detachedStopPollInterval = 20 * time.Millisecond

// waitForDetachedWorker waits for the worker inbox to disappear, returning a
// sentinel when the grace period expires. Polling is interruptible so a
// caller's context is not made to wait for the next fixed sleep interval.
func waitForDetachedWorker(ctx context.Context, path string, timeout time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(detachedStopPollInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !inboxLive(path) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			if !inboxLive(path) {
				return nil
			}
			return errDetachedStopTimeout
		case <-ticker.C:
		}
	}
}

// Stop cancels the agent's context. The Runner should observe the
// cancellation and return promptly; the goroutine then finalises the
// agent in StatusKilled. Also closes the supervisor-side inbox handle
// so any pending send retries fail fast instead of dialing a
// socket about to be unlinked. Stop uses a background context; callers
// that need cancellation while waiting for a detached worker should use
// StopContext.
//
// Stop is a no-op for any agent that's not in a live runnable state
// — Done / Failed / Killed (already finalised). Reloaded Detached agents
// are probed first: if their worker survived the supervisor, Stop sends the
// shutdown command through the durable inbox instead of pretending no
// process exists.
func (f *Supervisor) Stop(id string) error {
	return f.stop(context.Background(), id, ShutdownOriginTargeted)
}

// StopContext is Stop with a context that can interrupt detached-worker
// shutdown and its graceful and force-stop waits. The context is not held
// under the supervisor operation lock while those waits run.
func (f *Supervisor) StopContext(ctx context.Context, id string) error {
	return f.stop(ctx, id, ShutdownOriginTargeted)
}

func (f *Supervisor) stop(ctx context.Context, id string, origin ShutdownOrigin) error {
	if ctx == nil {
		ctx = context.Background()
	}
	f.operationMu.Lock()
	a := f.Get(id)
	if a == nil {
		f.operationMu.Unlock()
		return fmt.Errorf("no such agent %q", id)
	}

	// Coordinate the status transition with schedule. Holding f.mu while
	// checking and changing a.status closes the small window where Stop could
	// observe pending, schedule could promote the same agent to running, and
	// the pending cleanup below could then finalize an agent that was already
	// admitted to the runner.
	f.mu.Lock()
	status, shutdownStarted := a.prepareShutdown(origin)
	if !shutdownStarted {
		f.mu.Unlock()
		f.operationMu.Unlock()
		return nil
	}
	if status == StatusDetached {
		path := a.InboxPath
		inbox := a.inbox
		pid := a.ProcessPIDValue()
		f.mu.Unlock()
		// The detached worker may take the full grace period to exit. Do not
		// serialize unrelated supervisor operations for that entire wait.
		f.operationMu.Unlock()
		return f.stopDetached(ctx, a, path, inbox, pid, origin)
	}
	atomic.CompareAndSwapInt32(&a.launchState, 0, 2)
	if status == StatusPending {
		for i, queued := range f.queue {
			if queued == a {
				f.queue = append(f.queue[:i], f.queue[i+1:]...)
				break
			}
		}
	}
	f.mu.Unlock()
	f.operationMu.Unlock()
	a.setTurnState(TurnCanceling, a.CurrentTurnID())

	if status == StatusPending {
		a.setProcessState(ProcessKilled)
		a.setTurnState(TurnCanceled, a.CurrentTurnID())
		f.captureWorkspace(a)
		f.ensureResult(a, StatusKilled, context.Canceled)
		if a.cancel != nil {
			a.cancel()
		}
		f.persistAgent(a)
		if a.workspaceCleanup != nil {
			_ = a.workspaceCleanup()
		}
		_ = a.releaseLease()
		a.closeDone()
		f.schedule()
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	}

	// Ask a live worker to shut down cleanly first. If the socket is not
	// ready, fall back immediately; a forceful context cancellation is
	// safer than leaving a supervisor entry running forever.
	if err := ctx.Err(); err != nil {
		if a.cancel != nil {
			a.cancel()
		}
		return err
	}
	graceful := false
	if a.inbox != nil {
		err := a.inbox.SendCommandContext(ctx, NewCommand(CommandAgentShutdown, a.ID, a.CurrentTurnID(), AgentShutdownPayload{Origin: origin.Sanitized()}))
		if err == nil {
			graceful = true
		}
	}
	if !graceful && a.cancel != nil {
		a.cancel()
	}
	if graceful {
		grace := f.cfg.Policy.CancelGracePeriod
		if grace <= 0 {
			grace = 10 * time.Second
		}
		go func() {
			timer := time.NewTimer(grace)
			defer timer.Stop()
			select {
			case <-a.done:
			case <-timer.C:
				if a.cancel != nil {
					a.cancel()
				}
				if a.inbox != nil {
					_ = a.inbox.Close()
				}
			}
		}()
	}
	if err := ctx.Err(); err != nil {
		if a.cancel != nil {
			a.cancel()
		}
		if a.inbox != nil {
			_ = a.inbox.Close()
		}
		return err
	}
	return nil
}

func (f *Supervisor) stopDetached(ctx context.Context, a *Agent, path string, inbox *Inbox, pid int, origin ShutdownOrigin) error {
	if inbox == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !inboxLive(path) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := inbox.SendCommandContext(ctx, NewCommand(CommandAgentShutdown, a.ID, a.CurrentTurnID(), AgentShutdownPayload{Origin: origin.Sanitized()})); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	a.setActivity("shutdown requested")
	grace := f.cfg.Policy.CancelGracePeriod
	if grace <= 0 {
		grace = 10 * time.Second
	}
	waitErr := waitForDetachedWorker(ctx, path, grace)
	if waitErr != nil && !errors.Is(waitErr, errDetachedStopTimeout) {
		return waitErr
	}
	if waitErr == nil {
		f.markDetachedKilled(a)
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if pid <= 0 {
		return fmt.Errorf("agent %s live worker did not stop within %s and has no recorded pid", a.ID, grace)
	}
	if killErr := forceKillProcess(pid); killErr != nil {
		return fmt.Errorf("agent %s force-stop: %w", a.ID, killErr)
	}
	if waitErr := waitForDetachedWorker(ctx, path, time.Second); waitErr != nil {
		if errors.Is(waitErr, errDetachedStopTimeout) {
			return fmt.Errorf("agent %s live worker did not stop after force-stop", a.ID)
		}
		return waitErr
	}
	f.markDetachedKilled(a)
	return nil
}

func (f *Supervisor) markDetachedKilled(a *Agent) {
	f.operationMu.Lock()
	defer f.operationMu.Unlock()
	a.mu.Lock()
	a.status = StatusKilled
	a.activity = "killed"
	a.mu.Unlock()
	a.setProcessState(ProcessKilled)
	a.setTurnState(TurnCanceled, a.CurrentTurnID())
	a.setProcessPID(0)
	f.ensureResult(a, StatusKilled, context.Canceled)
	f.persistAgent(a)
	a.closeDone()
}

// StopByRootSession stops and waits for every agent owned by rootSessionID.
// It leaves the supervisor lifetime and agents from other sessions running.
func (f *Supervisor) StopByRootSession(rootSessionID string) error {
	return f.StopByRootSessionContext(context.Background(), rootSessionID)
}

// StopByRootSessionContext is the context-aware form of StopByRootSession.
func (f *Supervisor) StopByRootSessionContext(ctx context.Context, rootSessionID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	rootSessionID = strings.TrimSpace(rootSessionID)
	if rootSessionID == "" {
		return errors.New("subagents: root session id is required")
	}
	agents := f.List()
	owned := make([]*Agent, 0, len(agents))
	for _, a := range agents {
		if a.RootSessionID == rootSessionID {
			owned = append(owned, a)
		}
	}
	return f.stopAgents(ctx, owned, ShutdownOriginSession)
}

func (f *Supervisor) stopAgents(ctx context.Context, agents []*Agent, origin ShutdownOrigin) error {
	var firstErr error
	for _, a := range agents {
		if err := f.stop(ctx, a.ID, origin); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, a := range agents {
		select {
		case <-a.done:
		case <-ctx.Done():
			if firstErr == nil {
				firstErr = ctx.Err()
			}
			return firstErr
		}
	}
	return firstErr
}

// StopAll cancels every running agent. Used on shutdown.
func (f *Supervisor) StopAll() {
	_ = f.stopAll(context.Background())
	_ = f.Close()
}

// StopAllContext is the context-aware shutdown variant. It returns the
// context error when cancellation interrupts a detached-worker wait.
func (f *Supervisor) StopAllContext(ctx context.Context) error {
	return f.stopAll(ctx)
}

func (f *Supervisor) stopAll(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	defer func() {
		if f.lifetimeCancel != nil {
			f.lifetimeCancel()
		}
	}()
	return f.stopAgents(ctx, f.List(), ShutdownOriginProcess)
}

// Remove tears down the per-agent state for a terminated agent. It
// is an error to remove an agent that's still running; call Stop
// first and wait for the status to settle. Detached agents
// (reloaded from disk) remove cleanly because they have no live
// runner racing for the same files.
//
// Shared-workspace agents never touch source files during Remove; worktree
// agents have already captured their output and removed the isolated checkout
// during finalization. Remove then deletes the agent's durable state directory
// under <root>/agents/<id>/.
func (f *Supervisor) Remove(id string) error {
	f.operationMu.Lock()
	defer f.operationMu.Unlock()
	a := f.Get(id)
	if a == nil {
		return fmt.Errorf("no such agent %q", id)
	}
	a.mu.Lock()
	st := a.status
	inboxPath := a.InboxPath
	a.mu.Unlock()
	if st == StatusRunning || st == StatusPending {
		return fmt.Errorf("agent %s still %s", a.ID, st)
	}
	if inboxLive(inboxPath) {
		return fmt.Errorf("agent %s still has a live worker; stop it first", a.ID)
	}
	select {
	case <-a.done:
	default:
		return fmt.Errorf("agent %s is still finalizing", a.ID)
	}
	lease, err := acquireAgentLease(a.stateDirectory(f.cfg.Root))
	if err != nil {
		return fmt.Errorf("agent %s state is owned by another supervisor: %w", a.ID, err)
	}
	_ = lease.Close()
	// Best-effort cleanup of the per-agent state directory
	// (meta.json, events.jsonl, session.json). Failing here would leave
	// the user with no recourse,
	// so swallow the error.
	_ = os.RemoveAll(a.stateDirectory(f.cfg.Root))
	f.mu.Lock()
	delete(f.agents, a.ID)
	for i, k := range f.order {
		if k == a.ID {
			f.order = append(f.order[:i], f.order[i+1:]...)
			break
		}
	}
	f.mu.Unlock()
	return nil
}

// Snapshot returns a read-only view of one agent. Safe for the TUI
// goroutine to call repeatedly; never blocks on the Runner.
type AgentSnapshot struct {
	ID              string
	Task            string
	Dir             string
	Status          Status
	ProcessState    ProcessState
	TurnState       TurnState
	CurrentTurnID   string
	LifetimeTurns   int
	CurrentRunTurns int
	Attempt         int
	Activity        string
	Started         time.Time
	Finished        time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
	LastActivity    time.Time
	Err             string
	Tail            string   // last few transcript lines, joined with "\n"
	Lines           []string // full transcript (already capped by Agent.appendTranscript)
	LastAssistant   string   // complete final assistant message, without role formatting
	Result          *TurnResult
	ResultRef       string
	Requirement     RequirementSnapshot
	PatchRef        string
	ChangedFiles    []string
	OutputTruncated bool

	// Model, Provider, Reasoning, and the provider connection settings
	// expose the per-agent configuration captured at Spawn time. Empty
	// values inherit the child's defaults. Subagent is the selected named
	// markdown profile. The dashboard surfaces these so the user can
	// confirm the child configuration.
	Model         string
	Provider      string
	BaseURL       string
	InsecureTLS   bool
	Reasoning     string
	FastMode      bool
	Subagent      string
	WorkspaceMode WorkspaceMode
	WorkspacePath string

	// Paths to the agent's durable state. Surface them in the
	// snapshot so the dashboard /subagents view can
	// read events.jsonl or resume the session without going back through the Agent.
	InboxPath    string
	EventLogPath string
	SessionPath  string
}

// Snapshot copies the live agent state into a value the caller can
// inspect at leisure.
func (a *Agent) Snapshot() AgentSnapshot {
	a.mu.Lock()
	tail := strings.Join(lastN(a.transcript, 6), "\n")
	lines := make([]string, len(a.transcript))
	copy(lines, a.transcript)
	errStr := ""
	if a.lastErr != nil {
		errStr = a.lastErr.Error()
	}
	status := a.status
	activity := a.activity
	lastAssistant := a.lastAssistant
	outputTruncated := a.outputTruncated || a.streamingAssistantTruncated
	finished := a.finished
	a.mu.Unlock()

	a.lifecycleMu.Lock()
	processState := a.processState
	turnState := a.turnState
	currentTurnID := a.currentTurnID
	lifetimeTurns := a.LifetimeTurns
	currentRunTurns := a.CurrentRunTurns
	attempt := a.Attempt
	lastActivity := a.lastActivity
	updatedAt := a.updatedAt
	result := cloneTurnResult(a.result)
	resultRef := a.resultRef
	requirement := a.visibleRequirementLocked()
	patchRef := a.patchRef
	changedFiles := append([]string(nil), a.changedFiles...)
	a.lifecycleMu.Unlock()
	return AgentSnapshot{
		ID: a.ID, Task: a.Task, Dir: a.Dir,
		Status: status, ProcessState: processState, TurnState: turnState,
		CurrentTurnID: currentTurnID, LifetimeTurns: lifetimeTurns, CurrentRunTurns: currentRunTurns, Attempt: attempt, Activity: activity,
		Started: a.Started, Finished: finished, CreatedAt: a.Started, UpdatedAt: updatedAt, LastActivity: lastActivity,
		Err: errStr, Tail: tail, Lines: lines,
		LastAssistant: lastAssistant, Result: result, ResultRef: resultRef,
		Requirement: requirement, PatchRef: patchRef, ChangedFiles: changedFiles, OutputTruncated: outputTruncated,
		Model: a.Model, Provider: a.Provider, BaseURL: a.BaseURL,
		InsecureTLS: a.InsecureTLS, Reasoning: a.Reasoning,
		FastMode: a.FastMode, Subagent: a.Subagent, WorkspaceMode: a.WorkspaceMode,
		WorkspacePath: a.WorkspacePath, InboxPath: a.InboxPath,
		EventLogPath: a.EventLogPath, SessionPath: a.SessionPath,
	}
}

// SnapshotAll returns the supervisor's established status view: live agents
// are scoped to the active session, while durable terminal entries remain
// discoverable for recovery callers.
func (f *Supervisor) SnapshotAll() []AgentSnapshot {
	f.mu.Lock()
	active := f.activeSession
	f.mu.Unlock()
	return f.snapshotMatching(func(a *Agent) bool {
		if active == "" || a.SessionID == "" || a.SessionID == active {
			return true
		}
		status := a.Status()
		return status != StatusRunning && status != StatusPending
	})
}

// SnapshotAllSessions returns every agent, regardless of host session. It is
// the explicit cross-session view used by the dashboard's All filter.
func (f *Supervisor) SnapshotAllSessions() []AgentSnapshot {
	return f.snapshotMatching(func(*Agent) bool { return true })
}

// SnapshotCurrentSession returns only agents owned by the active host
// session. When no active session is set, it includes every agent for
// command-line and test callers that do not establish a session boundary.
func (f *Supervisor) SnapshotCurrentSession() []AgentSnapshot {
	f.mu.Lock()
	active := f.activeSession
	f.mu.Unlock()
	return f.snapshotMatching(func(a *Agent) bool {
		return active == "" || a.SessionID == active
	})
}

func (f *Supervisor) snapshotMatching(include func(*Agent) bool) []AgentSnapshot {
	agents := f.List()
	out := make([]AgentSnapshot, 0, len(agents))
	for _, a := range agents {
		if include(a) {
			out = append(out, a.Snapshot())
		}
	}
	// Sort by start time for a stable, deterministic listing.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// agentSink is the Sink the Supervisor hands to each Runner.
type agentSink struct{ a *Agent }

func (s agentSink) Activity(msg string) { s.a.setActivity(msg) }
func (s agentSink) Transcript(chunk string) {
	if strings.HasPrefix(chunk, "stderr: ") || strings.HasPrefix(chunk, "error: ") ||
		strings.HasPrefix(chunk, "tool: ") || strings.HasPrefix(chunk, "tool result: ") {
		s.a.appendTranscript(chunk)
		return
	}
	s.a.appendAssistantMessage(chunk)
}
func (s agentSink) userMessage(text string)      { s.a.appendUserMessage(text) }
func (s agentSink) assistantMessage(text string) { s.a.appendAssistantMessage(text) }
func (s agentSink) assistantDelta(text string)   { s.a.appendAssistantDelta(text) }
func (s agentSink) resetStreamingAssistant()     { s.a.resetStreamingAssistant() }

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 3 {
		return strings.Repeat(".", n)
	}
	return string(runes[:n-3]) + "..."
}

func lastN(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}
