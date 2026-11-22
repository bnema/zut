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
// before the next model call. Notes still queued when the run ends are
// dropped.
func (a *Agent) QueueWatcherNote(note string) bool {
	return a.QueuePrompt(QueuedMessage{Text: note, HostEvent: true, Meta: map[string]string{WatcherNoteMetaKey: "true"}})
}

type watcherReportKey struct{}

// withWatcherReport lets a watcher checkpoint report its blocking review on
// the loop's event sink.
func withWatcherReport(ctx context.Context, sink func(AgentEvent)) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, watcherReportKey{}, sink)
}

// ReportWatcherReview announces that the executor is waiting on a blocking
// review of kind ("change" or "finish") and returns the call that ends it.
// Watchers call it from BeforeTool or BeforeFinish only when they actually
// review; it is a no-op outside a checkpoint.
func ReportWatcherReview(ctx context.Context, kind string) (done func()) {
	sink, _ := ctx.Value(watcherReportKey{}).(func(AgentEvent))
	if sink == nil {
		return func() {}
	}
	sink(EvAdvisorReview{Kind: kind, Phase: AdvisorReviewStart})
	return func() { sink(EvAdvisorReview{Kind: kind, Phase: AdvisorReviewEnd}) }
}

// watcherHold asks the watcher about tc and returns the error result to use
// instead of running it when the watcher holds the call.
func (a *Agent) watcherHold(ctx context.Context, tc provider.ToolCallBlock, sink func(AgentEvent)) (ToolResult, bool) {
	w := a.Watcher()
	if w == nil {
		return ToolResult{}, false
	}
	v := w.BeforeTool(withWatcherReport(ctx, sink), tc)
	// A cancel while the watcher reviewed must not let the call run: tools
	// do not all check ctx before acting.
	if err := ctx.Err(); err != nil {
		return ToolResult{Content: []provider.Content{provider.TextBlock{Text: "Aborted: " + err.Error()}}, IsError: true}, true
	}
	if !v.Hold {
		return ToolResult{}, false
	}
	return ToolResult{Content: []provider.Content{provider.TextBlock{Text: watcherNoteText(v.Note)}}, IsError: true}, true
}

func watcherNoteText(note string) string {
	if note == "" {
		return "Held by the advisor."
	}
	return note
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
