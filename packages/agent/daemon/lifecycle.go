package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/agent/internal/flock"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/core"
)

// ErrUnknownSession reports a request for a session the daemon does not own.
var ErrUnknownSession = errors.New("unknown session")

// ErrStopping reports a request received while the daemon shuts down.
var ErrStopping = errors.New("daemon is stopping")

// Close and attach reasons published through Handle.Close / Handle.SetAttach.
const (
	ReasonCleared            = "cleared"
	ReasonDaemonStop         = "daemon_stop"
	ReasonIdleGC             = "idle_gc"
	ReasonClientDetached     = "client_detached"
	ReasonClientDisconnected = "client_disconnected"
)

const defaultStopTimeout = 10 * time.Second

// Options configures Run. Home and SocketPath are required.
type Options struct {
	// SocketPath is the AF_UNIX listen path (see DefaultSocketPath).
	SocketPath string
	// Home is $ZUT_HOME; daemon metadata lives in Home/daemon.
	Home string
	// IdleGC is the D7 idle duration (daemon.idle_gc_hours); 0 disables GC.
	IdleGC time.Duration
	// GCInterval defaults to DefaultGCInterval.
	GCInterval time.Duration
	// StopTimeout bounds closing all sessions at shutdown (default 10s).
	StopTimeout time.Duration
	// Version is reported by hello and daemon_status.
	Version string
	// Logger defaults to a discarding logger; see OpenLog.
	Logger *slog.Logger
	// Clock defaults to time.Now; it drives GC idle accounting.
	Clock func() time.Time
	// Ready, when non-nil, is closed once the socket listens and restore ran.
	Ready chan<- struct{}
}

var errRunTwice = errors.New("daemon: Run called twice")

// Run takes the daemon lock (ErrAlreadyRunning if held), writes the PID
// file, removes a stale socket, listens, restores sessions from state.json
// idle, and serves until ctx ends or a client sends daemon_stop. Shutdown
// stops accepting, detaches clients, closes sessions with reason
// daemon_stop while keeping them in state.json, and releases all locks.
func (d *Daemon) Run(ctx context.Context, opts Options) error {
	if opts.Home == "" || opts.SocketPath == "" {
		return errors.New("daemon: Home and SocketPath are required")
	}
	if !d.ran.CompareAndSwap(false, true) {
		return errRunTwice
	}
	lock, err := acquireDaemonLock(opts.Home)
	if err != nil {
		return err
	}
	defer lock.Close()
	if opts.GCInterval <= 0 {
		opts.GCInterval = DefaultGCInterval
	}
	if opts.StopTimeout <= 0 {
		opts.StopTimeout = defaultStopTimeout
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	d.mu.Lock()
	d.opts = opts
	if opts.Logger != nil {
		d.log = opts.Logger
	}
	d.ctx, d.stop = runCtx, stop
	d.sessCtx, d.sessCancel = context.WithCancel(context.WithoutCancel(ctx))
	d.started = time.Now()
	d.mu.Unlock()
	defer d.sessCancel()

	if err := writePID(opts.Home); err != nil {
		return err
	}
	defer os.Remove(pidPath(opts.Home))
	ln, err := listenSocket(opts.SocketPath, zutOwnedSocketDirs(opts.Home))
	if err != nil {
		return err
	}
	defer ln.close()
	d.log.Info("daemon started", "pid", os.Getpid(), "version", opts.Version)

	d.restore()
	d.goLoop(d.gcLoop)
	d.goLoop(d.stateLoop)
	d.goLoop(func(ctx context.Context) { d.acceptLoop(ctx, ln) })
	if opts.Ready != nil {
		close(opts.Ready)
	}
	<-runCtx.Done()
	_ = ln.Listener.Close()
	d.shutdown()
	d.log.Info("daemon stopped")
	return nil
}

func (d *Daemon) goLoop(fn func(context.Context)) {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		fn(d.ctx)
	}()
}

// Stop requests a graceful shutdown; Run returns once it completes.
func (d *Daemon) Stop() {
	d.mu.Lock()
	stop := d.stop
	d.mu.Unlock()
	if stop != nil {
		stop()
	}
}

