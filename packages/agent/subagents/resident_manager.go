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
	"time"

	"github.com/google/uuid"
)

// ResidentFactory is the host-owned construction boundary. It receives only
// an already-resolved non-secret specification and returns an in-process turn
// runner; it never starts another zut binary.
type ResidentFactory func(ResidentChildSpec, *ResidentJournal) (ResidentTurnRunner, error)

type ResidentManager struct {
	root             string
	factory          ResidentFactory
	prepareWorkspace func(context.Context, WorkspaceRequest) (WorkspaceHandle, error)
	mu               sync.Mutex
	lifecycleMu      sync.Mutex
	closed           bool
	children         map[string]*ResidentChild
	recovered        map[string]ResidentSnapshot
	recoveredSpec    map[string]ResidentChildSpec
	pending          map[string]struct{}
	scheduler        *ResidentScheduler
	queueTimeout     time.Duration
	allowedRoots     []string
	dispatchMu       sync.Mutex
	onCompletion     func(ResidentCompletion)
	onAccepted       func(ResidentChildSpec, string)
}

func (m *ResidentManager) SetCompletionObserver(observer func(ResidentCompletion)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.onCompletion = observer
	m.mu.Unlock()
}

// SetAcceptedObserver receives a prompt only after the authoritative
// child.accepted record has synchronized, and before it can be scheduled.
func (m *ResidentManager) SetAcceptedObserver(observer func(ResidentChildSpec, string)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.onAccepted = observer
	m.mu.Unlock()
}

// ResidentSnapshot is the manager's bounded public state projection. It
// deliberately omits prompts, provider credentials, and filesystem paths.
type ResidentSnapshot struct {
	ID            string
	State         ResidentState
	Profile       string
	Provider      string
	Model         string
	WorkspaceMode WorkspaceMode
	Required      bool
}

func NewResidentManager(root string, factory ResidentFactory) *ResidentManager {
	return NewResidentManagerWithLimit(root, 0, factory)
}

func NewResidentManagerWithLimit(root string, limit int, factory ResidentFactory) *ResidentManager {
	return NewResidentManagerWithWorkspace(root, limit, PrepareWorkspace, factory)
}

// NewResidentManagerWithPolicy applies the resident-only host policy. A zero
// queue timeout leaves accepted work queued until a scheduler slot is free.
func NewResidentManagerWithPolicy(root string, policy SubagentPolicy, factory ResidentFactory) *ResidentManager {
	return newResidentManager(root, policy, PrepareWorkspace, factory)
}

// NewResidentManagerWithWorkspace exposes the narrow allocation seam needed
// to make workspace preparation part of spawn's durable commit boundary.
func NewResidentManagerWithWorkspace(root string, limit int, prepare func(context.Context, WorkspaceRequest) (WorkspaceHandle, error), factory ResidentFactory) *ResidentManager {
	return newResidentManager(root, SubagentPolicy{MaxConcurrent: limit}, prepare, factory)
}

func newResidentManager(root string, policy SubagentPolicy, prepare func(context.Context, WorkspaceRequest) (WorkspaceHandle, error), factory ResidentFactory) *ResidentManager {
	if prepare == nil {
		prepare = PrepareWorkspace
	}
	return &ResidentManager{
		root:             root,
		factory:          factory,
		prepareWorkspace: prepare,
		children:         make(map[string]*ResidentChild),
		recovered:        make(map[string]ResidentSnapshot),
		recoveredSpec:    make(map[string]ResidentChildSpec),
		pending:          make(map[string]struct{}),
		scheduler:        NewResidentScheduler(policy.MaxConcurrent),
		queueTimeout:     policy.QueueTimeout,
		allowedRoots:     append([]string(nil), policy.AllowedRoots...),
	}
}

