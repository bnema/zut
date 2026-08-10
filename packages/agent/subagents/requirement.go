package subagents

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RequirementState is the durable outcome of work that the parent declared
// mandatory before it may finish its own turn.
type RequirementState string

const (
	RequirementPending       RequirementState = "pending"
	RequirementSatisfied     RequirementState = "satisfied"
	RequirementFailed        RequirementState = "failed"
	RequirementTimedOut      RequirementState = "timed_out"
	RequirementCanceled      RequirementState = "canceled"
	RequirementIndeterminate RequirementState = "indeterminate"
)

// RequirementSnapshot is persisted with its worker. A requirement targets a
// delegated message turn rather than a process attempt because resumable
// workers may execute several turns in one process.
type RequirementSnapshot struct {
	Required   bool             `json:"required"`
	State      RequirementState `json:"state,omitempty"`
	TargetTurn int              `json:"target_turn,omitempty"`
	UpdatedAt  time.Time        `json:"updated_at,omitempty"`
	ErrorCode  string           `json:"error_code,omitempty"`
	Notified   bool             `json:"notified,omitempty"`
}

// Unmet reports whether this requirement still prevents parent completion.
func (r RequirementSnapshot) Unmet() bool {
	if !r.Required {
		return false
	}
	return r.State != RequirementSatisfied
}

func (r RequirementSnapshot) pending() bool {
	return r.Required && r.State == RequirementPending
}

func newRequirement(required bool, targetTurn int, now time.Time) RequirementSnapshot {
	if !required {
		return RequirementSnapshot{}
	}
	return RequirementSnapshot{
		Required:   true,
		State:      RequirementPending,
		TargetTurn: targetTurn,
		UpdatedAt:  now.UTC(),
	}
}

func (a *Agent) requirementSnapshot() RequirementSnapshot {
	if a == nil {
		return RequirementSnapshot{}
	}
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	return a.visibleRequirementLocked()
}

func (a *Agent) prepareRequired(targetTurn int) RequirementSnapshot {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	a.initRequirementSignalLocked()
	previous := a.requirement
	if targetTurn <= 0 {
		nextTurn := a.LifetimeTurns + 1
		if previous.pending() && previous.TargetTurn >= nextTurn {
			return previous
		}
		targetTurn = nextTurn
		if previous.TargetTurn >= targetTurn {
			targetTurn = previous.TargetTurn + 1
		}
	}
	a.requirement = RequirementSnapshot{
		Required:   true,
		State:      RequirementPending,
		TargetTurn: targetTurn,
		UpdatedAt:  time.Now().UTC(),
	}
	a.signalRequirementLocked()
	return previous
}

func (a *Agent) restoreRequirement(previous RequirementSnapshot) {
	a.lifecycleMu.Lock()
	a.initRequirementSignalLocked()
	a.requirement = previous
	a.requirementPersisting = false
	a.signalRequirementLocked()
	a.lifecycleMu.Unlock()
}

func (a *Agent) markRequirementNotified() {
	if a == nil {
		return
	}
	a.lifecycleMu.Lock()
	if !a.requirement.Required || a.requirement.Notified {
		a.lifecycleMu.Unlock()
		return
	}
	previous := a.requirement
	a.requirement.Notified = true
	a.requirement.UpdatedAt = time.Now().UTC()
	notified := a.requirement
	a.lifecycleMu.Unlock()
	if a.persistFn != nil {
		if err := a.persistFn(a); err != nil {
			a.lifecycleMu.Lock()
			if a.requirement == notified {
				a.requirement = previous
			}
			a.lifecycleMu.Unlock()
			a.recordPersistenceError(err)
			return
		}
	}
	a.lifecycleMu.Lock()
	if a.requirement == notified {
		a.signalRequirementLocked()
	}
	a.lifecycleMu.Unlock()
}

