package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/agent/session"
)

// miniDaemon is a minimal real daemon server on daemonproto.Conn used until
// the L10 daemon is merged. It retains events, replays them on attach, and
// answers lifecycle and port requests.
type miniDaemon struct {
	ln       net.Listener
	version  string
	protocol int
	stopped  chan struct{}
	stopOnce sync.Once

	mu        sync.Mutex
	seq       uint64
	floor     uint64 // oldest retained seq is floor+1
	events    []session.Event
	attached  map[*daemonproto.Conn]bool
	conns     map[*daemonproto.Conn]bool
	calls     []daemonproto.PortRequest
	types     []string
	attachErr error
	attaches  []daemonproto.AttachRequest
	onAttach  chan struct{} // optional; signaled after each successful attach
	closeOnce sync.Once
	wg        sync.WaitGroup
}

func startMiniDaemon(path, version string, protocol int) (*miniDaemon, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	d := &miniDaemon{ln: ln, version: version, protocol: protocol, stopped: make(chan struct{}), attached: map[*daemonproto.Conn]bool{}, conns: map[*daemonproto.Conn]bool{}}
	d.wg.Add(1)
	go d.accept()
	return d, nil
}

func (d *miniDaemon) accept() {
	defer d.wg.Done()
	for {
		nc, err := d.ln.Accept()
		if err != nil {
			return
		}
		var conn *daemonproto.Conn
		conn = daemonproto.NewConn(nc, daemonproto.Options{OnRequest: func(ctx context.Context, r daemonproto.Request) (any, error) {
			return d.handle(conn, r)
		}})
		d.mu.Lock()
		d.conns[conn] = true
		d.mu.Unlock()
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			_ = conn.Serve(context.Background())
			d.mu.Lock()
			delete(d.conns, conn)
			delete(d.attached, conn)
			d.mu.Unlock()
		}()
	}
}

func (d *miniDaemon) handle(conn *daemonproto.Conn, r daemonproto.Request) (any, error) {
	d.mu.Lock()
	d.types = append(d.types, r.Type)
	d.mu.Unlock()
	switch r.Type {
	case "hello":
		return daemonproto.HelloData{ProtocolVersion: d.protocol, DaemonVersion: d.version}, nil
	case "attach":
		var a daemonproto.AttachRequest
		if err := json.Unmarshal(r.Raw, &a); err != nil {
			return nil, err
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.attachErr != nil {
			return nil, d.attachErr
		}
		if a.AfterSeq < d.floor || a.AfterSeq > d.seq {
			return nil, session.ErrSeqTooOld
		}
		for _, ev := range d.events {
			if ev.Seq > a.AfterSeq {
				if err := d.notify(conn, ev); err != nil {
					return nil, err
				}
			}
		}
		d.attached[conn] = true
		d.attaches = append(d.attaches, a)
		if d.onAttach != nil {
			d.onAttach <- struct{}{}
		}
		return session.Info{ID: string(a.Session), Attach: session.AttachStateAttached}, nil
	case "snapshot":
		d.mu.Lock()
		defer d.mu.Unlock()
		return session.Snapshot{Info: session.Info{ID: string(r.Session)}, Seq: d.seq}, nil
	case "port":
		var p daemonproto.PortRequest
		if err := json.Unmarshal(r.Raw, &p); err != nil {
			return nil, err
		}
		d.mu.Lock()
		d.calls = append(d.calls, p)
		d.mu.Unlock()
		switch p.Method {
		case "slide_back_queued":
			return session.Input{Text: "queued", Mode: session.SubmitMode("queue"), Origin: session.InputOrigin("user")}, nil
		case "sessions_query":
			return session.Tree{Workspaces: []session.WorkspaceNode{{RepoRoot: "/repo"}}}, nil
		case "set_title":
			return nil, errors.New("set_title: " + session.ErrBusy.Error())
		}
		return nil, nil
	case "list":
		return []session.Info{{ID: "s1"}}, nil
	case "daemon_status":
		return daemonproto.DaemonStatusData{PID: os.Getpid(), Version: d.version, Sessions: 1}, nil
	case "daemon_stop":
		d.stopOnce.Do(func() { close(d.stopped) })
		return nil, nil
	case "create", "clear":
		return session.Info{ID: "s2"}, nil
	case "detach", "close":
		return nil, nil
	}
	return nil, errors.New("unsupported")
}

func (d *miniDaemon) notify(conn *daemonproto.Conn, ev session.Event) error {
	n, err := daemonproto.EncodeEvent(ev)
	if err != nil {
		return err
	}
	return conn.Notify(n)
}

// publish appends a title event and sends it to attached connections.
func (d *miniDaemon) publish(title string) uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seq++
	ev := session.Event{Seq: d.seq, Session: "s1", Kind: session.EvTitle, Data: session.TitleEvent{Title: title}}
	d.events = append(d.events, ev)
	for c := range d.attached {
		_ = d.notify(c, ev)
	}
	return d.seq
}

