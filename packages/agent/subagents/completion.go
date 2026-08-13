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
	Status  string
	Task    string
	Error   string
	Summary string
}

// CompletionTracker coordinates resident child completions with the parent
// turn. It has no process or worker-event dependency.
type CompletionTracker struct {
	mu      sync.Mutex
	pending int
	ready   []Completion
	changed chan struct{}
}

func NewCompletionTracker() *CompletionTracker {
	return &CompletionTracker{changed: make(chan struct{})}
}

func (t *CompletionTracker) TrackResident() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.pending++
	t.signalLocked()
	t.mu.Unlock()
}

func (t *CompletionTracker) Report(completion Completion) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.pending > 0 {
		t.pending--
	}
	t.ready = append(t.ready, completion)
	t.signalLocked()
	t.mu.Unlock()
}

func (t *CompletionTracker) Pending() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pending
}

// Reset drops buffered outcomes at a parent-turn cancellation boundary.
func (t *CompletionTracker) Reset() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.pending = 0
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
		if t.pending == 0 {
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
