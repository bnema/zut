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
	// checkpointTimeout bounds every review. When a blocking one expires the
	// executor proceeds: the advisor gives advice, not permission.
	checkpointTimeout = 3 * time.Minute
)

// Mode is how the advisor reaches the executor for the next request.
type Mode int

const (
	// Off pauses the watcher, for example while the persisted settings no
	// longer match the session's decision and a refresh is pending.
	Off Mode = iota
	// Client runs reviews as separate advisor requests.
	Client
	// Native relies on the provider-side advisor tool, which only the
	// executor can call: checkpoints ask it to.
	Native
)

// WatcherConfig wires a Watcher to one executor agent.
type WatcherConfig struct {
	// Reviewer runs client-side reviews. Nil disables them.
	Reviewer *Reviewer
	// Mode reports the advisor path for the next executor request. Nil
	// means Client.
	Mode func() Mode
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
	mode   func() Mode
	runCtx context.Context
	cancel context.CancelFunc
	// bgCtx scopes background reviews. Checkpoints cancel it so their review
	// never waits behind a background one, then re-arm it.
	bgCtx    context.Context
	bgCancel context.CancelFunc
	// gen identifies the run; a background goroutine only updates state
	// for its own run.
	gen        uint64
	runStart   int
	changed    bool // a change was attempted in this run
	changeDone bool // the first-change checkpoint ran
	finishDone bool // the final-answer checkpoint ran
	running    bool // a background review of this run is in flight
	dirty      bool // steps completed while it ran
	cooldown   int
	notes      int // background notes delivered in this run
}

var _ core.Watcher = (*Watcher)(nil)

// NewWatcher returns a watcher for cfg.
func NewWatcher(cfg WatcherConfig) *Watcher {
	return &Watcher{reviewer: cfg.Reviewer, queue: cfg.Queue, messages: cfg.Messages, mode: cfg.Mode}
}

// Reviewer returns the watcher's reviewer, so a rebuilt registry can keep
// the advisor's conversation.
func (w *Watcher) Reviewer() *Reviewer { return w.reviewer }

// SetMode replaces the mode probe after the advisor mode was decided again.
func (w *Watcher) SetMode(mode func() Mode) {
	w.mu.Lock()
	w.mode = mode
	w.mu.Unlock()
}

func (w *Watcher) currentMode() Mode {
	w.mu.Lock()
	mode := w.mode
	w.mu.Unlock()
	if mode == nil {
		return Client
	}
	return mode()
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
	w.gen++
	w.runCtx, w.cancel = runCtx, cancel
	w.rearmLocked()
	w.runStart = start
	w.changed, w.changeDone, w.finishDone = false, false, false
	w.running, w.dirty, w.cooldown, w.notes = false, false, 0, 0
	w.mu.Unlock()
}

// RunEnded stops any review of this run.
func (w *Watcher) RunEnded() {
	w.mu.Lock()
	if w.cancel != nil {
		w.cancel()
	}
	w.mu.Unlock()
}

// rearmLocked starts a fresh background scope for the current run. The
// previous background review, if any, is canceled.
func (w *Watcher) rearmLocked() {
	if w.bgCancel != nil {
		w.bgCancel()
	}
	w.running, w.dirty = false, false
	if w.runCtx == nil {
		w.bgCtx, w.bgCancel = nil, nil
		return
	}
	w.bgCtx, w.bgCancel = context.WithCancel(w.runCtx)
}

// TakeUsage returns the usage spent by watcher reviews.
func (w *Watcher) TakeUsage() (provider.Usage, bool) {
	if w.reviewer == nil {
		return provider.Usage{}, false
	}
	return w.reviewer.TakeUsage()
}

