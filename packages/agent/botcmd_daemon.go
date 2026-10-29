package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/daemonclient"
	"github.com/bnema/zut/packages/agent/daemonproto"
)

// botRequestTimeout bounds one bot command against the daemon (it includes
// creating the bot session).
const botRequestTimeout = 60 * time.Second

// errDaemonAbsent reports that no daemon listens (status/stop never spawn).
var errDaemonAbsent = errors.New("zut daemon is not running")

// botDaemonConn is one bot-command connection. daemonclient.Client has no
// bot requests, so bot commands speak daemonproto directly after
// daemonclient has (optionally) spawned the daemon.
type botDaemonConn struct {
	conn *daemonproto.Conn
	done chan struct{}
}

// dialBotDaemon connects with the hello handshake. With spawn, a missing
// daemon is started with daemonclient.EnsureDaemon.
func dialBotDaemon(ctx context.Context, version string, spawn bool) (*botDaemonConn, error) {
	path := daemonclient.SocketPath(os.Getenv)
	opts := daemonclient.Options{ClientVersion: version, ClientKind: "cli", Warnings: os.Stderr}
	if spawn {
		c, err := daemonclient.EnsureDaemon(ctx, daemonclient.EnsureOptions{Options: opts, SocketPath: path})
		if err != nil {
			return nil, err
		}
		_ = c.Disconnect()
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		if daemonAbsent(err) {
			return nil, errDaemonAbsent
		}
		return nil, fmt.Errorf("dial zut daemon %s: %w", path, err)
	}
	c := &botDaemonConn{conn: daemonproto.NewConn(nc, daemonproto.Options{}), done: make(chan struct{})}
	go func() { _ = c.conn.Serve(context.Background()); close(c.done) }()
	var h daemonproto.HelloData
	if err := c.conn.Call(ctx, daemonproto.HelloRequest{Type: "hello", ProtocolVersion: daemonproto.ProtocolVersion, ClientVersion: version, ClientKind: "cli", PID: os.Getpid()}, &h); err != nil {
		c.close()
		return nil, fmt.Errorf("zut daemon hello: %w", err)
	}
	return c, nil
}

func (c *botDaemonConn) call(ctx context.Context, frame, out any) error {
	err := c.conn.Call(ctx, frame, out)
	var re *daemonproto.RemoteError
	if errors.As(err, &re) {
		return errors.New(re.Msg)
	}
	return err
}

func (c *botDaemonConn) close() {
	_ = c.conn.Close()
	<-c.done
}

// botLogFilter copies only daemon log lines tagged bot=<kind> or mentioning
// the bot kind, buffering partial lines between writes.
type botLogFilter struct {
	w    io.Writer
	kind string
	buf  []byte
}

func (f *botLogFilter) Write(p []byte) (int, error) {
	f.buf = append(f.buf, p...)
	for {
		i := bytes.IndexByte(f.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := string(f.buf[:i+1])
		f.buf = f.buf[i+1:]
		if strings.Contains(line, "bot="+f.kind) || strings.Contains(line, "kind="+f.kind) || strings.Contains(line, f.kind+" bridge") {
			if _, err := io.WriteString(f.w, line); err != nil {
				return len(p), err
			}
		}
	}
}
