package subagents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

var errStartupTimeout = errors.New("subagents: worker startup timeout")

func (f *Supervisor) resolveWebSearchPolicy(req SpawnRequest) WebSearchPolicy {
	// Inherit is valid only as the request's unresolved value. Unknown values
	// are not another way to inherit the parent's capability.
	switch req.WebSearchPolicy {
	case WebSearchInherit, WebSearchDeny, WebSearchAllow:
	default:
		return WebSearchDeny
	}

	f.mu.Lock()
	parent := f.cfg.WebSearchPolicy
	allowed := f.cfg.Policy.allowedTool("web_search")
	f.mu.Unlock()

	// A child can never exceed its parent capability or the supervisor policy.
	if parent != WebSearchAllow || !allowed || req.WebSearchPolicy == WebSearchDeny {
		return WebSearchDeny
	}
	if req.Subagent != "" {
		// Named profiles need an additional explicit opt-in. Passing an allow
		// value in SpawnRequest cannot substitute for the profile's tools list.
		return NamedWebSearchPolicy(req.Tools)
	}
	return WebSearchAllow
}

func (f *Supervisor) validateSpawnRequest(req SpawnRequest) error {
	if err := f.validateSpawnOptions(req); err != nil {
		return err
	}
	mode := req.WorkspaceMode
	if mode == "" {
		mode = WorkspaceShared
	}
	if mode != WorkspaceShared && mode != WorkspaceWorktree {
		return fmt.Errorf("subagents: unknown workspace mode %q", mode)
	}
	if req.WorkspaceCapture != "" && req.WorkspaceCapture != CapturePatch && req.WorkspaceCapture != CaptureDiff {
		return fmt.Errorf("subagents: unknown workspace capture mode %q", req.WorkspaceCapture)
	}

	f.mu.Lock()
	root := f.cfg.RepoRoot
	allowedRoots := append([]string(nil), f.cfg.Policy.AllowedRoots...)
	f.mu.Unlock()
	return validateWorkspaceRoot(root, mode, allowedRoots)
}

func (f *Supervisor) validateSpawnOptions(req SpawnRequest) error {
	p := f.cfg.Policy
	if req.MaxTurns < 0 || req.MaxTurns > p.MaxTurns {
		return fmt.Errorf("subagents: max_turns must be 0 through %d (zero uses the policy default)", p.MaxTurns)
	}
	if req.Timeout < 0 {
		return errorsInvalid("timeout must not be negative")
	}
	for _, tool := range req.Tools {
		name := strings.TrimSpace(tool)
		if name == "" {
			continue
		}
		if !p.allowedTool(name) {
			return fmt.Errorf("subagents: tool %q is not allowed by policy", name)
		}
	}
	return nil
}

func validateWorkspaceRoot(root string, mode WorkspaceMode, allowedRoots []string) error {
	if root == "" {
		return errorsInvalid("repository root is empty")
	}
	if len(allowedRoots) > 0 {
		ok := false
		for _, allowed := range allowedRoots {
			if pathWithin(root, allowed) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("subagents: repository root is outside allowed roots")
		}
	}
	if mode == WorkspaceWorktree {
		if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
			// Worktrees can be attached to repositories with a .git file or
			// a parent checkout. Let the workspace adapter provide the
			// detailed Git error, but fail obvious non-checkouts here.
			if _, statErr := os.Stat(filepath.Join(root, "HEAD")); statErr != nil {
				return fmt.Errorf("subagents: worktree mode requires a Git checkout: %w", err)
			}
		}
	}
	return nil
}

func errorsInvalid(message string) error { return fmt.Errorf("subagents: %s", message) }