// StepCompleted starts a background review, or marks one pending when a
// review is already running. Never blocks on the advisor.
func (w *Watcher) StepCompleted() {
	if w.reviewer == nil {
		return
	}
	w.mu.Lock()
	if w.bgCtx == nil || w.bgCtx.Err() != nil || w.notes >= maxBackgroundNotes {
		w.mu.Unlock()
		return
	}
	if w.cooldown > 0 {
		w.cooldown--
		w.mu.Unlock()
		return
	}
	if w.running {
		w.dirty = true
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()
	// The mode probe may read config; keep it off the cheap paths above.
	if w.currentMode() != Client {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running || w.bgCtx == nil || w.bgCtx.Err() != nil {
		return
	}
	w.running = true
	go w.backgroundReviews(w.bgCtx, w.gen)
}

func (w *Watcher) backgroundReviews(ctx context.Context, gen uint64) {
	for {
		reviewCtx, cancel := context.WithTimeout(ctx, checkpointTimeout)
		v, err := w.reviewer.Review(reviewCtx, KindStep, nil, nil)
		cancel()
		w.mu.Lock()
		if gen != w.gen || w.bgCtx != ctx {
			// A newer run or checkpoint owns the background state.
			w.mu.Unlock()
			return
		}
		// Delivery happens under mu, after checking ctx, so a canceled
		// scope never receives a stale note.
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

	switch w.currentMode() {
	case Native:
		if consultedSince(w.transcript(), runStart) {
			return core.WatcherVerdict{}
		}
		return core.WatcherVerdict{Hold: true, Note: nativeChangeNotice}
	case Client:
		return w.checkpoint(ctx, KindChange, &call, holdChangeFormat)
	}
	return core.WatcherVerdict{}
}

// BeforeFinish reviews a final answer once per request, when the request
// changed something.
func (w *Watcher) BeforeFinish(ctx context.Context) core.WatcherVerdict {
	w.mu.Lock()
	if w.finishDone || !w.changed {
		w.mu.Unlock()
		return core.WatcherVerdict{}
	}
	w.finishDone = true
	w.mu.Unlock()

	switch w.currentMode() {
	case Native:
		if consultedAfterLastChange(w.transcript()) {
			return core.WatcherVerdict{}
		}
		return core.WatcherVerdict{Hold: true, Note: nativeFinishNotice}
	case Client:
		return w.checkpoint(ctx, KindFinish, nil, holdFinishFormat)
	}
	return core.WatcherVerdict{}
}

// checkpoint runs a blocking client review. It first cancels the background
// review, which this one supersedes (its delta stays unsent, so this review
// covers it), and re-arms background reviews for any work that follows.
//
// A failed or timed-out review lets the executor proceed: the advisor gives
// advice, not permission. A STOP holds the action: core turns the note into
// the held call's result, or queues it after a final answer. A NOTE is queued
// and read before the next model call (after a final answer, core runs one
// more turn for it).
func (w *Watcher) checkpoint(ctx context.Context, kind Kind, pending *provider.ToolCallBlock, holdFormat string) core.WatcherVerdict {
	if w.reviewer == nil {
		return core.WatcherVerdict{}
	}
	w.mu.Lock()
	w.rearmLocked()
	w.mu.Unlock()

	reviewCtx, cancel := context.WithTimeout(ctx, checkpointTimeout)
	defer cancel()
	done := func() {}
	v, err := w.reviewer.Review(reviewCtx, kind, pending, func() {
		done = core.ReportWatcherReview(ctx, kind.String())
	})
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
// A from past the transcript (compacted mid-request) reports false.
func consultedSince(msgs []provider.Message, from int) bool {
	if from < 0 || from > len(msgs) {
		return false
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

// commandStart matches the start of a shell command: the line start, a
// separator, or a wrapper that runs its arguments as a command.
const commandStart = `(?:^|[;&|(\n]\s*|\b(?:sudo|xargs|env|time|nice|nohup)\s+(?:-\S+\s+)*|-exec\s+)`

// mutatingShell matches shell commands that change files, history, or
// installed state, in command position. It is a heuristic for "first
// change": read-only exploration (ls, cat, grep, git status/diff/log, tests)
// does not match.
var mutatingShell = regexp.MustCompile(commandStart + `(?:` +
	`(?:rm|mv|cp|mkdir|rmdir|touch|chmod|chown|ln|tee|truncate|dd|patch|install)(?:\s|$)|` +
	`sed\s+(?:\S+\s+)*?(?:-[a-zA-Z]*i|--in-place)|perl\s+(?:-\S+\s+)*-[a-zA-Z]*i|` +
	`gofmt\s+(?:-\S+\s+)*-[a-z]*w|goimports\s+(?:-\S+\s+)*-w|` +
	`git\s+(?:-\S+\s+)*(?:commit|push|reset|checkout|switch|rebase|merge|apply|am|stash|clean|restore|rm|mv|tag|cherry-pick|revert|pull|branch\s+-[dDmM])(?:\s|$)|` +
	`(?:npm|pnpm|yarn|bun)\s+(?:install|i|ci|add|remove|uninstall|update|upgrade)(?:\s|$)|` +
	`go\s+(?:get|mod\s+(?:tidy|init|edit|vendor)|generate|install)(?:\s|$)|` +
	`cargo\s+(?:add|remove|update|install)(?:\s|$)|pip3?\s+(?:install|uninstall)(?:\s|$)` +
	`)`)

// shellPayload matches `bash -c '...'`-style payloads, checked as commands
// before quoted strings are blanked.
var shellPayload = regexp.MustCompile(`\b(?:ba|z)?sh\s+(?:-\S+\s+)*-c\s+(?:'([^']*)'|"((?:[^"\\]|\\.)*)")`)

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
		return json.Unmarshal(call.Arguments, &args) == nil && isShellChange(args.Command, 0)
	}
	return false
}

func isShellChange(cmd string, depth int) bool {
	if depth < 3 {
		for _, m := range shellPayload.FindAllStringSubmatch(cmd, -1) {
			if isShellChange(m[1]+m[2], depth+1) {
				return true
			}
		}
	}
	cmd = quoted.ReplaceAllString(cmd, "''")
	return mutatingShell.MatchString(cmd) || hasFileRedirect(cmd)
}

// hasFileRedirect reports output redirection to a file, ignoring fd
// duplication (2>&1) and /dev/null.
func hasFileRedirect(cmd string) bool {
	for i := 0; i < len(cmd); i++ {
		if cmd[i] != '>' {
			continue
		}
		if i > 0 && (cmd[i-1] == '-' || cmd[i-1] == '=') {
			continue // ->, => outside quotes
		}
		j := i + 1
		if j < len(cmd) && cmd[j] == '>' {
			j++
		}
		i = j - 1
		for j < len(cmd) && (cmd[j] == ' ' || cmd[j] == '\t') {
			j++
		}
		rest := cmd[j:]
		if rest == "" || rest[0] == '&' || strings.HasPrefix(rest, "/dev/null") {
			continue
		}
		return true
	}
	return false
}
