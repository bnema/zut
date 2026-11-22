package advisor

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

const (
	// noteCooldownSteps is how many background steps are skipped after a
	// background note, so the executor has room to act on it.
	noteCooldownSteps = 2
	// maxBackgroundNotes caps background notes per request; checkpoints
	// stay available after the cap.
	maxBackgroundNotes = 3
	// checkpointTimeout bounds a blocking review. When it expires the
	// executor proceeds: the advisor gives advice, not permission.
	checkpointTimeout = 3 * time.Minute
)

// WatcherConfig wires a Watcher to one executor agent.
type WatcherConfig struct {
	// Reviewer runs client-side reviews. Nil disables them.
	Reviewer *Reviewer
	// Native reports whether the provider-native advisor serves the next
	// executor request. Native mode cannot review on its own, so the
	// watcher asks the executor to call advisor() at checkpoints instead.
	Native func() bool
	// Queue delivers a note to the executor before its next model call.
	Queue func(note string) bool
	// Messages returns the executor transcript.
	Messages func() []provider.Message
}

// Watcher is the passive advisor: it implements core.Watcher. In client mode
// it reviews every tool batch in the background and delivers notes only when
// the advisor has something to say. In both modes it holds the executor at two
// checkpoints per request: its first change, and its final answer when the
// request changed something.
type Watcher struct {
	reviewer *Reviewer
	queue    func(note string) bool
	messages func() []provider.Message

	mu     sync.Mutex
	native func() bool
	cancel context.CancelFunc
	// bgCtx scopes background reviews. The final checkpoint cancels it: its
	// review covers their work, and a late note must not outlive the request.
	bgCtx      context.Context
	bgCancel   context.CancelFunc
	runStart   int
	changed    bool // a change was attempted in this run
	changeDone bool // the first-change checkpoint ran
	finishDone bool // the final-answer checkpoint ran
	running    bool // a background review is in flight
	dirty      bool // steps completed while it ran
	cooldown   int
	notes      int // background notes delivered in this run
}

var _ core.Watcher = (*Watcher)(nil)

// NewWatcher returns a watcher for cfg.
func NewWatcher(cfg WatcherConfig) *Watcher {
	return &Watcher{reviewer: cfg.Reviewer, queue: cfg.Queue, messages: cfg.Messages, native: cfg.Native}
}

// Reviewer returns the watcher's reviewer, so a rebuilt registry can keep
// the advisor's conversation.
func (w *Watcher) Reviewer() *Reviewer { return w.reviewer }

// SetNative replaces the native-mode probe after the advisor mode was
// decided again.
func (w *Watcher) SetNative(native func() bool) {
	w.mu.Lock()
	w.native = native
	w.mu.Unlock()
}

func (w *Watcher) isNative() bool {
	w.mu.Lock()
	native := w.native
	w.mu.Unlock()
	return native != nil && native()
}

func (w *Watcher) transcript() []provider.Message {
	if w.messages == nil {
		return nil
	}
	return w.messages()
}

// RunStarted resets the per-request checkpoints.
func (w *Watcher) RunStarted(ctx context.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	start := len(w.transcript())
	w.mu.Lock()
	if w.cancel != nil {
		w.cancel()
	}
	w.cancel = cancel
	w.bgCtx, w.bgCancel = context.WithCancel(runCtx)
	w.runStart = start
	w.changed, w.changeDone, w.finishDone = false, false, false
	w.dirty, w.cooldown, w.notes = false, 0, 0
	w.mu.Unlock()
}

// RunEnded stops any background review of this run.
func (w *Watcher) RunEnded() {
	w.mu.Lock()
	if w.cancel != nil {
		w.cancel()
	}
	w.mu.Unlock()
}

// TakeUsage returns the usage spent by watcher reviews.
func (w *Watcher) TakeUsage() (provider.Usage, bool) {
	if w.reviewer == nil {
		return provider.Usage{}, false
	}
	return w.reviewer.TakeUsage()
}