// Spawn commits acceptance before publishing a child or starting its runner.
func (m *ResidentManager) Spawn(ctx context.Context, spec ResidentChildSpec, task string) (*ResidentChild, error) {
	if m == nil || m.factory == nil {
		return nil, errors.New("resident manager: no factory")
	}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("resident manager: closed")
	}
	if _, exists := m.children[spec.ID]; exists {
		m.mu.Unlock()
		return nil, errors.New("resident manager: duplicate child ID")
	}
	if _, exists := m.recovered[spec.ID]; exists {
		m.mu.Unlock()
		return nil, errors.New("resident manager: child journal already exists")
	}
	if _, exists := m.pending[spec.ID]; exists {
		m.mu.Unlock()
		return nil, errors.New("resident manager: child acceptance pending")
	}
	m.pending[spec.ID] = struct{}{}
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.pending, spec.ID); m.mu.Unlock() }()
	if spec.InitialTurnID == "" {
		spec.InitialTurnID = uuid.NewString()
	}
	journal, err := OpenResidentJournal(m.root, spec.ID)
	if err != nil {
		return nil, err
	}
	workspaceRoot := strings.TrimSpace(spec.RepositoryRoot)
	if workspaceRoot == "" {
		workspaceRoot = strings.TrimSpace(spec.Workspace)
	}
	if !m.workspaceAllowed(workspaceRoot) {
		_ = journal.Close()
		return nil, errors.New("resident manager: workspace is outside allowed roots")
	}
	var workspace WorkspaceHandle
	if workspaceRoot != "" || spec.WorkspaceMode == WorkspaceWorktree {
		workspace, err = m.prepareWorkspace(ctx, WorkspaceRequest{
			Mode: spec.WorkspaceMode, RepositoryRoot: workspaceRoot, StateDir: filepath.Join(m.root, spec.ID),
			AgentID: spec.ID, Base: spec.WorkspaceBase, Capture: spec.WorkspaceCapture, AllowedRoots: m.allowedRoots,
		})
		if err != nil {
			_ = journal.Close()
			return nil, fmt.Errorf("resident manager prepare workspace: %w", err)
		}
		spec.Workspace = workspace.Dir()
		spec.WorkspaceMode = workspace.Mode()
	}
	if err := journal.Accept(spec, task); err != nil {
		if workspace != nil {
			_ = workspace.Cleanup(context.Background())
		}
		_ = journal.Close()
		return nil, err
	}
	m.mu.Lock()
	accepted := m.onAccepted
	m.mu.Unlock()
	if accepted != nil {
		accepted(spec, task)
	}
	runner, err := m.factory(spec, journal)
	if err != nil {
		_ = journal.RecordFailure(spec)
		_ = journal.Close()
		m.mu.Lock()
		completion := m.onCompletion
		m.mu.Unlock()
		if completion != nil {
			completion(ResidentCompletion{ChildID: spec.ID, Task: task, Err: err})
		}
		return nil, err
	}
	m.mu.Lock()
	observer := m.onCompletion
	m.mu.Unlock()
	child := newJournaledResidentChildWithWorkspace(spec, journal, workspace, m.scheduledRunner(spec.ID, runner), observer)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = child.Close(context.Background())
		return nil, errors.New("resident manager: closed")
	}
	m.children[spec.ID] = child
	m.mu.Unlock()
	if err := child.resumeAccepted(ctx, spec.InitialTurnID, task); err != nil {
		_ = journal.RecordTurnInterrupted(spec, spec.InitialTurnID)
		return nil, err
	}
	return child, nil
}

func (m *ResidentManager) scheduledRunner(childID string, runner ResidentTurnRunner) ResidentTurnRunner {
	return func(ctx context.Context, prompt string) error {
		if ctx == nil {
			ctx = context.Background()
		}
		ticket := m.scheduler.Enqueue(childID, prompt)
		m.dispatch()
		var timeout <-chan time.Time
		var timer *time.Timer
		if m.queueTimeout > 0 {
			timer = time.NewTimer(m.queueTimeout)
			timeout = timer.C
			defer timer.Stop()
		}
		select {
		case <-ctx.Done():
			if m.scheduler.Cancel(ticket.Sequence) {
				m.dispatch()
				return ctx.Err()
			}
			// Admission won the race with cancellation. Consume the handoff
			// and release its reservation without running provider work.
			<-ticket.ready
			_ = m.scheduler.Release(childID)
			m.dispatch()
			return ctx.Err()
		case <-ticket.ready:
		case <-timeout:
			if m.scheduler.Cancel(ticket.Sequence) {
				m.dispatch()
				return fmt.Errorf("resident manager: queue timeout after %s", m.queueTimeout)
			}
			// Admission won the race with the timeout. The reservation is now
			// owned by this runner and must be released below.
			<-ticket.ready
		}
		defer func() {
			_ = m.scheduler.Release(childID)
			m.dispatch()
		}()
		return runner(ctx, prompt)
	}
}