func (a *Agent) resolveRequirement(step int, result *TurnResult, errMsg string, force bool) RequirementSnapshot {
	if a == nil {
		return RequirementSnapshot{}
	}
	a.lifecycleMu.Lock()
	a.initRequirementSignalLocked()
	current := a.requirement
	if !current.pending() || (!force && step != current.TargetTurn) {
		a.lifecycleMu.Unlock()
		return current
	}
	if result != nil && strings.TrimSpace(result.TurnID) != "" && !requirementAcceptsResult(current, result) {
		result = nil
		if strings.TrimSpace(errMsg) == "" {
			errMsg = "required turn ended without a matching durable result"
		}
	}
	if result != nil && a.persistFn != nil && !unobservedRequirementResult(result) {
		durable, err := readTurnResult(a.stateDirectory(""))
		if err != nil || durable.TurnID != result.TurnID || durable.AgentID != result.AgentID {
			if err == nil {
				err = fmt.Errorf("durable result identity does not match agent %s turn %s", result.AgentID, result.TurnID)
			}
			persistenceErr := fmt.Errorf("persist required result: %w", err)
			a.requirement.State = RequirementIndeterminate
			a.requirement.ErrorCode = "required_outcome_unobserved"
			a.requirement.Notified = false
			a.requirement.UpdatedAt = time.Now().UTC()
			resolved := a.requirement
			a.requirementPersisting = true
			a.lifecycleMu.Unlock()
			a.recordPersistenceError(persistenceErr)
			if err := a.persistFn(a); err != nil {
				a.recordPersistenceError(err)
			}
			a.lifecycleMu.Lock()
			if a.requirement == resolved {
				a.requirementPersisting = false
				a.signalRequirementLocked()
			}
			a.lifecycleMu.Unlock()
			return resolved
		}
	}
	state, errorCode := classifyRequirementOutcome(result, errMsg)
	a.requirement.State = state
	a.requirement.ErrorCode = errorCode
	a.requirement.Notified = false
	a.requirement.UpdatedAt = time.Now().UTC()
	resolved := a.requirement
	if a.persistFn != nil {
		a.requirementPersisting = true
	}
	a.lifecycleMu.Unlock()
	if a.persistFn != nil {
		if err := a.persistFn(a); err != nil {
			a.lifecycleMu.Lock()
			if a.requirement == resolved {
				a.requirement = current
				a.requirementPersisting = false
			}
			a.lifecycleMu.Unlock()
			a.recordPersistenceError(err)
			return current
		}
	}
	a.lifecycleMu.Lock()
	if a.requirement == resolved {
		a.requirementPersisting = false
		a.signalRequirementLocked()
	}
	a.lifecycleMu.Unlock()
	return resolved
}

func unobservedRequirementResult(result *TurnResult) bool {
	return result != nil && result.Error != nil && result.Error.Code == "required_outcome_unobserved"
}

func requirementAcceptsResult(requirement RequirementSnapshot, result *TurnResult) bool {
	if !requirement.pending() || result == nil {
		return false
	}
	turnID := strings.TrimSpace(result.TurnID)
	if strings.HasPrefix(turnID, "turn-") {
		if turn, err := strconv.Atoi(strings.TrimPrefix(turnID, "turn-")); err == nil {
			return turn == requirement.TargetTurn
		}
	}
	// Legacy first-turn results did not always carry a turn id. They can
	// satisfy only the initial requirement; retries fail closed without proof.
	return requirement.TargetTurn == 1
}

func classifyRequirementOutcome(result *TurnResult, errMsg string) (RequirementState, string) {
	if result != nil {
		errorCode := ""
		if result.Error != nil {
			errorCode = strings.TrimSpace(result.Error.Code)
		}
		switch {
		case errorCode == "required_outcome_unobserved":
			return RequirementIndeterminate, "required_outcome_unobserved"
		case result.ShutdownOrigin == ShutdownOriginDeadline || errorCode == "deadline_exceeded" || errorCode == "timeout":
			return RequirementTimedOut, "deadline_exceeded"
		case result.Status == ResultCanceled || errorCode == "canceled" || errorCode == "cancelled":
			return RequirementCanceled, "canceled"
		case result.Status == ResultFailed:
			return RequirementFailed, "failed"
		default:
			return RequirementSatisfied, ""
		}
	}
	lower := strings.ToLower(strings.TrimSpace(errMsg))
	switch {
	case lower == "":
		return RequirementSatisfied, ""
	case strings.Contains(lower, "deadline exceeded") || strings.Contains(lower, "timed out"):
		return RequirementTimedOut, "deadline_exceeded"
	case strings.Contains(lower, "canceled") || strings.Contains(lower, "cancelled"):
		return RequirementCanceled, "canceled"
	default:
		return RequirementFailed, "failed"
	}
}