// dropSlowConsumer mimics the session controller dropping a full subscriber
// while publishing title: the event is retained, but every attached
// connection instead gets a final detached/slow_consumer event carrying its
// Seq and loses its subscription. Connections stay open. An empty title
// drops without publishing (the drop event carries the next, unused Seq),
// so a re-attach makes no progress.
func (d *miniDaemon) dropSlowConsumer(title string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	seq := d.seq + 1
	if title != "" {
		d.seq = seq
		d.events = append(d.events, session.Event{Seq: seq, Session: "s1", Kind: session.EvTitle, Data: session.TitleEvent{Title: title}})
	}
	drop := session.Event{Seq: seq, Session: "s1", Kind: session.EvAttach, Data: session.AttachEvent{State: session.AttachStateDetached, Reason: "slow_consumer"}}
	for c := range d.attached {
		delete(d.attached, c)
		_ = d.notify(c, drop)
	}
}

func (d *miniDaemon) attachRequests() []daemonproto.AttachRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]daemonproto.AttachRequest(nil), d.attaches...)
}

// trim drops retained events up to and including seq.
func (d *miniDaemon) trim(seq uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.floor = seq
	kept := d.events[:0]
	for _, ev := range d.events {
		if ev.Seq > seq {
			kept = append(kept, ev)
		}
	}
	d.events = kept
}

// dropConns closes every client transport (simulated transport loss).
func (d *miniDaemon) dropConns() {
	d.mu.Lock()
	conns := make([]*daemonproto.Conn, 0, len(d.conns))
	for c := range d.conns {
		conns = append(conns, c)
	}
	d.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

func (d *miniDaemon) portCalls() []daemonproto.PortRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]daemonproto.PortRequest(nil), d.calls...)
}

func (d *miniDaemon) close() {
	d.closeOnce.Do(func() {
		_ = d.ln.Close()
		d.dropConns()
		d.wg.Wait()
	})
}

// count returns how many requests of type typ were received.
func (d *miniDaemon) count(typ string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, t := range d.types {
		if t == typ {
			n++
		}
	}
	return n
}

func (d *miniDaemon) setAttachErr(err error) {
	d.mu.Lock()
	d.attachErr = err
	d.mu.Unlock()
}

// runChildDaemon is the TestMain entry when re-executed as a spawned daemon.
func runChildDaemon(mode string) int {
	switch mode {
	case "fail":
		_, _ = os.Stderr.WriteString("synthetic daemon startup failure\n")
		return 3
	case "loser":
		// Lost the start race: another daemon will own the socket.
		_, _ = os.Stderr.WriteString("loser exiting\n")
		return 0
	}
	d, err := startMiniDaemon(os.Getenv("ZUT_DAEMON_SOCKET"), "child-version", daemonproto.ProtocolVersion)
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		return 1
	}
	select {
	case <-d.stopped:
	case <-time.After(time.Minute):
	}
	d.close()
	return 0
}

func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "zd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "d.sock")
}

func newDaemon(t *testing.T, version string, protocol int) (*miniDaemon, string) {
	t.Helper()
	path := shortSocket(t)
	return newDaemonAt(t, path, version, protocol), path
}

func newDaemonAt(t *testing.T, path, version string, protocol int) *miniDaemon {
	t.Helper()
	d, err := startMiniDaemon(path, version, protocol)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.close)
	return d
}