// StepCompleted starts a background review, or marks one pending when a
// review is already running. Never blocks.
func (w *Watcher) StepCompleted() {
	if w.reviewer == nil || w.isNative() {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.bgCtx == nil || w.bgCtx.Err() != nil || w.notes >= maxBackgroundNotes {
		return
	}
	if w.cooldown > 0 {
		w.cooldown--
		return
	}
	if w.running {
		w.dirty = true
		return
	}
	w.running = true
	go w.backgroundReviews(w.bgCtx)
}

func (w *Watcher) backgroundReviews(ctx context.Context) {
	for {
		v, err := w.reviewer.Review(ctx, KindStep, nil)
		w.mu.Lock()
		// Delivery happens under mu, after checking ctx, so a canceled
		// request never receives a stale note.
		if err == nil && ctx.Err() == nil && v.Decision != Silent && v.Text != "" {
			format := noteFormat
			if v.Decision == Stop {
				format = stopFormat
			}
			w.deliverLocked(format, v.Text)
			w.notes++
			w.cooldown = noteCooldownSteps
			w.dirty = false
		}
		if w.dirty && ctx.Err() == nil && w.notes < maxBackgroundNotes {
			w.dirty = false
			w.mu.Unlock()
			continue
		}
		w.running = false
		w.mu.Unlock()
		return
	}
}

func (w *Watcher) deliverLocked(format, text string) {
	if w.queue != nil {
		w.queue(fmt.Sprintf(format, text))
	}
}

// BeforeTool holds the first change of a request for review.
func (w *Watcher) BeforeTool(ctx context.Context, call provider.ToolCallBlock) core.WatcherVerdict {
	if !IsChange(call) {
		return core.WatcherVerdict{}
	}
	w.mu.Lock()
	w.changed = true
	if w.changeDone {
		w.mu.Unlock()
		return core.WatcherVerdict{}
	}
	w.changeDone = true
	runStart := w.runStart
	w.mu.Unlock()

	if w.isNative() {
		if consultedSince(w.transcript(), runStart) {
			return core.WatcherVerdict{}
		}
		return core.WatcherVerdict{Hold: true, Note: nativeChangeNotice}
	}
	return w.checkpoint(ctx, KindChange, &call, holdChangeFormat)
}

// BeforeFinish reviews a final answer once per request, when the request
// changed something. It first stops background reviews, which this review
// supersedes.
func (w *Watcher) BeforeFinish(ctx context.Context) core.WatcherVerdict {
	w.mu.Lock()
	if w.bgCancel != nil {
		w.bgCancel()
	}
	if w.finishDone || !w.changed {
		w.mu.Unlock()
		return core.WatcherVerdict{}
	}
	w.finishDone = true
	w.mu.Unlock()

	if w.isNative() {
		if consultedAfterLastChange(w.transcript()) {
			return core.WatcherVerdict{}
		}
		return core.WatcherVerdict{Hold: true, Note: nativeFinishNotice}
	}
	return w.checkpoint(ctx, KindFinish, nil, holdFinishFormat)
}

// checkpoint runs a blocking client review. A failed or timed-out review
// lets the executor proceed: the advisor gives advice, not permission. A
// STOP holds the action: core turns the note into the held call's result, or
// queues it after a final answer. A NOTE is queued and read before the next
// model call (after a final answer, core runs one more turn for it).
func (w *Watcher) checkpoint(ctx context.Context, kind Kind, pending *provider.ToolCallBlock, holdFormat string) core.WatcherVerdict {
	if w.reviewer == nil {
		return core.WatcherVerdict{}
	}
	reviewCtx, cancel := context.WithTimeout(ctx, checkpointTimeout)
	defer cancel()
	done := core.ReportWatcherReview(ctx, kind.String())
	v, err := w.reviewer.Review(reviewCtx, kind, pending)
	done()
	if err != nil || ctx.Err() != nil || v.Text == "" {
		return core.WatcherVerdict{}
	}
	switch v.Decision {
	case Stop:
		return core.WatcherVerdict{Hold: true, Note: fmt.Sprintf(holdFormat, v.Text)}
	case Note:
		w.mu.Lock()
		w.deliverLocked(noteFormat, v.Text)
		w.mu.Unlock()
	}
	return core.WatcherVerdict{}
}

// consultedSince reports an advisor call (native or client) after msgs[from].
func consultedSince(msgs []provider.Message, from int) bool {
	if from < 0 || from > len(msgs) {
		from = 0
	}
	for _, m := range msgs[from:] {
		for _, c := range m.Content {
			if isConsultBlock(c) {
				return true
			}
		}
	}
	return false
}

// consultedAfterLastChange reports whether the advisor was consulted after
// the transcript's last change call.
func consultedAfterLastChange(msgs []provider.Message) bool {
	for i := len(msgs) - 1; i >= 0; i-- {
		content := msgs[i].Content
		for j := len(content) - 1; j >= 0; j-- {
			if isConsultBlock(content[j]) {
				return true
			}
			if call, ok := content[j].(provider.ToolCallBlock); ok && IsChange(call) {
				return false
			}
		}
	}
	return false
}

func isConsultBlock(c provider.Content) bool {
	switch v := c.(type) {
	case provider.ServerToolBlock:
		return v.Name == provider.AdvisorToolName
	case provider.ToolCallBlock:
		return v.Name == provider.AdvisorToolName
	}
	return false
}

// mutatingShell matches shell commands that change files, history, or
// installed state. It is a heuristic for "first change": read-only
// exploration (ls, cat, grep, git status/diff/log, tests) does not match.
var mutatingShell = regexp.MustCompile(`(?:^|[;&|(\s])(?:` +
	`rm|mv|cp|mkdir|rmdir|touch|chmod|chown|ln|tee|truncate|dd|patch|` +
	`sed\s+(?:-[a-zA-Z]*\s+)*-i|perl\s+-[a-zA-Z]*i|gofmt\s+-[a-z]*w|` +
	`git\s+(?:commit|push|reset|checkout|switch|rebase|merge|apply|am|stash|clean|restore|rm|mv|tag|cherry-pick|revert|branch\s+-[dDmM])|` +
	`(?:npm|pnpm|yarn|bun)\s+(?:install|i|add|remove|uninstall|update)|` +
	`go\s+(?:get|mod\s+(?:tidy|init|edit)|generate)|cargo\s+(?:add|remove|update)|pip3?\s+(?:install|uninstall)` +
	`)(?:\s|$)`)

// redirect matches output redirection to a file, ignoring fd duplication
// (2>&1) and /dev/null.
var redirect = regexp.MustCompile(`(?:^|[^0-9&>])>>?\s*(?:[^&\s/]|/(?:[^d]|d[^e]|de[^v]))`)

// quoted matches single- and double-quoted shell strings, whose content is
// data (a grep pattern, a commit message), not commands.
var quoted = regexp.MustCompile(`'[^']*'|"(?:[^"\\]|\\.)*"`)

// IsChange reports whether call is likely to change the workspace: file
// edits and writes, ast rewrites, code execution (python, codemode), and
// state-changing shell commands.
func IsChange(call provider.ToolCallBlock) bool {
	switch call.Name {
	case "edit", "write", "python", "codemode":
		return true
	case "ast":
		var args struct {
			Rewrite string `json:"rewrite"`
		}
		return json.Unmarshal(call.Arguments, &args) == nil && strings.TrimSpace(args.Rewrite) != ""
	case "bash":
		var args struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(call.Arguments, &args) != nil {
			return false
		}
		cmd := quoted.ReplaceAllString(args.Command, "''")
		return mutatingShell.MatchString(cmd) || redirect.MatchString(cmd)
	}
	return false
}