func (a *Agent) visibleRequirementLocked() RequirementSnapshot {
	requirement := a.requirement
	if a.requirementPersisting {
		requirement.State = RequirementPending
		requirement.ErrorCode = ""
		requirement.Notified = false
	}
	return requirement
}

func (a *Agent) initRequirementSignalLocked() {
	if a.requirementChanged == nil {
		a.requirementChanged = make(chan struct{})
	}
}

func (a *Agent) signalRequirementLocked() {
	a.initRequirementSignalLocked()
	close(a.requirementChanged)
	a.requirementChanged = make(chan struct{})
}

func (a *Agent) waitRequirement(ctx context.Context) (RequirementSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		a.lifecycleMu.Lock()
		a.initRequirementSignalLocked()
		requirement := a.visibleRequirementLocked()
		changed := a.requirementChanged
		a.lifecycleMu.Unlock()
		if !requirement.pending() {
			return requirement, nil
		}
		select {
		case <-ctx.Done():
			return RequirementSnapshot{}, ctx.Err()
		case <-changed:
		}
	}
}

var errRequiredAgentMissing = errors.New("subagents: required agent no longer exists")

// WaitRequired blocks on requirement state transitions, never filesystem or
// status polling, until the selected required turn reaches a terminal outcome.
func (f *Supervisor) WaitRequired(ctx context.Context, id string) (RequirementSnapshot, error) {
	a := f.Get(strings.TrimSpace(id))
	if a == nil {
		return RequirementSnapshot{}, fmt.Errorf("%w: %q", errRequiredAgentMissing, id)
	}
	requirement := a.requirementSnapshot()
	if !requirement.Required {
		return requirement, fmt.Errorf("subagents: agent %s is not required", a.ID)
	}
	return a.waitRequirement(ctx)
}

// MarkRequirementNotified records that the parent received the terminal
// outcome through a blocking manager tool or host-injected update.
func (f *Supervisor) MarkRequirementNotified(id string) error {
	a := f.Get(strings.TrimSpace(id))
	if a == nil {
		return fmt.Errorf("subagents: no such agent %q", id)
	}
	a.markRequirementNotified()
	return nil
}

// RequiredSnapshots returns required workers owned by the active host session.
// Unlike dashboard snapshots, terminal workers from other sessions are never
// included because they must not gate an unrelated parent conversation.
func (f *Supervisor) RequiredSnapshots() []AgentSnapshot {
	f.mu.Lock()
	active := f.activeSession
	agents := make([]*Agent, 0, len(f.order))
	for _, id := range f.order {
		if a := f.agents[id]; a != nil {
			agents = append(agents, a)
		}
	}
	f.mu.Unlock()

	out := make([]AgentSnapshot, 0, len(agents))
	for _, a := range agents {
		if active != "" && a.SessionID != "" && a.SessionID != active {
			continue
		}
		snapshot := a.Snapshot()
		if snapshot.Requirement.Required {
			out = append(out, snapshot)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// UnmetRequirements returns every required worker that still blocks parent
// completion in the active session.
func (f *Supervisor) UnmetRequirements() []AgentSnapshot {
	required := f.RequiredSnapshots()
	out := required[:0]
	for _, snapshot := range required {
		if snapshot.Requirement.Unmet() {
			out = append(out, snapshot)
		}
	}
	return out
}

// WaitActiveRequirements waits for all currently pending required workers in
// the active session. It returns all required snapshots so the host can inject
// both successful outcomes and unresolved failures before the next model turn.
func (f *Supervisor) WaitActiveRequirements(ctx context.Context) ([]AgentSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		pending := false
		for _, snapshot := range f.RequiredSnapshots() {
			if snapshot.Requirement.pending() {
				pending = true
				if _, err := f.WaitRequired(ctx, snapshot.ID); err != nil {
					if errors.Is(err, errRequiredAgentMissing) {
						break
					}
					return nil, err
				}
				break
			}
		}
		if !pending {
			return f.RequiredSnapshots(), nil
		}
	}
}