func (m *ResidentManager) dispatch() {
	if m == nil || m.scheduler == nil {
		return
	}
	m.dispatchMu.Lock()
	defer m.dispatchMu.Unlock()
	for {
		if _, ok := m.scheduler.Admit(); !ok {
			return
		}
	}
}

// Resume synchronizes a follow-up acceptance before making it visible to the
// resident child. A disk-only child is rebuilt only for this explicit new
// prompt; reconciliation itself never constructs or replays one.
func (m *ResidentManager) Resume(ctx context.Context, childID, prompt string) error {
	if m == nil {
		return errors.New("resident manager: unavailable")
	}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	childID = strings.TrimSpace(childID)
	prompt = strings.TrimSpace(prompt)
	if childID == "" || prompt == "" {
		return errors.New("resident manager: child ID and prompt are required")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("resident manager: closed")
	}
	child := m.children[childID]
	spec, recovered := m.recoveredSpec[childID]
	if child == nil && recovered {
		if _, pending := m.pending[childID]; pending {
			m.mu.Unlock()
			return errors.New("resident manager: child resume pending")
		}
		m.pending[childID] = struct{}{}
	}
	m.mu.Unlock()
	if child == nil && recovered {
		defer func() { m.mu.Lock(); delete(m.pending, childID); m.mu.Unlock() }()
		journal, err := OpenResidentJournal(m.root, childID)
		if err != nil {
			return err
		}
		var workspace WorkspaceHandle
		workspaceRoot := strings.TrimSpace(spec.RepositoryRoot)
		if workspaceRoot == "" {
			workspaceRoot = strings.TrimSpace(spec.Workspace)
		}
		if !m.workspaceAllowed(workspaceRoot) {
			_ = journal.Close()
			return errors.New("resident manager: workspace is outside allowed roots")
		}
		if workspaceRoot != "" || spec.WorkspaceMode == WorkspaceWorktree {
			workspace, err = m.prepareWorkspace(ctx, WorkspaceRequest{
				Mode: spec.WorkspaceMode, RepositoryRoot: workspaceRoot, StateDir: filepath.Join(m.root, childID),
				AgentID: childID, Base: spec.WorkspaceBase, Capture: spec.WorkspaceCapture, AllowedRoots: m.allowedRoots, ExistingPath: spec.Workspace,
			})
			if err != nil {
				_ = journal.Close()
				return fmt.Errorf("resident manager rebuild workspace: %w", err)
			}
			spec.Workspace = workspace.Dir()
			spec.WorkspaceMode = workspace.Mode()
		}
		runner, err := m.factory(spec, journal)
		if err != nil {
			if workspace != nil && workspace.Mode() == WorkspaceShared {
				_ = workspace.Cleanup(context.Background())
			}
			_ = journal.Close()
			return fmt.Errorf("resident manager rebuild child: %w", err)
		}
		m.mu.Lock()
		observer := m.onCompletion
		m.mu.Unlock()
		child = newJournaledResidentChildWithWorkspace(spec, journal, workspace, m.scheduledRunner(spec.ID, runner), observer)
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			_ = child.Close(context.Background())
			return errors.New("resident manager: closed")
		}
		m.children[childID] = child
		delete(m.recovered, childID)
		delete(m.recoveredSpec, childID)
		m.mu.Unlock()
	}
	if child == nil {
		return errors.New("resident manager: child is not live")
	}
	turnID := uuid.NewString()
	if child.journal == nil {
		return child.resumeAccepted(ctx, turnID, prompt)
	}
	if err := child.journal.AcceptFollowUp(child.spec, turnID, prompt); err != nil {
		return err
	}
	m.mu.Lock()
	accepted := m.onAccepted
	m.mu.Unlock()
	if accepted != nil {
		accepted(child.spec, prompt)
	}
	if err := child.resumeAccepted(ctx, turnID, prompt); err != nil {
		_ = child.journal.RecordTurnInterrupted(child.spec, turnID)
		return err
	}
	return nil
}

