package advisor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

const (
	// noteCooldownSteps is how many background steps are skipped after a
	// background note, so the executor has room to act on it.
	noteCooldownSteps = 2
	// maxBackgroundNotes caps background notes per request.
	maxBackgroundNotes = 3
	// reviewTimeout bounds background reviews.
	reviewTimeout = 3 * time.Minute
)

// Mode is how the advisor reaches the executor for the next request.
type Mode int

const (
	// Off pauses the watcher, for example while the persisted settings no
	// longer match the session's decision and a refresh is pending.
	Off Mode = iota
	// Client runs background reviews as separate advisor requests.
	Client
	// Native runs no client-side watcher reviews: the executor calls the
	// native advisor itself.
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
}

// Watcher is the passive advisor: it implements core.Watcher. In client mode
// it reviews every tool batch in the background and delivers notes only when
// the advisor has something to say. Native mode runs no watcher reviews.
type Watcher struct {
	reviewer *Reviewer
	queue    func(note string) bool

	mu     sync.Mutex
	mode   func() Mode
	runCtx context.Context
	cancel context.CancelFunc
	// bgCtx scopes background reviews to the current run.
	bgCtx    context.Context
	bgCancel context.CancelFunc
	// gen identifies the run; a background goroutine only updates state
	// for its own run.
	gen      uint64
	running  bool // a background review of this run is in flight
	dirty    bool // steps completed while it ran
	cooldown int
	notes    int // background notes delivered in this run
}

var _ core.Watcher = (*Watcher)(nil)

// NewWatcher returns a watcher for cfg.
func NewWatcher(cfg WatcherConfig) *Watcher {
	return &Watcher{reviewer: cfg.Reviewer, queue: cfg.Queue, mode: cfg.Mode}
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

// RunStarted resets background review state for the request.
func (w *Watcher) RunStarted(ctx context.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	w.mu.Lock()
	if w.cancel != nil {
		w.cancel()
	}
	w.gen++
	w.runCtx, w.cancel = runCtx, cancel
	w.rearmLocked()
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
		reviewCtx, cancel := context.WithTimeout(ctx, reviewTimeout)
		v, err := w.reviewer.Review(reviewCtx)
		cancel()
		w.mu.Lock()
		dropped := gen != w.gen || w.bgCtx != ctx || ctx.Err() != nil
		if dropped && err == nil && v.Decision != Silent {
			w.reviewer.Undelivered(v.Text)
		}
		if gen != w.gen || w.bgCtx != ctx {
			// A newer run owns the background state.
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
