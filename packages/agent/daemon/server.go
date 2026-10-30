package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/bnema/zut/packages/agent/internal/flock"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

// ErrNotController reports a mutating port call from a client that does not
// control a session another client controls (D5).
var ErrNotController = errors.New("session is controlled by another client")

// DefaultSocketPath resolves the daemon socket: ZUT_DAEMON_SOCKET, then
// Linux $XDG_RUNTIME_DIR/zut/daemon.sock, Windows %LOCALAPPDATA%\zut\run,
// otherwise home/run/daemon.sock.
func DefaultSocketPath(home string) string {
	if p := os.Getenv("ZUT_DAEMON_SOCKET"); p != "" {
		return p
	}
	switch runtime.GOOS {
	case "linux":
		if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
			return filepath.Join(xdg, "zut", "daemon.sock")
		}
	case "windows":
		if la := os.Getenv("LOCALAPPDATA"); la != "" {
			return filepath.Join(la, "zut", "run", "daemon.sock")
		}
	}
	return filepath.Join(home, "run", "daemon.sock")
}

// socketListener is a listening daemon socket guarded by `<socket>.lock`.
type socketListener struct {
	net.Listener
	path string
	lock *flock.Lock
	info os.FileInfo // identity recorded right after Listen
}

// listenSocket prepares the socket dir (see prepareSocketDir), takes the
// socket lock, and only then removes a stale socket, listens and chmods the
// socket 0600. A busy socket lock means another daemon (possibly with a
// different ZUT_HOME) serves this path.
func listenSocket(path string, zutOwnedDirs []string) (*socketListener, error) {
	if err := prepareSocketDir(filepath.Dir(path), zutOwnedDirs); err != nil {
		return nil, err
	}
	lock, err := flock.TryLock(path + ".lock")
	if errors.Is(err, flock.ErrBusy) {
		return nil, fmt.Errorf("%w: socket %s is in use", ErrAlreadyRunning, path)
	}
	if err != nil {
		return nil, fmt.Errorf("lock daemon socket: %w", err)
	}
	fail := func(err error) (*socketListener, error) { _ = lock.Close(); return nil, err }
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(fmt.Errorf("remove stale socket: %w", err))
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fail(fmt.Errorf("listen daemon socket: %w", err))
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		// Removal is done by close, only if the path is still our socket.
		ul.SetUnlinkOnClose(false)
	}
	info, err := os.Lstat(path)
	if err != nil {
		_ = ln.Close()
		return fail(fmt.Errorf("stat daemon socket: %w", err))
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o600); err != nil {
			_ = ln.Close()
			return fail(fmt.Errorf("chmod daemon socket: %w", err))
		}
	}
	return &socketListener{Listener: ln, path: path, lock: lock, info: info}, nil
}

// close stops listening, removes the socket only if the path still refers to
// the file created by this listener, then releases the socket lock.
func (l *socketListener) close() {
	_ = l.Close()
	if cur, err := os.Lstat(l.path); err == nil && os.SameFile(cur, l.info) {
		_ = os.Remove(l.path)
	}
	_ = l.lock.Close()
}

// prepareSocketDir creates dir 0700 when missing. An existing directory must
// not be a symlink and, on Unix, must be owned by the current user; a
// zut-owned default dir is tightened to 0700, any other dir with group/other
// access is refused. On Windows, a created or zut-owned dir gets a protected
// current-user DACL; any other dir is only verified (owner and DACL).
func prepareSocketDir(dir string, zutOwnedDirs []string) error {
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		err = createSocketDir(dir)
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		// Another daemon created it first: apply the existing-dir checks.
		if fi, err = os.Lstat(dir); err != nil {
			return fmt.Errorf("stat socket dir: %w", err)
		}
	}
	if err != nil {
		return fmt.Errorf("stat socket dir: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("socket dir %s is not a directory", dir)
	}
	owned := isZutOwnedDir(dir, zutOwnedDirs)
	if runtime.GOOS == "windows" {
		return secureSocketDir(dir, owned)
	}
	if uid, ok := fileOwner(fi); ok && uid != os.Getuid() {
		return fmt.Errorf("socket dir %s is not owned by the current user", dir)
	}
	if fi.Mode().Perm()&0o077 == 0 {
		return nil
	}
	if owned {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("chmod socket dir: %w", err)
		}
		return nil
	}
	return fmt.Errorf("socket dir %s is accessible by group or others (mode %v)", dir, fi.Mode().Perm())
}