func (m *ResidentManager) workspaceAllowed(root string) bool {
	if len(m.allowedRoots) == 0 || strings.TrimSpace(root) == "" {
		return true
	}
	for _, allowed := range m.allowedRoots {
		if pathWithin(root, allowed) {
			return true
		}
	}
	return false
}

// Get returns the live child only. Durable children recovered from a previous
// host are intentionally not auto-resumed.
func (m *ResidentManager) Get(childID string) *ResidentChild {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.children[childID]
}

// Live returns an immutable unfinished-turn snapshot for one live child. It
// never holds the manager lock while copying the child projection.
func (m *ResidentManager) Live(childID string) (ResidentLiveSnapshot, bool) {
	if m == nil {
		return ResidentLiveSnapshot{}, false
	}
	m.mu.Lock()
	child := m.children[childID]
	m.mu.Unlock()
	if child == nil {
		return ResidentLiveSnapshot{}, false
	}
	return child.Live(), true
}

// Snapshot returns copies of the live children without holding the manager
// lock while callers render or perform I/O.
func (m *ResidentManager) Snapshot() []ResidentSnapshot {
	children, _ := m.SnapshotPage(0, -1)
	return children
}

// SnapshotPage returns a bounded, ID-sorted state projection. It avoids
// materializing live snapshots that a renderer cannot display and reports the
// total so callers can page without relying on a stale full list.
func (m *ResidentManager) SnapshotPage(offset, limit int) ([]ResidentSnapshot, int) {
	if m == nil {
		return nil, 0
	}
	if offset < 0 {
		offset = 0
	}
	m.mu.Lock()
	live := make(map[string]*ResidentChild, len(m.children))
	for id, child := range m.children {
		live[id] = child
	}
	recovered := make(map[string]ResidentSnapshot, len(m.recovered))
	for id, snapshot := range m.recovered {
		recovered[id] = snapshot
	}
	m.mu.Unlock()
	ids := make([]string, 0, len(live)+len(recovered))
	for id := range recovered {
		ids = append(ids, id)
	}
	for id := range live {
		if _, alreadyRecovered := recovered[id]; !alreadyRecovered {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	total := len(ids)
	if offset >= total || limit == 0 {
		return nil, total
	}
	end := total
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	result := make([]ResidentSnapshot, 0, end-offset)
	for _, id := range ids[offset:end] {
		if child := live[id]; child != nil {
			result = append(result, ResidentSnapshot{
				ID: child.spec.ID, State: child.State(), Profile: child.spec.Profile,
				Provider: child.spec.Provider, Model: child.spec.Model,
				WorkspaceMode: child.spec.WorkspaceMode, Required: child.spec.Required,
			})
			continue
		}
		if snapshot, ok := recovered[id]; ok {
			result = append(result, snapshot)
		}
	}
	return result, total
}

// SnapshotFor returns metadata for one child without building a dashboard
// projection. It keeps child-session headers independent of the total number
// of resident children.
func (m *ResidentManager) SnapshotFor(childID string) (ResidentSnapshot, bool) {
	if m == nil {
		return ResidentSnapshot{}, false
	}
	m.mu.Lock()
	child := m.children[childID]
	recovered, ok := m.recovered[childID]
	m.mu.Unlock()
	if child != nil {
		return ResidentSnapshot{ID: child.spec.ID, State: child.State(), Profile: child.spec.Profile,
			Provider: child.spec.Provider, Model: child.spec.Model,
			WorkspaceMode: child.spec.WorkspaceMode, Required: child.spec.Required}, true
	}
	return recovered, ok
}

// Reconcile discovers journals left by an earlier host, repairs their bounded
// projections, and marks queued/running work interrupted. It never creates a
// live child or replays a prompt. Per-child errors are returned for callers to
// surface without hiding valid sibling journals.
func (m *ResidentManager) Reconcile() []error {
	if m == nil {
		return []error{errors.New("resident manager: unavailable")}
	}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	entries, err := os.ReadDir(m.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return []error{fmt.Errorf("resident manager read root: %w", err)}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	m.mu.Lock()
	m.recovered = make(map[string]ResidentSnapshot)
	m.recoveredSpec = make(map[string]ResidentChildSpec)
	m.mu.Unlock()
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// The old subprocess runtime stored children beneath an "agents"
		// container. Resident children are direct entries in this root. Leave
		// that legacy state untouched: it is neither read nor migrated.
		if entry.Name() == "agents" {
			continue
		}
		childID := entry.Name()
		m.mu.Lock()
		_, live := m.children[childID]
		_, pending := m.pending[childID]
		m.mu.Unlock()
		if live || pending {
			continue
		}
		metadata, reconcileErr := ReconcileResidentJournal(filepath.Join(m.root, childID))
		if reconcileErr != nil {
			errs = append(errs, fmt.Errorf("resident child %s: %w", childID, reconcileErr))
			continue
		}
		records, readErr := ReadResidentJournal(filepath.Join(m.root, childID, residentTranscriptName))
		if readErr != nil || len(records) == 0 || records[0].Spec == nil {
			errs = append(errs, fmt.Errorf("resident child %s: invalid accepted specification", childID))
			continue
		}
		spec := records[0].Spec
		m.mu.Lock()
		if _, live := m.children[childID]; !live {
			m.recovered[childID] = ResidentSnapshot{ID: childID, State: metadata.State, Profile: spec.Profile, Provider: spec.Provider, Model: spec.Model, WorkspaceMode: spec.WorkspaceMode, Required: spec.Required}
			m.recoveredSpec[childID] = *spec
		}
		m.mu.Unlock()
	}
	return errs
}

// UnmetRequired returns required work that has not reached the successful idle
// boundary. Failed and interrupted work intentionally remain unmet until an
// explicit successful follow-up is accepted and completed.
func (m *ResidentManager) UnmetRequired() []ResidentSnapshot {
	all := m.Snapshot()
	result := all[:0]
	for _, snapshot := range all {
		if snapshot.Required && snapshot.State != ResidentIdle {
			result = append(result, snapshot)
		}
	}
	return result
}

// Stop cancels one live child and waits for its control loop. A disk-only
// interrupted child must be explicitly resumed by host construction instead.
func (m *ResidentManager) Stop(ctx context.Context, childID string) error {
	child := m.Get(strings.TrimSpace(childID))
	if child == nil {
		return errors.New("resident manager: child is not live")
	}
	return child.Close(ctx)
}

// History reads bounded finalized resident history through the manager-owned
// state root. It accepts only a child ID, never a filesystem path.
func (m *ResidentManager) History(childID string, limit int) ([]ResidentHistoryItem, error) {
	if m == nil {
		return nil, errors.New("resident manager: unavailable")
	}
	dir, err := ResidentHistoryDir(m.root, childID)
	if err != nil {
		return nil, err
	}
	return ReadResidentHistory(dir, limit)
}

// HistoryPage reads one recent-first, bounded page through the manager-owned
// state root. Public consumers identify only a child and receive an opaque
// cursor for older content.
func (m *ResidentManager) HistoryPage(childID, olderCursor string, limit int) (ResidentHistoryPage, error) {
	if m == nil {
		return ResidentHistoryPage{}, errors.New("resident manager: unavailable")
	}
	dir, err := ResidentHistoryDir(m.root, childID)
	if err != nil {
		return ResidentHistoryPage{}, err
	}
	return ReadResidentHistoryPage(dir, olderCursor, limit)
}

// Result returns the bounded latest-turn projection for a resident child.
func (m *ResidentManager) Result(childID string) (ResidentResult, error) {
	if m == nil {
		return ResidentResult{}, errors.New("resident manager: unavailable")
	}
	dir, err := ResidentHistoryDir(m.root, childID)
	if err != nil {
		return ResidentResult{}, err
	}
	return ReadResidentResult(filepath.Join(dir, residentResultName))
}

func (m *ResidentManager) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	m.closed = true
	children := make([]*ResidentChild, 0, len(m.children))
	for _, child := range m.children {
		children = append(children, child)
	}
	m.mu.Unlock()
	for _, child := range children {
		if err := child.Close(ctx); err != nil {
			return err
		}
	}
	return nil
}