func pathWithin(path, root string) bool {
	pathAbs, err := containmentPath(path)
	if err != nil {
		return false
	}
	rootAbs, err := containmentPath(root)
	if err != nil {
		return false
	}
	pathAbs = filepath.Clean(pathAbs)
	rootAbs = filepath.Clean(rootAbs)
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// containmentPath returns a canonical path even when the final path does not
// exist yet. EvalSymlinks cannot resolve a missing event or session file, so
// resolve the nearest existing ancestor and append the missing components.
// Without this, a symlinked temporary directory can make a valid child path
// look outside its state directory on macOS and Windows.
func containmentPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)

	current := absolute
	var missing []string
	for {
		if _, statErr := os.Lstat(current); statErr == nil {
			evaluated, evalErr := filepath.EvalSymlinks(current)
			if evalErr != nil {
				return "", evalErr
			}
			for i := len(missing) - 1; i >= 0; i-- {
				evaluated = filepath.Join(evaluated, missing[i])
			}
			return filepath.Clean(evaluated), nil
		} else if !os.IsNotExist(statErr) {
			return "", statErr
		}

		parent := filepath.Dir(current)
		if parent == current {
			return absolute, nil
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func (f *Supervisor) schedule() {
	var starts []*Agent
	f.mu.Lock()
	for len(f.queue) > 0 && f.active < f.cfg.Policy.MaxConcurrent {
		idx := f.nextRunnableIndexLocked()
		if idx < 0 {
			break
		}
		a := f.queue[idx]
		f.queue = append(f.queue[:idx], f.queue[idx+1:]...)

		a.mu.Lock()
		if a.status != StatusPending {
			a.mu.Unlock()
			continue
		}
		a.status = StatusRunning
		a.activity = "starting"
		a.mu.Unlock()
		a.setProcessState(ProcessStarting)
		prompt, _ := a.ResumePromptInfo()
		initialTurn := (!a.Resuming && strings.TrimSpace(a.Task) != "") || prompt != ""
		if initialTurn {
			a.setTurnState(TurnQueued, "")
		} else {
			a.setTurnState(TurnIdle, "")
		}
		a.incrementAttempt()

		f.active++
		key := parentKey(a)
		f.activeByParent[key]++
		if a.BatchID != "" {
			f.activeByBatch[a.BatchID]++
		}
		starts = append(starts, a)
	}
	f.mu.Unlock()

	for _, a := range starts {
		f.persistAgent(a)
		f.startLifecycleMonitor(a)
		f.armStartupTimeout(a)
		go f.run(a)
	}
}

func (f *Supervisor) startLifecycleMonitor(a *Agent) {
	if a == nil || f.cfg.Policy.IdleTimeout <= 0 {
		return
	}
	interval := f.cfg.Policy.HeartbeatInterval / 2
	if interval <= 0 || interval > time.Minute {
		interval = time.Minute
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		heartbeatGrace := f.cfg.Policy.HeartbeatInterval + f.cfg.Policy.ReconnectTimeout
		for {
			select {
			case <-a.done:
				return
			case <-ticker.C:
				if a.ProcessState() != ProcessAlive {
					continue
				}
				last := a.LastActivity()
				if !last.IsZero() && heartbeatGrace > 0 && time.Since(last) >= heartbeatGrace {
					if err := f.Ping(a.ID); err != nil {
						a.setProcessState(ProcessDetached)
						a.setActivity("detached")
						f.persistAgent(a)
						continue
					}
				}
				if a.TurnState() == TurnIdle && !last.IsZero() && time.Since(last) >= f.cfg.Policy.IdleTimeout {
					_ = f.Stop(a.ID)
					return
				}
			}
		}
	}()
}

// Ping asks a live worker for a heartbeat. A detached or terminal worker
// returns the same not-ready error as other inbox operations.
func (f *Supervisor) Ping(id string) error {
	a := f.Get(id)
	if a == nil {
		return fmt.Errorf("subagents: no such agent %q", id)
	}
	if a.inbox == nil {
		return fmt.Errorf("subagents: agent %s has no inbox", a.ID)
	}
	return a.inbox.SendCommand(NewCommand(CommandAgentPing, a.ID, a.CurrentTurnID(), AgentPingPayload{}))
}

func (f *Supervisor) nextRunnableIndexLocked() int {
	if f.active >= f.cfg.Policy.MaxConcurrent {
		return -1
	}
	for i, a := range f.queue {
		if a == nil {
			continue
		}
		a.mu.Lock()
		status := a.status
		a.mu.Unlock()
		if status != StatusPending {
			continue
		}
		if f.cfg.Policy.MaxConcurrentPerParent > 0 && f.activeByParent[parentKey(a)] >= f.cfg.Policy.MaxConcurrentPerParent {
			continue
		}
		if a.BatchID != "" {
			if batch := f.batches[a.BatchID]; batch != nil && batch.MaxConcurrent > 0 && f.activeByBatch[a.BatchID] >= batch.MaxConcurrent {
				continue
			}
		}
		return i
	}
	return -1
}

func parentKey(a *Agent) string {
	if a == nil || strings.TrimSpace(a.ParentID) == "" {
		return "supervisor"
	}
	return a.ParentID
}

func (f *Supervisor) armQueueTimeout(a *Agent) {
	if a == nil {
		return
	}
	if d := f.cfg.Policy.QueueTimeout; d > 0 {
		time.AfterFunc(d, func() { f.expireQueued(a.ID) })
	}
	go func() {
		<-a.ctx.Done()
		f.cancelQueued(a.ID, a.ctx.Err())
	}()
}

func (f *Supervisor) expireQueued(id string) {
	f.cancelQueued(id, fmt.Errorf("subagents: queue timeout after %s waiting for an execution slot", f.cfg.Policy.QueueTimeout))
}

func (f *Supervisor) armStartupTimeout(a *Agent) {
	if a == nil {
		return
	}
	d := f.cfg.Policy.StartupTimeout
	if d <= 0 {
		return
	}
	go func() {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-a.done:
			return
		case <-timer.C:
			f.expireStarting(a, d)
		}
	}()
}

func (f *Supervisor) expireStarting(a *Agent, d time.Duration) {
	if a == nil || a.ProcessState() != ProcessStarting {
		return
	}
	cause := fmt.Errorf("%w after %s waiting for agent_ready; inspect the worker output and retry", errStartupTimeout, d)
	a.mu.Lock()
	if a.status != StatusRunning {
		a.mu.Unlock()
		return
	}
	a.startupErr = cause
	a.lastErr = cause
	a.activity = "startup timeout: worker did not become ready"
	a.mu.Unlock()
	f.persistAgent(a)
	if a.cancel != nil {
		a.cancel()
	}
}

func (f *Supervisor) cancelQueued(id string, cause error) {
	a := f.Get(id)
	if a == nil {
		return
	}

	// schedule and Stop use f.mu as the admission lock. Make queued
	// cancellation take the same lock while it changes status and removes the
	// entry, so a timeout/cancel cannot finalize an agent concurrently with
	// scheduler admission.
	f.mu.Lock()
	a.mu.Lock()
	if a.status != StatusPending {
		a.mu.Unlock()
		f.mu.Unlock()
		return
	}
	a.status = StatusFailed
	atomic.CompareAndSwapInt32(&a.launchState, 0, 2)
	if errors.Is(cause, context.Canceled) {
		a.activity = "cancelled while queued"
	} else {
		a.activity = "queue timeout: no execution slot became available"
	}
	a.lastErr = cause
	a.finished = f.cfg.Now()
	for i, queued := range f.queue {
		if queued == a {
			f.queue = append(f.queue[:i], f.queue[i+1:]...)
			break
		}
	}
	a.mu.Unlock()
	f.mu.Unlock()
	a.setProcessState(ProcessExited)
	if cause == context.Canceled {
		a.setTurnState(TurnCanceled, "")
	} else {
		a.setTurnState(TurnFailed, "")
	}
	f.captureWorkspace(a)
	f.ensureResult(a, StatusFailed, cause)
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
}

func (f *Supervisor) releaseCapacity(a *Agent) {
	f.mu.Lock()
	if f.active > 0 {
		f.active--
	}
	key := parentKey(a)
	if f.activeByParent[key] > 0 {
		f.activeByParent[key]--
	}
	if a != nil && a.BatchID != "" && f.activeByBatch[a.BatchID] > 0 {
		f.activeByBatch[a.BatchID]--
	}
	f.mu.Unlock()
	f.schedule()
}

func (f *Supervisor) persistAgent(a *Agent) error {
	if a == nil {
		return errors.New("subagents: nil agent")
	}
	stateDir := a.stateDirectory(f.cfg.Root)
	if err := writeAgentMeta(stateDir, a); err != nil {
		// Lifecycle callbacks cannot all return an error to their caller. Keep
		// the failure visible in the agent snapshot instead of silently
		// allowing a queued prompt or counter to disappear on restart.
		a.recordPersistenceError(err)
		return err
	}
	return nil
}