func (d *Daemon) shutdown() {
	d.mu.Lock()
	d.stopping = true
	clients := make([]*client, 0, len(d.clients))
	for _, c := range d.clients {
		clients = append(clients, c)
	}
	d.mu.Unlock()

	d.opMu.Lock()
	// Persist first so turns in flight are recorded and restored interrupted.
	d.persist()
	for _, c := range clients {
		c.close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), d.stopTimeout())
	// Close every session concurrently so the stop timeout bounds the whole
	// shutdown rather than each session in turn.
	var closing sync.WaitGroup
	for _, e := range d.entries() {
		d.mu.Lock()
		controlled := d.controller[e.id] != nil
		delete(d.controller, e.id)
		d.mu.Unlock()
		closing.Add(1)
		go func() {
			defer closing.Done()
			if controlled {
				e.h.SetAttach(ctx, session.AttachStateDetached, ReasonDaemonStop)
			}
			if err := e.h.Close(ctx, ReasonDaemonStop); err != nil {
				d.log.Warn("close session on stop", "session", e.id, "err", err)
			}
			_ = e.lock.Close()
		}()
	}
	closing.Wait()
	cancel()
	d.sessCancel()
	d.opMu.Unlock()
	d.wg.Wait()
}

func (d *Daemon) isStopping() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stopping || d.ctx == nil
}

// restore reopens state.json sessions idle; failures are logged and dropped.
func (d *Daemon) restore() {
	st, err := loadState(d.opts.Home)
	if err != nil {
		d.log.Error("load daemon state", "err", err)
		return
	}
	d.opMu.Lock()
	defer d.opMu.Unlock()
	for _, s := range st.Sessions {
		if err := d.restoreOne(s); err != nil {
			d.log.Warn("restore session dropped", "session", s.ID, "err", err)
		}
	}
	d.persist()
}

func (d *Daemon) restoreOne(s stateSession) error {
	lock, err := lockSessionFile(s.SessionPath)
	if err != nil {
		return err
	}
	h, err := d.factory.Restore(d.sessCtx, session.RestoreSpec{
		ID: s.ID, SessionPath: s.SessionPath, CWD: s.CWD, Env: s.Env, Args: s.Args, InterruptedTurn: s.Running, Zutfile: s.Zutfile.WithoutPre(),
	})
	if err != nil {
		_ = lock.Close()
		return err
	}
	s.Running = false
	return d.register(h, s, lock)
}

// create builds a new session; callers must not hold opMu.
func (d *Daemon) create(spec session.CreateSpec) (session.Info, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	return d.createLocked(spec)
}

func (d *Daemon) createLocked(spec session.CreateSpec) (session.Info, error) {
	if d.isStopping() {
		return session.Info{}, ErrStopping
	}
	pre, err := lockSessionFile(spec.ResumePath)
	if err != nil {
		return session.Info{}, err
	}
	h, err := d.factory.Create(d.sessCtx, spec)
	if err != nil {
		_ = pre.Close()
		return session.Info{}, err
	}
	info := h.Info()
	lock := pre
	if info.SessionPath != "" && info.SessionPath != spec.ResumePath {
		l, err := lockSessionFile(info.SessionPath)
		if err != nil {
			_ = pre.Close()
			d.closeHandle(h, "session_lock_failed")
			return session.Info{}, err
		}
		_ = pre.Close()
		lock = l
	}
	cwd := spec.CWD
	if cwd == "" {
		cwd = info.CWD
	}
	if err := d.register(h, stateSession{ID: session.ID(info.ID), SessionPath: info.SessionPath, CWD: cwd, Env: spec.Env, Args: spec.Args, Headless: spec.Headless, Zutfile: spec.Zutfile.WithoutPre()}, lock); err != nil {
		return session.Info{}, err
	}
	d.persist()
	return h.Info(), nil
}

func (d *Daemon) closeHandle(h session.Handle, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultStopTimeout)
	defer cancel()
	if err := h.Close(ctx, reason); err != nil {
		d.log.Warn("close session", "reason", reason, "err", err)
	}
}

// register adds h to the registry and starts its exit watcher. Callers hold opMu.
func (d *Daemon) register(h session.Handle, s stateSession, lock *flock.Lock) error {
	id := session.ID(h.Info().ID)
	if s.ID == "" {
		s.ID = id
	}
	d.mu.Lock()
	if id == "" || d.reg[id] != nil {
		d.mu.Unlock()
		_ = lock.Close()
		d.closeHandle(h, "duplicate_id")
		return fmt.Errorf("session id %q is empty or already registered", id)
	}
	e := &entry{id: id, h: h, spec: s, lock: lock, detachedAt: d.now()}
	d.reg[id] = e
	d.mu.Unlock()
	d.goLoop(func(ctx context.Context) { d.watch(ctx, e) })
	return nil
}

