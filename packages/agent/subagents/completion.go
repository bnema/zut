package subagents

import (
	"context"
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
	if t == nil {
		return nil, nil
	}
	for {
		t.mu.Lock()
		if len(t.pending) == 0 {
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
	if len(batch) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[auto-subagents update]\n")
	for _, completion := range batch {
		fmt.Fprintf(&b, "- %s: %s", completion.AgentID, completion.Status)
		if completion.Error != "" {
			fmt.Fprintf(&b, " (%s)", completion.Error)
		}
		if completion.Task != "" {
			fmt.Fprintf(&b, " — %s", completion.Task)
		}
		if completion.Summary != "" {
			fmt.Fprintf(&b, "\n  final: %s", completion.Summary)
		}
		b.WriteByte('\n')
	}
	if instruction != "" {
		b.WriteString(instruction)
	}
	return strings.TrimSpace(b.String())
}
