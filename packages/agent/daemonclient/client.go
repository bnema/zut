// Package daemonclient connects front-ends to the local zut daemon: dialing
// with the hello/version handshake, auto-spawning the daemon, lifecycle
// requests, and RemotePort, a session.Port over the "port" request.
package daemonclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/agent/session"
)

// Options configures the hello handshake and client diagnostics.
type Options struct {
	// ClientVersion is the zut binary version sent in hello.
	ClientVersion string
	// ClientKind is tui, headless, cli or bot (default cli).
	ClientKind string
	// PID is sent in hello (default os.Getpid()).
	PID int
	// Warnings receives non-fatal diagnostics such as daemon version skew.
	// Nil discards them; the library never writes to os.Stderr itself.
	Warnings io.Writer
}

// Client is one logical daemon connection. After transport loss the next
// request (or a RemotePort subscription) redials the same socket.
type Client struct {
	path string
	opts Options

	dialMu   sync.Mutex // serializes connects; never held by the read loop
	mu       sync.Mutex // guards the fields below; never held across I/O
	cur      *daemonproto.Conn
	hello    daemonproto.HelloData
	warned   bool
	subs     map[session.ID]*subscription
	closed   bool
	ctx      context.Context
	cancel   context.CancelFunc
	routines sync.WaitGroup
}

// Dial connects to socketPath and performs hello. A protocol version mismatch
// is an error; a daemon binary version different from opts.ClientVersion
// writes `daemon is running zut <v>; run "zut daemon restart"` to
// opts.Warnings and continues.
func Dial(ctx context.Context, socketPath string, opts Options) (*Client, error) {
	if opts.ClientKind == "" {
		opts.ClientKind = "cli"
	}
	if opts.PID == 0 {
		opts.PID = os.Getpid()
	}
	if opts.Warnings == nil {
		opts.Warnings = io.Discard
	}
	c := &Client{path: socketPath, opts: opts, subs: make(map[session.ID]*subscription)}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	if _, err := c.conn(ctx); err != nil {
		c.cancel()
		c.routines.Wait()
		return nil, err
	}
	return c, nil
}

// connect dials, starts the read loop and runs hello. Caller holds c.dialMu
// (not c.mu: the read loop needs c.mu to route notifications).
func (c *Client) connect(ctx context.Context) (*daemonproto.Conn, error) {
	if err := checkSocketPath(c.path); err != nil {
		return nil, err
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "unix", c.path)
	if err != nil {
		return nil, fmt.Errorf("dial zut daemon %s: %w", c.path, err)
	}
	conn := daemonproto.NewConn(nc, daemonproto.Options{OnNotification: c.onNotification})
	c.routines.Add(1)
	go func() {
		defer c.routines.Done()
		_ = conn.Serve(c.ctx)
	}()
	var h daemonproto.HelloData
	err = conn.Call(ctx, daemonproto.HelloRequest{Type: "hello", ProtocolVersion: daemonproto.ProtocolVersion, ClientVersion: c.opts.ClientVersion, ClientKind: c.opts.ClientKind, PID: c.opts.PID}, &h)
	if err == nil && h.ProtocolVersion != daemonproto.ProtocolVersion {
		err = fmt.Errorf("zut daemon speaks protocol %d, client requires %d; run \"zut daemon restart\"", h.ProtocolVersion, daemonproto.ProtocolVersion)
	} else if err != nil {
		err = fmt.Errorf("zut daemon hello: %w", err)
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	c.mu.Lock()
	closed := c.closed
	warn := h.DaemonVersion != c.opts.ClientVersion && !c.warned
	if !closed {
		c.cur, c.hello = conn, h
		c.warned = c.warned || warn
	}
	c.mu.Unlock()
	if closed {
		conn.Close()
		return nil, daemonproto.ErrClosed
	}
	if warn {
		fmt.Fprintf(c.opts.Warnings, "daemon is running zut %s; run \"zut daemon restart\"\n", h.DaemonVersion)
	}
	return conn, nil
}

// conn returns the live transport, redialing when the previous one was lost.
func (c *Client) conn(ctx context.Context) (*daemonproto.Conn, error) {
	c.dialMu.Lock()
	defer c.dialMu.Unlock()
	c.mu.Lock()
	cur, closed := c.cur, c.closed
	c.mu.Unlock()
	if closed {
		return nil, daemonproto.ErrClosed
	}
	if cur != nil {
		select {
		case <-cur.Done():
		default:
			return cur, nil
		}
	}
	return c.connect(ctx)
}

// DaemonVersion returns the version reported by the last hello.
func (c *Client) DaemonVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hello.DaemonVersion
}

// Disconnect closes the transport, ends every subscription and waits for the
// client's goroutines. It is idempotent.
func (c *Client) Disconnect() error {
	c.mu.Lock()
	c.closed = true
	cur := c.cur
	subs := c.subs
	c.subs = make(map[session.ID]*subscription)
	c.mu.Unlock()
	c.cancel()
	if cur != nil {
		cur.Close()
	}
	for _, s := range subs {
		s.finish(nil)
	}
	c.routines.Wait()
	return nil
}