// watch follows one session: turn start/end events mark state.json dirty, and the session ending on its own unregisters it. The
// event channel is only read, never waited on from the session loop. If the
// subscription is dropped (slow consumer) it resubscribes after the last
// seen sequence; if that is too old the 15s refresh remains the backup.
func (d *Daemon) watch(ctx context.Context, e *entry) {
	done := e.h.Done()
	var seq uint64
	var events <-chan session.Event
	subscribe := func() {
		ch, err := e.h.Subscribe(ctx, seq)
		if err != nil {
			d.log.Debug("session watch subscribe", "session", e.id, "err", err)
			events = nil
			return
		}
		events = ch
	}
	subscribe()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			d.forget(e)
			return
		case ev, ok := <-events:
			if !ok {
				subscribe()
				continue
			}
			seq = ev.Seq
			if runTransitionEvent(ev) {
				d.markRunDirty()
			}
		}
	}
}

// runTransitionEvent reports events the controller publishes when a turn
// starts or finishes (agent turn_start, turn_end, done, error), plus EvInfo.
// stateLoop compares the actual Info().Run with the persisted flag, so
// spurious triggers only cost a comparison.
func runTransitionEvent(ev session.Event) bool {
	switch ev.Kind {
	case session.EvInfo:
		return true
	case session.EvAgent:
		switch ev.Agent.(type) {
		case core.EvTurnStart, *core.EvTurnStart, core.EvTurnEnd, *core.EvTurnEnd,
			core.EvDone, *core.EvDone, core.EvError, *core.EvError:
			return true
		}
	}
	return false
}

// forget drops a session that ended on its own (for example /exit).
func (d *Daemon) forget(e *entry) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	if d.isStopping() {
		return
	}
	d.mu.Lock()
	if d.reg[e.id] != e {
		d.mu.Unlock()
		return
	}
	delete(d.reg, e.id)
	ctrl := d.controller[e.id]
	delete(d.controller, e.id)
	d.mu.Unlock()
	_ = e.lock.Close()
	d.persist()
	if ctrl != nil {
		ctrl.cancelSub(e.id)
		ctrl.enqueue(daemonproto.ClosedNotification{Type: "closed", Session: e.id})
	}
}

// attach subscribes c to id after afterSeq; a controlling attach replaces
// the previous controller, which receives detached_by_other (D5).
func (d *Daemon) attach(c *client, id session.ID, afterSeq uint64, controlling bool) (session.Info, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	if d.isStopping() {
		return session.Info{}, ErrStopping
	}
	e := d.lookup(id)
	if e == nil {
		return session.Info{}, ErrUnknownSession
	}
	if err := c.subscribe(e, afterSeq); err != nil {
		return session.Info{}, err
	}
	if controlling {
		d.takeControl(c, e)
	}
	return e.h.Info(), nil
}

func (d *Daemon) takeControl(c *client, e *entry) {
	d.mu.Lock()
	prev := d.controller[e.id]
	d.controller[e.id] = c
	d.mu.Unlock()
	if prev == c {
		return
	}
	if prev != nil {
		prev.cancelSub(e.id)
		prev.enqueue(daemonproto.DetachedByOtherNotification{Type: "detached_by_other", Session: e.id})
	}
	e.h.SetAttach(context.Background(), session.AttachStateAttached, "")
}

// detach releases control (if held by c) and ends c's subscription.
func (d *Daemon) detach(c *client, id session.ID, reason string) error {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	e := d.lookup(id)
	if e == nil {
		return ErrUnknownSession
	}
	c.cancelSub(id)
	d.releaseControl(c, e, reason)
	return nil
}

func (d *Daemon) releaseControl(c *client, e *entry, reason string) {
	d.mu.Lock()
	if d.controller[e.id] != c {
		d.mu.Unlock()
		return
	}
	delete(d.controller, e.id)
	e.detachedAt = d.now()
	d.mu.Unlock()
	e.h.SetAttach(context.Background(), session.AttachStateDetached, reason)
}

// clientGone detaches every session controlled by a disconnected client.
func (d *Daemon) clientGone(c *client) {
	d.mu.Lock()
	delete(d.clients, c.id)
	d.mu.Unlock()
	d.opMu.Lock()
	defer d.opMu.Unlock()
	if d.isStopping() {
		return
	}
	for _, e := range d.entries() {
		d.releaseControl(c, e, ReasonClientDisconnected)
	}
}

