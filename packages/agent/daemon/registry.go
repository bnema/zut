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
	// deliveryQueueSize bounds pending inter-session deliveries; Send fails
	// with session.ErrBusy when it is full.
	deliveryQueueSize = 64
	// clientOutboxSize bounds daemon-originated notifications per client; a
	// client that falls this far behind is disconnected.
	clientOutboxSize = 64
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

	opts    Options
	log     *slog.Logger
	started time.Time
	wg      sync.WaitGroup
	ctx     context.Context // lifecycle context, set by Run
	stop    context.CancelFunc
	deliver chan delivery
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
		factory:    factory,
		reg:        make(map[session.ID]*entry),
		controller: make(map[session.ID]*client),
		clients:    make(map[uint64]*client),
		deliver:    make(chan delivery, deliveryQueueSize),
		log:        discardLogger(),
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

// stateLoop refreshes the running flags in state.json so that a crash marks
// in-flight turns interrupted on restore (D6).
func (d *Daemon) stateLoop(ctx context.Context) {
	t := time.NewTicker(stateRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.opMu.Lock()
			if !d.isStopping() && d.runningChanged() {
				d.persist()
			}
			d.opMu.Unlock()
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
	}
}
