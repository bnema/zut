package core

import (
	"context"

	"github.com/bnema/zut/packages/provider"
)

// WatcherVerdict is a watcher's decision at a blocking checkpoint.
type WatcherVerdict struct {
	// Hold stops the pending action. At BeforeTool the call is not executed
	// and Note becomes its error result. At BeforeFinish the loop keeps
	// running and Note is delivered as a host message before the next model
	// call.
	Hold bool
	// Note is the text shown to the executor. Empty with Hold false means
	// "proceed".
	Note string
}

// Watcher observes an agent's loop and may intervene at safe boundaries. It
// is how a passive reviewer (the advisor) watches the executor without the
// executor having to call it.
//
// All methods are called from the loop goroutine. StepCompleted must not
// block; BeforeTool and BeforeFinish may block and must honor ctx.
// Asynchronous advice is delivered with Agent.QueuePrompt as a HostEvent, so
// it reaches the model at the next safe boundary and never splits a tool call
// from its result.
type Watcher interface {
	// RunStarted is called when Prompt or Continue enters the loop. ctx is
	// the run context; work started for this run should stop when it ends.
	RunStarted(ctx context.Context)
	// StepCompleted is called after a tool-results message is appended.
	StepCompleted()
	// BeforeTool is called before each top-level tool call runs.
	BeforeTool(ctx context.Context, call provider.ToolCallBlock) WatcherVerdict
	// BeforeFinish is called when the model ended its turn with a final
	// answer and nothing else is queued.
	BeforeFinish(ctx context.Context) WatcherVerdict
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
// before the next model call.
func (a *Agent) QueueWatcherNote(note string) bool {
	return a.QueuePrompt(QueuedMessage{Text: note, HostEvent: true, Meta: map[string]string{WatcherNoteMetaKey: "true"}})
}

// watcherHold asks the watcher about tc and returns the error result to use
// instead of running it when the watcher holds the call.
func (a *Agent) watcherHold(ctx context.Context, tc provider.ToolCallBlock) (ToolResult, bool) {
	w := a.Watcher()
	if w == nil {
		return ToolResult{}, false
	}
	v := w.BeforeTool(ctx, tc)
	if !v.Hold {
		return ToolResult{}, false
	}
	note := v.Note
	if note == "" {
		note = "Held by the advisor before running."
	}
	return ToolResult{Content: []provider.Content{provider.TextBlock{Text: note}}, IsError: true}, true
}