// closeSession ends id explicitly and removes it from state.json. Control is
// checked under opMu so a concurrent takeover cannot slip in between.
func (d *Daemon) closeSession(c *client, id session.ID, reason string) error {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	if err := c.checkControl(id); err != nil {
		return err
	}
	return d.closeLocked(id, reason, "")
}

func (d *Daemon) closeLocked(id session.ID, reason string, successor session.ID) error {
	d.mu.Lock()
	e := d.reg[id]
	if e == nil {
		d.mu.Unlock()
		return ErrUnknownSession
	}
	delete(d.reg, id)
	ctrl := d.controller[id]
	delete(d.controller, id)
	d.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), d.stopTimeout())
	err := e.h.Close(ctx, reason)
	cancel()
	_ = e.lock.Close()
	d.persist()
	if ctrl != nil {
		ctrl.cancelSub(id)
		ctrl.enqueue(daemonproto.ClosedNotification{Type: "closed", Session: id, Successor: successor})
	}
	return err
}

func (d *Daemon) stopTimeout() time.Duration {
	if v := d.stopOverride.Load(); v > 0 {
		return time.Duration(v)
	}
	if d.opts.StopTimeout > 0 {
		return d.opts.StopTimeout
	}
	return defaultStopTimeout
}

// clear implements D9: create a successor with the same launch configuration
// and current provider/model/reasoning, close the old session with reason
// cleared and move the controlling client to the successor. The successor
// identity reaches the client through the closed notification and result.
func (d *Daemon) clear(c *client, id session.ID) (session.Info, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	if err := c.checkControl(id); err != nil {
		return session.Info{}, err
	}
	e := d.lookup(id)
	if e == nil {
		return session.Info{}, ErrUnknownSession
	}
	old := e.h.Info()
	cwd := old.CWD
	if cwd == "" {
		cwd = e.spec.CWD
	}
	spec := session.CreateSpec{CWD: cwd, Env: e.spec.Env, Args: clearArgs(e.spec.Args, old), Headless: e.spec.Headless, Zutfile: e.spec.Zutfile.WithoutPre()}
	info, err := d.createLocked(spec)
	if err != nil {
		return session.Info{}, err
	}
	next := d.lookup(session.ID(info.ID))
	d.mu.Lock()
	ctrl := d.controller[id]
	d.mu.Unlock()
	if err := d.closeLocked(id, ReasonCleared, next.id); err != nil {
		d.log.Warn("close cleared session", "session", id, "err", err)
	}
	if ctrl != nil {
		if err := ctrl.subscribe(next, 0); err != nil {
			d.log.Warn("subscribe successor", "session", next.id, "err", err)
		}
		d.takeControl(ctrl, next)
	}
	return next.h.Info(), nil
}

// clearArgs drops resume/session selection and model flags from the launch
// args and appends the session's current provider/model/reasoning.
func clearArgs(args []string, info session.Info) []string {
	out := make([]string, 0, len(args)+6)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-c", "--continue":
			continue
		case "-r", "--resume":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
			continue
		case "--session", "--provider", "--model", "--reasoning":
			i++
			continue
		case "--":
			out = append(out, args[i:]...)
			i = len(args)
			continue
		}
		out = append(out, a)
	}
	var over []string
	if info.Provider != "" {
		over = append(over, "--provider", info.Provider)
	}
	if info.Model != "" {
		over = append(over, "--model", info.Model)
	}
	if info.Reasoning != "" {
		over = append(over, "--reasoning", info.Reasoning)
	}
	// Overrides go before a "--" terminator so they stay flags.
	for i, a := range out {
		if a == "--" {
			return append(append(append([]string{}, out[:i]...), over...), out[i:]...)
		}
	}
	return append(out, over...)
}

// acceptLoop serves each connection until ctx ends or the listener closes.
func (d *Daemon) acceptLoop(ctx context.Context, ln net.Listener) {
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				d.log.Error("accept", "err", err)
			}
			return
		}
		c := d.newClient(nc)
		if c == nil {
			_ = nc.Close()
			continue
		}
		d.goLoop(func(context.Context) {
			err := c.conn.Serve(c.ctx)
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, daemonproto.ErrClosed) && !errors.Is(err, io.ErrUnexpectedEOF) {
				d.log.Debug("client connection ended", "client", c.id, "err", err)
			}
			c.close()
			d.clientGone(c)
		})
	}
}
