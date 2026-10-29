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

// listenSocket creates the 0700 socket dir, removes a stale socket and
// listens with mode 0600. Callers must hold the daemon lock.
func listenSocket(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create socket dir: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("chmod socket dir: %w", err)
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen daemon socket: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o600); err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("chmod daemon socket: %w", err)
		}
	}
	return ln, nil
}

// client is one connected peer. Notifications go through a bounded outbox
// drained by one writer goroutine; event forwarders block on it (session
// rings drop slow consumers), daemon-originated notifications never block and
// disconnect the client when the outbox is full.
type client struct {
	id     uint64
	d      *Daemon
	conn   *daemonproto.Conn
	ctx    context.Context
	cancel context.CancelFunc
	outbox chan any

	mu   sync.Mutex
	subs map[session.ID]context.CancelFunc
}

func (d *Daemon) newClient(nc net.Conn) *client {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopping {
		return nil
	}
	d.nextClient++
	ctx, cancel := context.WithCancel(d.ctx)
	c := &client{id: d.nextClient, d: d, ctx: ctx, cancel: cancel, outbox: make(chan any, clientOutboxSize), subs: map[session.ID]context.CancelFunc{}}
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
		select {
		case <-c.ctx.Done():
			return
		case <-c.conn.Done():
			c.cancel()
			return
		case n := <-c.outbox:
			if err := c.conn.Notify(n); err != nil {
				c.close()
				return
			}
		}
	}
}

// enqueue queues a daemon-originated notification without blocking.
func (c *client) enqueue(n any) {
	select {
	case c.outbox <- n:
	case <-c.ctx.Done():
	default:
		c.d.log.Warn("client outbox full; disconnecting", "client", c.id)
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
		return daemonproto.HelloData{ProtocolVersion: daemonproto.ProtocolVersion, DaemonVersion: d.opts.Version}, nil
	case "create":
		req, err := decodeFrame[daemonproto.CreateRequest](r)
		if err != nil {
			return nil, err
		}
		return d.create(req.Spec)
	case "attach":
		req, err := decodeFrame[daemonproto.AttachRequest](r)
		if err != nil {
			return nil, err
		}
		return d.attach(c, req.Session, req.AfterSeq, req.Controlling)
	case "detach":
		return nil, d.detach(c, r.Session, ReasonClientDetached)
	case "close":
		req, err := decodeFrame[daemonproto.CloseRequest](r)
		if err != nil {
			return nil, err
		}
		return nil, d.closeSession(req.Session, req.Reason)
	case "clear":
		if err := c.checkControl(r.Session); err != nil {
			return nil, err
		}
		return d.clear(r.Session)
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
		return nil, h.Submit(ctx, p.In)
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
