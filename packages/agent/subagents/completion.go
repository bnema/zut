package subagents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Completion is one resident child's terminal turn outcome.
type Completion struct {
	AgentID string
	TurnID  string
	Status  string
	Task    string
	Error   string
	Summary string
	// Undelivered lists follow-ups steered into the turn that the child never
	// read because the turn ended first.
	Undelivered []string
}

// CompletionDropped is the status of a queued follow-up that was discarded
// before the child ever started it.
const CompletionDropped = "dropped"

// maxUndeliveredPreview bounds each undelivered follow-up in a parent update.
const maxUndeliveredPreview = 120

// Completion projects a typed resident outcome into the parent notification
// shared by interactive and orchestrated modes.
func (c ResidentCompletion) Completion() Completion {
	result := Completion{AgentID: c.ChildID, TurnID: c.TurnID, Status: string(ResidentCompleted), Task: c.Task, Summary: c.Summary, Undelivered: append([]string(nil), c.Undelivered...)}
	if c.NotStarted {
		result.Status = CompletionDropped
		return result
	}
	if c.Err != nil {
		result.Status, result.Error = string(ResidentFailed), c.Err.Error()
		if errors.Is(c.Err, context.Canceled) {
			result.Status = string(ResidentInterrupted)
		}
	}
	return result
}

// CompletionTracker coordinates resident child completions with the parent
// turn. It has no process or worker-event dependency.
type CompletionTracker struct {
	mu      sync.Mutex
	pending map[string]struct{}
	ready   []Completion
	changed chan struct{}
}

func NewCompletionTracker() *CompletionTracker {
	return &CompletionTracker{changed: make(chan struct{}), pending: make(map[string]struct{})}
}

// TrackResident records an accepted resident turn. It returns false when the
// turn is incomplete or already tracked.
func (t *CompletionTracker) TrackResident(agentID, turnID string) bool {
	if t == nil {
		return false
	}
	key := completionKey(agentID, turnID)
	if key == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.pending[key]; exists {
		return false
	}
	t.pending[key] = struct{}{}
	t.signalLocked()
	return true
}

// Report accepts one terminal completion for an accepted resident turn. It
// returns false for duplicate or unknown terminal reports.
func (t *CompletionTracker) Report(completion Completion) bool {
	if t == nil {
		return false
	}
	key := completionKey(completion.AgentID, completion.TurnID)
	if key == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.pending[key]; !exists {
		return false
	}
	delete(t.pending, key)
	t.ready = append(t.ready, completion)
	t.signalLocked()
	return true
}

func completionKey(agentID, turnID string) string {
	if agentID == "" || turnID == "" {
		return ""
	}
	return agentID + "\x00" + turnID
}

func (t *CompletionTracker) Pending() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pending)
}

// Reset drops buffered outcomes at a parent-turn cancellation boundary.
func (t *CompletionTracker) Reset() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.pending = make(map[string]struct{})
	t.ready = nil
	t.signalLocked()
	t.mu.Unlock()
}

func (t *CompletionTracker) WaitIdle(ctx context.Context) ([]Completion, error) {
	return t.wait(ctx, true)
}

// WaitReady returns available completions without waiting for sibling turns.
// It returns an empty batch when no accepted turns remain.
func (t *CompletionTracker) WaitReady(ctx context.Context) ([]Completion, error) {
	return t.wait(ctx, false)
}

func (t *CompletionTracker) wait(ctx context.Context, idle bool) ([]Completion, error) {
	if t == nil {
		return nil, nil
	}
	for {
		t.mu.Lock()
		if len(t.pending) == 0 || (!idle && len(t.ready) != 0) {
			ready := append([]Completion(nil), t.ready...)
			t.ready = nil
			t.mu.Unlock()
			return ready, nil
		}
		changed := t.changed
		t.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (t *CompletionTracker) signalLocked() { close(t.changed); t.changed = make(chan struct{}) }

func FormatCompletionUpdate(batch []Completion, instruction string) string {
	return FormatCompletionUpdateWithActive(batch, nil, instruction)
}

// FormatCompletionUpdateWithActive formats terminal outcomes and identifies
// sibling agents that are still running when this update is emitted.
func FormatCompletionUpdateWithActive(batch []Completion, activeAgentIDs []string, instruction string) string {
	if len(batch) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[auto-subagents update]\n")
	for _, completion := range batch {
		fmt.Fprintf(&b, "- %s: %s", completion.AgentID, completion.Status)
		if completion.Status == CompletionDropped {
			// The child never read this follow-up. Echoing it as a task would
			// read as work the child attempted.
			fmt.Fprintf(&b, " (queued follow-up never started: %s)\n", undeliveredPreview(completion.Task))
			continue
		}
		if completion.Error != "" {
			fmt.Fprintf(&b, " (%s)", completion.Error)
		}
		if completion.Task != "" {
			fmt.Fprintf(&b, " — %s", completion.Task)
		}
		if len(completion.Undelivered) != 0 {
			fmt.Fprintf(&b, "\n  undelivered: %d follow-up(s) the child never read:", len(completion.Undelivered))
			for _, text := range completion.Undelivered {
				fmt.Fprintf(&b, "\n    - %s", undeliveredPreview(text))
			}
		}
		if completion.Summary != "" {
			label := "final"
			if completion.Status != string(ResidentCompleted) {
				label = "partial"
			}
			fmt.Fprintf(&b, "\n  %s: %s", label, completion.Summary)
		}
		b.WriteByte('\n')
	}
	if len(activeAgentIDs) != 0 {
		b.WriteString("Still running:")
		for _, agentID := range activeAgentIDs {
			fmt.Fprintf(&b, " %s", agentID)
		}
		b.WriteByte('\n')
	}
	if instruction != "" {
		b.WriteString(instruction)
	}
	return strings.TrimSpace(b.String())
}

// undeliveredPreview renders one follow-up on a single bounded line.
func undeliveredPreview(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) <= maxUndeliveredPreview {
		return text
	}
	return strings.TrimSpace(string(runes[:maxUndeliveredPreview-1])) + "…"
}