// createSocketDir creates missing parents 0700, then creates dir itself
// private from the start (0700, or a protected current-user DACL on Windows),
// verifies it, and refuses it if anything appeared inside meanwhile.
func createSocketDir(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return fmt.Errorf("create socket dir parent: %w", err)
	}
	if err := mkdirPrivate(dir); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("chmod socket dir: %w", err)
		}
	}
	if err := secureSocketDir(dir, true); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read socket dir: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("newly created socket dir %s is not empty", dir)
	}
	return nil
}

// isZutOwnedDir reports whether dir is one of the zut-owned socket dirs.
func isZutOwnedDir(dir string, zutOwnedDirs []string) bool {
	clean := filepath.Clean(dir)
	for _, owned := range zutOwnedDirs {
		if owned != "" && filepath.Clean(owned) == clean {
			return true
		}
	}
	return false
}

// zutOwnedSocketDirs lists the default socket directories that zut owns.
func zutOwnedSocketDirs(home string) []string {
	dirs := []string{filepath.Join(home, "run")}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		dirs = append(dirs, filepath.Join(xdg, "zut"))
	}
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		dirs = append(dirs, filepath.Join(la, "zut", "run"))
	}
	return dirs
}

// client is one connected peer. One writer goroutine drains two bounded
// queues: control (closed, detached_by_other; cap controlQueueSize) is always
// preferred, outbox carries session events. Event forwarders block on outbox
// (session rings drop slow consumers); control notifications never block and
// only an overflowing control queue disconnects the client.
type client struct {
	id      uint64
	d       *Daemon
	conn    *daemonproto.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	outbox  chan any
	control chan any

	mu   sync.Mutex
	subs map[session.ID]context.CancelFunc
	// kind is the hello client_kind (tui, headless, cli, bot), for logs.
	kind string
}

func (c *client) clientKind() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.kind == "" {
		return "unknown"
	}
	return c.kind
}

// logLifecycle records a successful client lifecycle operation with IDs and
// kinds only.
func (c *client) logLifecycle(op string, id session.ID, attrs ...any) {
	c.d.log.Info("client "+op, append([]any{"client", c.id, "client_kind", c.clientKind(), "session", id}, attrs...)...)
}

func (d *Daemon) newClient(nc net.Conn) *client {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopping {
		return nil
	}
	d.nextClient++
	ctx, cancel := context.WithCancel(d.ctx)
	c := &client{id: d.nextClient, d: d, ctx: ctx, cancel: cancel, outbox: make(chan any, clientOutboxSize), control: make(chan any, controlQueueSize), subs: map[session.ID]context.CancelFunc{}}
	c.conn = daemonproto.NewConn(nc, daemonproto.Options{OnRequest: c.handle})
	d.clients[c.id] = c
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		c.writeLoop()
	}()
	return c
}

func (c *client) close() {
	c.cancel()
	_ = c.conn.Close()
}

func (c *client) writeLoop() {
	for {
		var n any
		select {
		case n = <-c.control:
		default:
			select {
			case <-c.ctx.Done():
				return
			case <-c.conn.Done():
				c.cancel()
				return
			case n = <-c.control:
			case n = <-c.outbox:
			}
		}
		if err := c.conn.Notify(n); err != nil {
			c.close()
			return
		}
	}
}

// enqueue queues a daemon-originated control notification without blocking.
func (c *client) enqueue(n any) {
	select {
	case c.control <- n:
	case <-c.ctx.Done():
	default:
		c.d.log.Warn("client control queue full; disconnecting", "client", c.id)
		c.close()
	}
}

