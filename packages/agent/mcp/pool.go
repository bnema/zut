package mcp

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// errPoolClosed is returned by connects after Pool.Close.
var errPoolClosed = errors.New("pool closed")

// Pool shares stdio MCP server processes between Managers (sessions) whose
// launch is identical (see sharedKey). Each entry is reference counted by
// the Managers using it; at zero references it closes after the idle delay.
// The pool keeps raw tool lists; every Manager applies its own filters.
type Pool struct {
	idle time.Duration
	log  *slog.Logger

	// ctx bounds every pool-owned connect; Close cancels it.
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	closed  bool
	entries map[poolKey]*poolEntry
}

// NewPool returns a pool whose unreferenced processes stop after idle
// (0 stops them as soon as the last Manager releases them). log may be nil.
func NewPool(idle time.Duration, log *slog.Logger) *Pool {
	if idle < 0 {
		idle = 0
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Pool{idle: idle, log: log, ctx: ctx, cancel: cancel, entries: map[poolKey]*poolEntry{}}
}

// poolEntry is one shared server process. mu guards every field below it and
// is never held across process or network I/O.
type poolEntry struct {
	p        *Pool
	key      poolKey
	name     string // first configured name, for logs only
	cfg      ServerConfig
	rootsCWD string
	info     ClientInfo

	mu        sync.Mutex
	refs      int
	closed    bool
	client    *Client
	tools     []Tool // raw, unfiltered
	refresher *toolRefresher
	flight    *poolFlight
	subs      map[*poolLease]struct{}
	idleTimer *time.Timer
	idleGen   uint64
}

// poolFlight is one in-progress connect shared by every concurrent caller.
// Its deadline is the largest startup timeout among the callers that joined
// it, counted from the flight's start.
type poolFlight struct {
	done   chan struct{}
	client *Client
	tools  []Tool
	err    error

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	started  time.Time
	timeout  time.Duration
	timer    *time.Timer
	timedOut bool
}

func newPoolFlight(parent context.Context, timeout time.Duration) *poolFlight {
	ctx, cancel := context.WithCancel(parent)
	f := &poolFlight{done: make(chan struct{}), ctx: ctx, cancel: cancel, started: time.Now(), timeout: timeout}
	f.timer = time.AfterFunc(timeout, func() {
		f.mu.Lock()
		f.timedOut = true
		f.mu.Unlock()
		cancel()
	})
	return f
}

// extend raises the flight deadline to timeout (from the flight start) when
// it is larger than the current one and the deadline has not passed.
func (f *poolFlight) extend(timeout time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.timedOut || timeout <= f.timeout {
		return
	}
	if f.timer.Stop() {
		f.timeout = timeout
		f.timer.Reset(time.Until(f.started.Add(timeout)))
	}
}

// expired reports whether the flight hit its deadline and which one.
func (f *poolFlight) expired() (bool, time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.timedOut, f.timeout
}

func timeoutError(d time.Duration, err error) error {
	return fmt.Errorf("timed out after %s; raise startupTimeoutSec if the server is slow to start: %w", d, err)
}

// poolLease is one Manager's reference to an entry. onTools receives the
// raw tool list after each tools/list_changed re-listing, on the lease's own
// goroutine so one slow Manager never delays the others; deliveries are
// ordered and coalesced to the latest list.
type poolLease struct {
	entry   *poolEntry
	onTools func(raw []Tool)

	mu      sync.Mutex
	latest  []Tool
	pending bool
	running bool
}

// deliver hands raw to the lease without blocking the caller.
func (l *poolLease) deliver(raw []Tool) {
	if l.onTools == nil {
		return
	}
	l.mu.Lock()
	l.latest, l.pending = raw, true
	if l.running {
		l.mu.Unlock()
		return
	}
	l.running = true
	l.mu.Unlock()
	go func() {
		for {
			l.mu.Lock()
			if !l.pending {
				l.running = false
				l.mu.Unlock()
				return
			}
			tools := l.latest
			l.latest, l.pending = nil, false
			l.mu.Unlock()
			l.onTools(tools)
		}
	}()
}

func (e *poolEntry) logAttrs(extra ...any) []any {
	return append([]any{"server", e.name, "key", hex.EncodeToString(e.key[:4]), "refs", e.refs}, extra...)
}

// acquire returns a lease on the entry for cfg, creating the entry when
// needed. The lease must be released exactly once.
func (p *Pool) acquire(name string, cfg ServerConfig, sessionCWD string, info ClientInfo, onTools func([]Tool)) (*poolLease, error) {
	key, rootsCWD, err := sharedKey(cfg, sessionCWD)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errPoolClosed
	}
	result := "hit"
	e := p.entries[key]
	if e == nil {
		result = "miss"
		e = &poolEntry{p: p, key: key, name: name, cfg: cfg, rootsCWD: rootsCWD, info: info, subs: map[*poolLease]struct{}{}}
		p.entries[key] = e
	}
	lease := &poolLease{entry: e, onTools: onTools}
	e.mu.Lock()
	e.refs++
	e.idleGen++ // cancels a pending idle close
	if e.idleTimer != nil {
		e.idleTimer.Stop()
		e.idleTimer = nil
	}
	e.subs[lease] = struct{}{}
	p.log.Info("mcp pool acquire", e.logAttrs("result", result)...)
	e.mu.Unlock()
	return lease, nil
}

