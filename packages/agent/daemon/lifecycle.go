package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/agent/internal/flock"
	"github.com/bnema/zut/packages/agent/session"
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
	// Ready, when non-nil, is closed once startup cleanup finishes and the socket listens.
	Ready chan<- struct{}
}

var errRunTwice = errors.New("daemon: Run called twice")

// Run takes the daemon lock (ErrAlreadyRunning if held), writes the PID
// file, removes stale daemon session metadata, and listens with an empty registry.
// It serves until ctx ends or a client sends daemon_stop. Shutdown stops
// accepting, detaches clients, closes sessions, and releases all locks.
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

	d.cleanupPreviousSessions()
	d.goLoop(d.gcLoop)
	d.goLoop(func(ctx context.Context) { d.acceptLoop(ctx, ln) })
	if opts.Ready != nil {
		close(opts.Ready)
	}
	<-runCtx.Done()
	_ = ln.Close()
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

// cleanupPreviousSessions removes metadata owned only by previous daemon sessions.
// Transcripts and schedules live elsewhere and remain untouched.
func (d *Daemon) cleanupPreviousSessions() {
	if err := os.Remove(statePath(d.opts.Home)); err != nil && !errors.Is(err, os.ErrNotExist) {
		d.log.Warn("remove previous daemon state", "err", err)
	}
	if err := os.RemoveAll(filepath.Join(stateDir(d.opts.Home), "sessions")); err != nil {
		d.log.Warn("remove previous daemon session directories", "err", err)
	}
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
	spec, err := d.forkSpec(spec)
	if err != nil {
		return session.Info{}, err
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
	if err := d.register(h, session.CreateSpec{CWD: cwd, Env: spec.Env, Args: spec.Args, Headless: spec.Headless, Zutfile: spec.Zutfile.WithoutPre()}, lock); err != nil {
		return session.Info{}, err
	}
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
func (d *Daemon) register(h session.Handle, s session.CreateSpec, lock *flock.Lock) error {
	id := session.ID(h.Info().ID)
	d.mu.Lock()
	if id == "" || d.reg[id] != nil {
		d.mu.Unlock()
		_ = lock.Close()
		d.closeHandle(h, "duplicate_id")
		return fmt.Errorf("session id %q is empty or already registered", id)
	}
	e := &entry{parentPath: normParentPath(h.Info().SessionPath), id: id, h: h, spec: s, lock: lock, detachedAt: d.now()}
	d.reg[id] = e
	d.mu.Unlock()
	d.goLoop(func(ctx context.Context) { d.watch(ctx, e) })
	return nil
}

// watch unregisters a session when it ends on its own.
func (d *Daemon) watch(ctx context.Context, e *entry) {
	select {
	case <-ctx.Done():
	case <-e.h.Done():
		d.forget(e)
	}
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

// detach releases control (if held by c) and ends c's subscription. The
// session's SetAttach(Detached) runs synchronously, before the detach request
// is answered, so a client sees the session detached once Detach returns.
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

// closeSession ends id explicitly and removes it from the registry. Control is
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

// forkSpec makes a fork inherit the launch policy of the live session whose
// transcript it branches: cwd, args (--no-yolo, --tools, extensions, sandbox
// and model flags), env and zutfile permissions. A fork only inherits:
// client-supplied args, env and zutfile are replaced, never merged, so a fork
// cannot widen its parent's policy. When no live session owns the parent
// path the policy is unknown and the fork is rejected.
func (d *Daemon) forkSpec(spec session.CreateSpec) (session.CreateSpec, error) {
	if spec.Fork == nil {
		return spec, nil
	}
	if spec.ResumePath != "" {
		return session.CreateSpec{}, fmt.Errorf("%w: fork cannot include resume_path", session.ErrInvalidFork)
	}
	want := normParentPath(spec.Fork.ParentPath)
	var parent *entry
	var info session.Info
	for _, e := range d.entries() {
		i := e.h.Info()
		if e.parentPath != "" && e.parentPath == want {
			parent, info = e, i
			break
		}
	}
	if parent == nil {
		return session.CreateSpec{}, session.ErrForkParentNotFound
	}
	fork := *spec.Fork
	fork.ParentPath = parent.parentPath
	fork.ParentFile, _ = os.Stat(parent.parentPath)
	if fork.ParentFile == nil {
		return session.CreateSpec{}, fmt.Errorf("%w: parent transcript unavailable", session.ErrInvalidFork)
	}
	spec.Fork = &fork
	spec.CWD = info.CWD
	if spec.CWD == "" {
		spec.CWD = parent.spec.CWD
	}
	spec.Args = clearArgs(parent.spec.Args, info)
	spec.Env = append([]string(nil), parent.spec.Env...)
	spec.Zutfile = parent.spec.Zutfile.WithoutPre()
	return spec, nil
}

// normParentPath resolves symlinks when the path exists and always cleans it.
func normParentPath(p string) string {
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		if cwd, err := os.Getwd(); err == nil {
			p = cwd + string(filepath.Separator) + p
		}
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
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
		if uid, err := checkPeerUID(nc, d.peerUID); err != nil {
			_ = nc.Close()
			d.log.Warn("reject socket peer", "uid", uid)
			continue
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
