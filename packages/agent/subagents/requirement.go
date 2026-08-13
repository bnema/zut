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
	Required     bool             `json:"required"`
	State        RequirementState `json:"state,omitempty"`
	TargetTurn   int              `json:"target_turn,omitempty"`
	ResultTurnID string           `json:"result_turn_id,omitempty"`
	UpdatedAt    time.Time        `json:"updated_at,omitempty"`
	ErrorCode    string           `json:"error_code,omitempty"`
	Notified     bool             `json:"notified,omitempty"`
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

func (a *Agent) markRequirementNotified() (bool, error) {
	if a == nil {
		return false, nil
	}
	a.lifecycleMu.Lock()
	if !a.requirement.Required || a.requirement.Notified {
		a.lifecycleMu.Unlock()
		return false, nil
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
			return false, err
		}
	}
	a.lifecycleMu.Lock()
	if a.requirement == notified {
		a.signalRequirementLocked()
	}
	a.lifecycleMu.Unlock()
	return true, nil
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
	if result != nil {
		a.requirement.ResultTurnID = strings.TrimSpace(result.TurnID)
	}
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

// MarkRequirementNotified records that the parent received the terminal
// outcome through a host-injected update.
func (f *Supervisor) MarkRequirementNotified(id string) error {
	a := f.Get(strings.TrimSpace(id))
	if a == nil {
		return fmt.Errorf("subagents: no such agent %q", id)
	}
	snapshot := a.Snapshot()
	if strings.TrimSpace(snapshot.ResultRef) == "" {
		return errors.New("subagents: required result is not durably available")
	}
	requirement := snapshot.Requirement
	expectedTurnID := strings.TrimSpace(requirement.ResultTurnID)
	if expectedTurnID == "" && requirement.TargetTurn > 0 {
		expectedTurnID = fmt.Sprintf("turn-%d", requirement.TargetTurn)
	}
	result := snapshot.Result
	if result == nil || strings.TrimSpace(result.TurnID) != expectedTurnID {
		return fmt.Errorf("subagents: durable required result does not match %s", expectedTurnID)
	}
	marked, err := a.markRequirementNotified()
	if err != nil {
		a.recordTrace(TraceEvent{Type: "result.delivery.failed", TurnID: expectedTurnID, Data: map[string]any{"ref": ResultRef(a.ID)}})
		return fmt.Errorf("subagents: persist required result delivery: %w", err)
	}
	if marked {
		a.recordTrace(TraceEvent{Type: "result.delivered", TurnID: expectedTurnID, Data: map[string]any{"ref": ResultRef(a.ID)}})
	}
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

// UnmetRequirements returns every required worker that still blocks the
// parent's terminal response in the active session.
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