// release drops one reference. It never closes the client directly unless
// the idle delay is zero.
func (p *Pool) release(lease *poolLease) {
	e := lease.entry
	p.mu.Lock()
	e.mu.Lock()
	if _, ok := e.subs[lease]; !ok {
		e.mu.Unlock()
		p.mu.Unlock()
		return
	}
	delete(e.subs, lease)
	e.refs--
	p.log.Info("mcp pool release", e.logAttrs()...)
	stop := e.idleLocked()
	e.mu.Unlock()
	p.mu.Unlock()
	stop()
}

// idleLocked handles refs == 0: with a zero idle delay the entry is retired
// now (the returned func closes it outside the locks); otherwise a timer is
// armed. Callers hold p.mu and e.mu.
func (e *poolEntry) idleLocked() func() {
	if e.refs > 0 || e.closed {
		return func() {}
	}
	if e.p.idle == 0 {
		return e.retireLocked("idle")
	}
	e.idleGen++
	gen := e.idleGen
	e.idleTimer = time.AfterFunc(e.p.idle, func() {
		e.p.mu.Lock()
		e.mu.Lock()
		var stop func()
		if gen == e.idleGen && e.refs == 0 && !e.closed {
			stop = e.retireLocked("idle")
		}
		e.mu.Unlock()
		e.p.mu.Unlock()
		if stop != nil {
			stop()
		}
	})
	return func() {}
}

// retireLocked removes the entry from the pool and returns a func that
// closes its process. Callers hold p.mu and e.mu.
func (e *poolEntry) retireLocked(reason string) func() {
	e.closed = true
	if e.p.entries[e.key] == e {
		delete(e.p.entries, e.key)
	}
	if e.idleTimer != nil {
		e.idleTimer.Stop()
		e.idleTimer = nil
	}
	client, refresher := e.client, e.refresher
	e.client, e.refresher = nil, nil
	e.p.log.Info("mcp pool stop", e.logAttrs("reason", reason)...)
	return func() {
		if refresher != nil {
			refresher.stop()
		}
		if client != nil {
			_ = client.Close()
		}
	}
}

// live returns the current client when its connection is still usable.
func (e *poolEntry) liveLocked() *Client {
	if e.client == nil {
		return nil
	}
	select {
	case <-e.client.Done():
		return nil
	default:
		return e.client
	}
}