// subscribe replaces any subscription of c to e with one after afterSeq.
func (c *client) subscribe(e *entry, afterSeq uint64) error {
	c.cancelSub(e.id)
	ctx, cancel := context.WithCancel(c.ctx)
	ch, err := e.h.Subscribe(ctx, afterSeq)
	if err != nil {
		cancel()
		return err
	}
	c.mu.Lock()
	c.subs[e.id] = cancel
	c.mu.Unlock()
	c.d.wg.Add(1)
	go func() {
		defer c.d.wg.Done()
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				if ev.Session == "" {
					ev.Session = e.id
				}
				n, err := daemonproto.EncodeEvent(ev)
				if err != nil {
					c.d.log.Warn("encode event", "session", e.id, "kind", ev.Kind, "err", err)
					continue
				}
				select {
				case c.outbox <- n:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return nil
}

func (c *client) cancelSub(id session.ID) {
	c.mu.Lock()
	cancel := c.subs[id]
	delete(c.subs, id)
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func decodeFrame[T any](r daemonproto.Request) (T, error) {
	var v T
	if err := json.Unmarshal(r.Raw, &v); err != nil {
		return v, fmt.Errorf("decode %s request: %w", r.Type, err)
	}
	return v, nil
}

func (c *client) handle(ctx context.Context, r daemonproto.Request) (any, error) {
	d := c.d
	switch r.Type {
	case "hello":
		req, err := decodeFrame[daemonproto.HelloRequest](r)
		if err != nil {
			return nil, err
		}
		if req.ProtocolVersion != daemonproto.ProtocolVersion {
			return nil, fmt.Errorf("protocol version %d unsupported (daemon speaks %d)", req.ProtocolVersion, daemonproto.ProtocolVersion)
		}
		c.mu.Lock()
		c.kind = req.ClientKind
		c.mu.Unlock()
		return daemonproto.HelloData{ProtocolVersion: daemonproto.ProtocolVersion, DaemonVersion: d.opts.Version}, nil
	case "create":
		req, err := decodeFrame[daemonproto.CreateRequest](r)
		if err != nil {
			return nil, err
		}
		info, err := d.create(req.Spec)
		if err == nil {
			c.logLifecycle("create", session.ID(info.ID), "headless", req.Spec.Headless)
		}
		return info, err
	case "attach":
		req, err := decodeFrame[daemonproto.AttachRequest](r)
		if err != nil {
			return nil, err
		}
		info, err := d.attach(c, req.Session, req.AfterSeq, req.Controlling)
		if err == nil {
			c.logLifecycle("attach", req.Session, "controlling", req.Controlling)
		}
		return info, err
	case "detach":
		err := d.detach(c, r.Session, ReasonClientDetached)
		if err == nil {
			c.logLifecycle("detach", r.Session)
		}
		return nil, err
	case "close":
		req, err := decodeFrame[daemonproto.CloseRequest](r)
		if err != nil {
			return nil, err
		}
		err = d.closeSession(c, req.Session, req.Reason)
		if err == nil {
			c.logLifecycle("kill", req.Session)
		}
		return nil, err
	case "clear":
		info, err := d.clear(c, r.Session)
		if err == nil {
			c.logLifecycle("clear", r.Session, "successor", info.ID)
		}
		return info, err
	case "list":
		return d.List(ctx), nil
	case "tree":
		return d.Tree(ctx), nil
	case "snapshot":
		e := d.lookup(r.Session)
		if e == nil {
			return nil, ErrUnknownSession
		}
		return e.h.Snapshot(ctx)
	case "port":
		req, err := decodeFrame[daemonproto.PortRequest](r)
		if err != nil {
			return nil, err
		}
		return c.port(ctx, req)
	case "daemon_status":
		d.mu.Lock()
		n := len(d.reg)
		started := d.started
		d.mu.Unlock()
		return daemonproto.DaemonStatusData{PID: os.Getpid(), Version: d.opts.Version, UptimeMS: time.Since(started).Milliseconds(), Sessions: n}, nil
	case "daemon_stop":
		req, err := decodeFrame[daemonproto.DaemonStopRequest](r)
		if err != nil {
			return nil, err
		}
		d.requestStop(time.Duration(req.GracefulMS) * time.Millisecond)
		return nil, nil
	}
	if v, err, ok := c.handleBot(ctx, r); ok {
		return v, err
	}
	return nil, fmt.Errorf("unsupported request type %q", r.Type)
}

// requestStop starts shutdown. The result may race with connection teardown;
// clients treat a closed connection after daemon_stop as success.
func (d *Daemon) requestStop(graceful time.Duration) {
	if graceful > 0 {
		d.stopOverride.Store(int64(graceful))
	}
	d.Stop()
}

// checkControl fails when another client controls id.
func (c *client) checkControl(id session.ID) error {
	d := c.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.reg[id] == nil {
		return ErrUnknownSession
	}
	if ctrl := d.controller[id]; ctrl != nil && ctrl != c {
		return ErrNotController
	}
	return nil
}

type portParams struct {
	AfterSeq     uint64                    `json:"after_seq"`
	In           session.Input             `json:"in"`
	ID           string                    `json:"id"`
	ReqID        string                    `json:"req_id"`
	Decision     core.ConfirmDecision      `json:"decision"`
	Answers      []tools.QuestionAnswer    `json:"answers"`
	Action       string                    `json:"action"`
	ProviderName string                    `json:"provider_name"`
	Model        string                    `json:"model"`
	Level        string                    `json:"level"`
	Enabled      bool                      `json:"enabled"`
	N            int                       `json:"n"`
	Patch        session.SettingsPatch     `json:"patch"`
	Command      json.RawMessage           `json:"command"`
	Path         string                    `json:"path"`
	Title        string                    `json:"title"`
	Name         string                    `json:"name"`
	Global       bool                      `json:"global"`
	Args         string                    `json:"args"`
	Ext          string                    `json:"ext"`
	PanelID      string                    `json:"panel_id"`
	Key          extproto.PanelKeyFromHost `json:"key"`
}

// port dispatches a snake_case Port method (docs/daemon-protocol.md). Read
// methods are always allowed; mutations require that no other client
// controls the session. "subscribe" is a non-controlling attach.
func (c *client) port(ctx context.Context, req daemonproto.PortRequest) (any, error) {
	var p portParams
	if len(req.Params) > 0 && string(req.Params) != "null" {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("decode %s params: %w", req.Method, err)
		}
	}
	e := c.d.lookup(req.Session)
	if e == nil {
		return nil, ErrUnknownSession
	}
	h := e.h
	switch req.Method {
	case "info":
		return h.Info(), nil
	case "snapshot":
		return h.Snapshot(ctx)
	case "sessions_query":
		return h.SessionsQuery(ctx)
	case "subscribe":
		_, err := c.d.attach(c, req.Session, p.AfterSeq, false)
		return nil, err
	}
	if err := c.checkControl(req.Session); err != nil {
		return nil, err
	}
	switch req.Method {
	case "submit":
		// Clients always submit as the user; inter_session origin and a
		// sender are reserved for Directory.Send (D10 loop guard).
		in := p.In
		in.Origin, in.From = session.InputOriginUser, nil
		return nil, h.Submit(ctx, in)
	case "abort":
		return nil, h.Abort(ctx)
	case "compact":
		return nil, h.Compact(ctx)
	case "cancel_queued":
		return nil, h.CancelQueued(ctx, p.ID)
	case "slide_back_queued":
		return h.SlideBackQueued(ctx)
	case "answer_confirmation":
		return nil, h.AnswerConfirmation(ctx, p.ReqID, p.Decision)
	case "answer_question":
		return nil, h.AnswerQuestion(ctx, p.ReqID, p.Answers)
	case "resolve_deferred":
		return nil, h.ResolveDeferred(ctx, p.ID, p.Action)
	case "set_model":
		return nil, h.SetModel(ctx, p.ProviderName, p.Model)
	case "set_reasoning":
		return nil, h.SetReasoning(ctx, p.Level)
	case "set_fast_mode":
		return nil, h.SetFastMode(ctx, p.Enabled)
	case "apply_profile":
		return nil, h.ApplyProfile(ctx, p.N)
	case "apply_settings":
		return nil, h.ApplySettings(ctx, p.Patch)
	case "set_goal":
		var g session.GoalCommand
		if err := unmarshalOptional(p.Command, &g); err != nil {
			return nil, err
		}
		return nil, h.SetGoal(ctx, g)
	case "change_cwd":
		return nil, h.ChangeCWD(ctx, p.Path)
	case "set_jail":
		return nil, h.SetJail(ctx, p.Enabled)
	case "set_title":
		return nil, h.SetTitle(ctx, p.Title)
	case "toggle_skill_pin":
		return nil, h.ToggleSkillPin(ctx, p.Name, p.Global)
	case "reload_extensions":
		return nil, h.ReloadExtensions(ctx)
	case "invoke_extension_command":
		return nil, h.InvokeExtensionCommand(ctx, p.Name, p.Args)
	case "extension_panel_input":
		return nil, h.ExtensionPanelInput(ctx, p.Ext, p.PanelID, p.Key)
	case "reload_credentials":
		return nil, h.ReloadCredentials(ctx)
	case "resident_action":
		var rc session.ResidentCommand
		if err := unmarshalOptional(p.Command, &rc); err != nil {
			return nil, err
		}
		return nil, h.ResidentAction(ctx, rc)
	}
	return nil, fmt.Errorf("unsupported port method %q", req.Method)
}

func unmarshalOptional(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("decode command: %w", err)
	}
	return nil
}
