// Package daemon owns long-lived zut sessions on behalf of local clients.
//
// The daemon keeps a registry of session handles created through a
// session.Factory, enforces a single controlling client per session (D5),
// persists open sessions to $ZUT_HOME/daemon/state.json for idle restore (D6),
// closes eligible detached sessions (D7), recreates sessions on clear (D9) and
// implements session.Directory for guarded inter-session messages (D10).
//
// Locking: lifecycle operations are serialized by opMu and may call Handle
// methods. mu only guards the maps and is never held across a Handle call, so
// session loops calling back into Directory can never deadlock with a
// lifecycle operation.
package daemon

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bnema/zut/packages/agent/internal/flock"
	"github.com/bnema/zut/packages/agent/session"
)

const (
	// DefaultGCInterval is how often idle GC eligibility is evaluated.
	DefaultGCInterval = 5 * time.Minute
	// deliveryQueueSize bounds pending inter-session deliveries per target;
	// Send fails with session.ErrBusy when the target's queue is full.
	deliveryQueueSize = 16
	// deliveryTimeout bounds one Submit to a target.
	deliveryTimeout = 5 * time.Second
	// clientOutboxSize bounds queued session events per client.
	clientOutboxSize = 64
	// controlQueueSize bounds queued control notifications per client; a
	// client overflowing it is disconnected.
	controlQueueSize = 8
	// runPersistDebounce is the minimum spacing of run-state persists.
	runPersistDebounce = time.Second
	// stateRefreshInterval is how often running flags are re-persisted.
	stateRefreshInterval = 15 * time.Second
)

// Daemon is the session registry and lifecycle owner. Create it with New and
// start it with Run; a Daemon runs at most once.
type Daemon struct {
	factory session.Factory

	opMu sync.Mutex // serializes lifecycle operations

	mu         sync.Mutex
	reg        map[session.ID]*entry
	controller map[session.ID]*client
	clients    map[uint64]*client
	nextClient uint64
	stopping   bool
	ran        atomic.Bool
	// stopOverride is daemon_stop's graceful_ms as a Duration (0 = unset).
	stopOverride atomic.Int64
	// sessCtx is passed to the factory; it outlives ctx so sessions can be
	// closed during shutdown and is cancelled once they are.
	sessCtx    context.Context
	sessCancel context.CancelFunc
	// lastRunning is the persisted running set, refreshed by stateLoop.
	lastRunning map[session.ID]bool

	opts       Options
	log        *slog.Logger
	started    time.Time
	wg         sync.WaitGroup
	ctx        context.Context // lifecycle context, set by Run
	stop       context.CancelFunc
	deliverers map[session.ID]chan session.Input
	// runDebounce spaces run-state persists (default runPersistDebounce).
	runDebounce time.Duration
	// stateWrites receives a non-blocking signal after each state.json
	// write (cap 1); it lets tests observe persists without polling.
	stateWrites chan struct{}
	// runDirty signals a run-state transition to stateLoop (cap 1).
	runDirty chan struct{}
	// bot holds hosted bot services and bridges (bot.go).
	bot *botState
}

type entry struct {
	id         session.ID
	h          session.Handle
	spec       stateSession
	lock       *flock.Lock
	detachedAt time.Time
}

// New returns a daemon that builds sessions with factory. The factory may
// capture the daemon as its session.Directory.
func New(factory session.Factory) *Daemon {
	return &Daemon{
		factory:     factory,
		reg:         make(map[session.ID]*entry),
		controller:  make(map[session.ID]*client),
		clients:     make(map[uint64]*client),
		deliverers:  make(map[session.ID]chan session.Input),
		runDirty:    make(chan struct{}, 1),
		runDebounce: runPersistDebounce,
		stateWrites: make(chan struct{}, 1),
		log:         discardLogger(),
		bot:         newBotState(),
	}
}

func (d *Daemon) now() time.Time {
	if d.opts.Clock != nil {
		return d.opts.Clock()
	}
	return time.Now()
}

func (d *Daemon) lookup(id session.ID) *entry {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reg[id]
}

// entries returns a stable copy of registered entries.
func (d *Daemon) entries() []*entry {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*entry, 0, len(d.reg))
	for _, e := range d.reg {
		out = append(out, e)
	}
	return out
}

// runningChanged reports whether any session's running flag differs from the
// last persisted state. Callers hold opMu.
func (d *Daemon) runningChanged() bool {
	es := d.entries()
	if len(es) != len(d.lastRunning) {
		return true
	}
	for _, e := range es {
		if (e.h.Info().Run == session.RunStateRunning) != d.lastRunning[e.id] {
			return true
		}
	}
	return false
}

// markRunDirty asks stateLoop to persist a run-state transition. Never blocks.
func (d *Daemon) markRunDirty() {
	select {
	case d.runDirty <- struct{}{}:
	default:
	}
}

// stateLoop persists running flags so that a crash marks in-flight turns
// interrupted on restore (D6): on run-state transitions, at most once per
// runPersistDebounce, and every stateRefreshInterval as a backup.
func (d *Daemon) stateLoop(ctx context.Context) {
	t := time.NewTicker(stateRefreshInterval)
	defer t.Stop()
	var last time.Time
	var debounce *time.Timer
	var debounceC <-chan time.Time
	defer func() {
		if debounce != nil {
			debounce.Stop()
		}
	}()
	flush := func() {
		d.opMu.Lock()
		if !d.isStopping() && d.runningChanged() {
			d.persist()
		}
		d.opMu.Unlock()
		last = time.Now()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			flush()
		case <-d.runDirty:
			if debounceC != nil {
				continue // a flush is already scheduled
			}
			if wait := d.runDebounce - time.Since(last); wait > 0 {
				debounce = time.NewTimer(wait)
				debounceC = debounce.C
				continue
			}
			flush()
		case <-debounceC:
			debounceC = nil
			flush()
		}
	}
}

// persist writes state.json from the registry. Callers hold opMu.
func (d *Daemon) persist() {
	if d.opts.Home == "" {
		return
	}
	st := persistedState{}
	running := map[session.ID]bool{}
	for _, e := range d.entries() {
		s := e.spec
		s.Running = e.h.Info().Run == session.RunStateRunning
		running[e.id] = s.Running
		st.Sessions = append(st.Sessions, s)
	}
	d.lastRunning = running
	sortState(&st)
	if err := saveState(d.opts.Home, st); err != nil {
		d.log.Error("persist daemon state", "err", err)
		return
	}
	select {
	case d.stateWrites <- struct{}{}:
	default:
	}
}