// connect returns the shared client and its raw tools, starting (or
// restarting after a crash) the process once for all concurrent callers.
// The connect runs on the pool's context; its deadline is the largest
// timeout among the callers waiting for it. Each caller waits only as long
// as its own ctx allows. Failures are returned to the callers of that
// attempt and not cached.
func (e *poolEntry) connect(ctx context.Context, timeout time.Duration) (*Client, []Tool, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, nil, errPoolClosed
	}
	if c := e.liveLocked(); c != nil {
		tools := append([]Tool(nil), e.tools...)
		e.mu.Unlock()
		return c, tools, nil
	}
	f := e.flight
	if f != nil {
		f.extend(timeout)
	} else {
		f = newPoolFlight(e.p.ctx, timeout)
		e.flight = f
		stale, staleRefresher := e.client, e.refresher
		e.client, e.refresher = nil, nil
		e.p.log.Info("mcp pool start", e.logAttrs("restart", stale != nil)...)
		go func() {
			if staleRefresher != nil {
				staleRefresher.stop()
			}
			if stale != nil {
				_ = stale.Close() // reap the exited process
			}
			e.dial(f)
		}()
	}
	e.mu.Unlock()
	select {
	case <-f.done:
		if f.err != nil {
			return nil, nil, f.err
		}
		return f.client, append([]Tool(nil), f.tools...), nil
	case <-ctx.Done():
		err := ctx.Err()
		if errors.Is(err, context.DeadlineExceeded) {
			err = timeoutError(timeout, err)
		}
		return nil, nil, err
	}
}

func (e *poolEntry) dial(f *poolFlight) {
	defer close(f.done)
	defer f.cancel()
	defer f.timer.Stop()
	ctx := f.ctx
	refresher := newToolRefresher()
	client, err := connectWith(ctx, e.name, e.cfg, e.rootsCWD, e.info, nil, refresher.notify, nil)
	var tools []Tool
	if err == nil {
		tools, err = client.ListTools(ctx)
		if err != nil {
			_ = closeStartupTransport(client.t)
		}
	}
	if err != nil {
		if hit, d := f.expired(); hit && e.p.ctx.Err() == nil {
			err = timeoutError(d, err)
		}
	}
	e.p.mu.Lock()
	e.mu.Lock()
	e.flight = nil
	if err == nil && e.closed {
		err = errPoolClosed
		_ = closeStartupTransport(client.t)
	}
	if err != nil {
		e.mu.Unlock()
		e.p.mu.Unlock()
		refresher.stop()
		f.err = err
		return
	}
	e.client, e.tools, e.refresher = client, tools, refresher
	f.client, f.tools = client, tools
	// Everyone left while the process started: apply the idle policy.
	stop := e.idleLocked()
	e.mu.Unlock()
	e.p.mu.Unlock()
	stop()
	go func() {
		<-client.Done()
		refresher.stop()
		// Keep the exited client: the next connect closes (reaps) it before
		// the single shared restart.
		e.mu.Lock()
		if e.client == client {
			e.p.log.Info("mcp pool exited", e.logAttrs()...)
		}
		e.mu.Unlock()
	}()
	refresher.start(func(ctx context.Context) { e.relist(ctx, client) })
}

// relist re-reads the raw tool list after tools/list_changed and fans it
// out to every lease. Only the entry's refresher calls it.
func (e *poolEntry) relist(ctx context.Context, client *Client) {
	ctx, cancel := withTimeout(ctx, e.cfg.StartupTimeout())
	defer cancel()
	tools, err := client.ListTools(ctx)
	if err != nil {
		return
	}
	e.mu.Lock()
	if e.client != client {
		e.mu.Unlock()
		return
	}
	e.tools = tools
	subs := make([]*poolLease, 0, len(e.subs))
	for l := range e.subs {
		subs = append(subs, l)
	}
	e.mu.Unlock()
	for _, l := range subs {
		l.deliver(append([]Tool(nil), tools...))
	}
}

// Close stops every shared process regardless of references. Later
// connects fail with "pool closed". It is idempotent.
func (p *Pool) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.cancel()
	var stops []func()
	for _, e := range p.entries {
		e.mu.Lock()
		stops = append(stops, e.retireLocked("pool closed"))
		e.mu.Unlock()
	}
	p.mu.Unlock()
	var wg sync.WaitGroup
	for _, stop := range stops {
		wg.Add(1)
		go func() { defer wg.Done(); stop() }()
	}
	wg.Wait()
	return nil
}
