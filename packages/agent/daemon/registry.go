// Package daemon owns long-lived zut sessions on behalf of local clients.
//
// The daemon keeps a registry of session handles created through a
// session.Factory, enforces a single controlling client per session (D5),
// starts empty on each daemon run,
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
	"os"
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
)

// Daemon is the session registry and lifecycle owner. Create it with New and
// start it with Run; a Daemon runs at most once.
type Daemon struct {
	factory session.Factory
	// peerUID is the UID socket peers must have (os.Getuid; tests override).
	peerUID int

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

	opts       Options
	log        *slog.Logger
	started    time.Time
	wg         sync.WaitGroup
	ctx        context.Context // lifecycle context, set by Run
	stop       context.CancelFunc
	deliverers map[session.ID]chan session.Input
	// bot holds hosted bot services and bridges (bot.go).
	bot *botState
}

type entry struct {
	parentPath string
	id         session.ID
	h          session.Handle
	spec       session.CreateSpec
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
		deliverers: make(map[session.ID]chan session.Input),
		log:        discardLogger(),
		bot:        newBotState(),
		peerUID:    os.Getuid(),
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