// call sends one request on the live transport and maps remote session errors
// to their session sentinels.
func (c *Client) call(ctx context.Context, frame, out any) error {
	conn, err := c.conn(ctx)
	if err != nil {
		return err
	}
	return mapErr(conn.Call(ctx, frame, out))
}

var sentinels = []error{session.ErrSeqTooOld, session.ErrClosed, session.ErrNotAttached, session.ErrBusy, session.ErrUnknownRequest, session.ErrLoopGuard, session.ErrForkParentNotFound}

func mapErr(err error) error {
	var re *daemonproto.RemoteError
	if !errors.As(err, &re) {
		return err
	}
	for _, s := range sentinels {
		if strings.HasSuffix(re.Msg, s.Error()) {
			prefix := strings.TrimSuffix(strings.TrimSuffix(re.Msg, s.Error()), ": ")
			if prefix == "" {
				return fmt.Errorf("zut daemon: %w", s)
			}
			return fmt.Errorf("zut daemon: %s: %w", prefix, s)
		}
	}
	return fmt.Errorf("zut daemon: %w", err)
}

// Create creates a session from spec.
func (c *Client) Create(ctx context.Context, spec session.CreateSpec) (session.Info, error) {
	var info session.Info
	err := c.call(ctx, daemonproto.CreateRequest{Type: "create", Spec: spec}, &info)
	return info, err
}

// Attach sends a raw attach request. Event notifications are delivered only to
// a RemotePort subscription for id; prefer RemotePort.Subscribe for streaming.
func (c *Client) Attach(ctx context.Context, id session.ID, afterSeq uint64, controlling bool) (session.Info, error) {
	var info session.Info
	err := c.call(ctx, daemonproto.AttachRequest{Type: "attach", Session: id, AfterSeq: afterSeq, Controlling: controlling}, &info)
	return info, err
}

// Detach releases control of id; the session keeps running detached.
func (c *Client) Detach(ctx context.Context, id session.ID) error {
	return c.call(ctx, daemonproto.DetachRequest{Type: "detach", Session: id}, nil)
}

// Close ends session id with reason (for example user_exit).
func (c *Client) Close(ctx context.Context, id session.ID, reason string) error {
	return c.call(ctx, daemonproto.CloseRequest{Type: "close", Session: id, Reason: reason}, nil)
}

// Kill ends session id on behalf of `zut kill`.
func (c *Client) Kill(ctx context.Context, id session.ID) error { return c.Close(ctx, id, "killed") }

// Clear closes id and returns its successor (D9).
func (c *Client) Clear(ctx context.Context, id session.ID) (session.Info, error) {
	var info session.Info
	err := c.call(ctx, daemonproto.ClearRequest{Type: "clear", Session: id}, &info)
	return info, err
}

// TelegramBridge turns the daemon-side Telegram mirror for id on or off.
func (c *Client) TelegramBridge(ctx context.Context, id session.ID, on bool) (daemonproto.TelegramBridgeData, error) {
	var out daemonproto.TelegramBridgeData
	err := c.call(ctx, daemonproto.TelegramBridgeRequest{Type: "telegram_bridge", Session: id, On: on}, &out)
	return out, err
}

// List returns every open session.
func (c *Client) List(ctx context.Context) ([]session.Info, error) {
	var out []session.Info
	err := c.call(ctx, daemonproto.ListRequest{Type: "list"}, &out)
	return out, err
}

// Tree returns sessions grouped by workspace.
func (c *Client) Tree(ctx context.Context) (session.Tree, error) {
	var out session.Tree
	err := c.call(ctx, daemonproto.TreeRequest{Type: "tree"}, &out)
	return out, err
}

// Snapshot returns the full state of session id.
func (c *Client) Snapshot(ctx context.Context, id session.ID) (session.Snapshot, error) {
	var out session.Snapshot
	err := c.call(ctx, daemonproto.SnapshotRequest{Type: "snapshot", Session: id}, &out)
	return out, err
}

// Status returns daemon process status.
func (c *Client) Status(ctx context.Context) (daemonproto.DaemonStatusData, error) {
	var out daemonproto.DaemonStatusData
	err := c.call(ctx, daemonproto.DaemonStatusRequest{Type: "daemon_status"}, &out)
	return out, err
}

// Stop asks the daemon to shut down within graceful. A transport closed by
// the stopping daemon before its result arrives counts as success.
func (c *Client) Stop(ctx context.Context, graceful time.Duration) error {
	err := c.call(ctx, daemonproto.DaemonStopRequest{Type: "daemon_stop", GracefulMS: int(graceful.Milliseconds())}, nil)
	if errors.Is(err, daemonproto.ErrClosed) {
		return nil
	}
	return err
}

// Port returns a session.Port for id. controlling selects whether its
// subscriptions take control of the session (D5).
func (c *Client) Port(id session.ID, controlling bool) *RemotePort {
	return &RemotePort{c: c, id: id, controlling: controlling}
}
