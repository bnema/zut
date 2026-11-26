package core

import (
	"context"

	"github.com/bnema/zut/packages/provider"
)

// Watcher observes an agent's loop and may intervene at safe boundaries. It
// is how a passive reviewer (the advisor) watches the executor without the
// executor having to call it.
//
// All methods are called from the loop goroutine. StepCompleted must not
// block.
// Asynchronous advice is delivered with Agent.QueuePrompt as a HostEvent, so
// it reaches the model at the next safe boundary and never splits a tool call
// from its result.
type Watcher interface {
	// RunStarted is called when Prompt or Continue enters the loop. ctx is
	// the run context; work started for this run should stop when it ends.
	RunStarted(ctx context.Context)
	// StepCompleted is called after a tool-results message is appended.
	StepCompleted()
	// RunEnded is called when the loop returns, for any reason.
	RunEnded()
	// TakeUsage returns and clears usage the watcher spent since the last
	// call. The loop folds it into the cumulative cost as external usage.
	TakeUsage() (provider.Usage, bool)
}

// SetWatcher installs (or clears with nil) the loop watcher.
func (a *Agent) SetWatcher(w Watcher) {
	a.mu.Lock()
	a.watcher = w
	a.mu.Unlock()
}

// Watcher returns the installed loop watcher, or nil.
func (a *Agent) Watcher() Watcher {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.watcher
}

// foldWatcherUsage folds the watcher's spent usage into the cumulative cost
// as external usage (it does not fill the executor's context).
func (a *Agent) foldWatcherUsage(w Watcher, sink func(AgentEvent)) {
	if u, ok := w.TakeUsage(); ok {
		res := ToolResult{Usage: &u}
		a.foldToolUsage(&res, sink)
	}
}

// WatcherNoteMetaKey tags transcript messages that carry watcher (advisor)
// notes, so hosts can render them apart from user input.
const WatcherNoteMetaKey = "zut_advisor_note"

// QueueWatcherNote queues a watcher note as a tagged host message, delivered
// before the next model call. Notes still queued when the run ends are
// dropped.
func (a *Agent) QueueWatcherNote(note string) bool {
	return a.QueuePrompt(QueuedMessage{Text: note, HostEvent: true, Meta: map[string]string{WatcherNoteMetaKey: "true"}})
}

// endWatcherRun closes the watcher's run: watcher notes still queued belong
// to the run that ended (an error, length, or step-limit exit) and must not
// steer the next request, and usage spent since the last fold is recorded.
func (a *Agent) endWatcherRun(w Watcher) {
	w.RunEnded()
	a.mu.Lock()
	kept := a.queued[:0]
	for _, q := range a.queued {
		if q.message.Meta[WatcherNoteMetaKey] != "true" {
			kept = append(kept, q)
		}
	}
	for i := len(kept); i < len(a.queued); i++ {
		a.queued[i] = queuedMessage{}
	}
	a.queued = kept
	a.mu.Unlock()
	// The run's events are over, so the cost is recorded without an event.
	a.foldWatcherUsage(w, nil)
}
