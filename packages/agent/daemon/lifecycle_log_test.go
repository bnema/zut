package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
)

func TestDaemonLogsClientLifecycleWithoutSecrets(t *testing.T) {
	const secret = "sk-live-CREATESECRET-123"
	s1, s2 := newTestSession(t, "s1"), newTestSession(t, "s2")
	f := sessionmocks.NewMockFactory(t)
	f.EXPECT().Create(mock.Anything, mock.Anything).Return(s1.h, nil).Once()
	f.EXPECT().Create(mock.Anything, mock.Anything).Return(s2.h, nil).Once()
	buf := &syncBuffer{}
	r := startDaemon(t, f, t.TempDir(), Options{Logger: slog.New(slog.NewTextHandler(buf, nil))})
	a := dial(t, r.sock)
	if err := a.call(t, daemonproto.HelloRequest{Type: "hello", ProtocolVersion: daemonproto.ProtocolVersion, ClientKind: "tui"}, nil); err != nil {
		t.Fatal(err)
	}
	spec := session.CreateSpec{CWD: "/w", Env: []string{"API_KEY=" + secret}, Args: []string{"--prompt", secret}}
	if err := a.call(t, daemonproto.CreateRequest{Type: "create", Spec: spec}, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.call(t, daemonproto.AttachRequest{Type: "attach", Session: "s1", Controlling: true}, nil); err != nil {
		t.Fatal(err)
	}
	recv(t, s1.attach)
	if err := a.call(t, daemonproto.DetachRequest{Type: "detach", Session: "s1"}, nil); err != nil {
		t.Fatal(err)
	}
	recv(t, s1.attach)
	if err := a.call(t, daemonproto.AttachRequest{Type: "attach", Session: "s1", Controlling: true}, nil); err != nil {
		t.Fatal(err)
	}
	recv(t, s1.attach)
	if err := a.call(t, daemonproto.ClearRequest{Type: "clear", Session: "s1"}, nil); err != nil {
		t.Fatal(err)
	}
	recv(t, s2.attach)
	if err := a.call(t, daemonproto.CloseRequest{Type: "close", Session: "s2", Reason: "user_exit"}, nil); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`msg="client create" client=1 client_kind=tui session=s1`,
		`msg="client attach" client=1 client_kind=tui session=s1 controlling=true`,
		`msg="client detach" client=1 client_kind=tui session=s1`,
		`msg="client clear" client=1 client_kind=tui session=s1 successor=s2`,
		`msg="client kill" client=1 client_kind=tui session=s2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "CREATESECRET") {
		t.Fatalf("log leaked secret:\n%s", out)
	}
}

func TestDaemonFailedClientOperationsLogNothing(t *testing.T) {
	s1 := newTestSession(t, "s1")
	f := sessionmocks.NewMockFactory(t)
	f.EXPECT().Create(mock.Anything, mock.Anything).Return(nil, errors.New("boom")).Once()
	f.EXPECT().Create(mock.Anything, mock.Anything).Return(s1.h, nil).Once()
	buf := &syncBuffer{}
	r := startDaemon(t, f, t.TempDir(), Options{Logger: slog.New(slog.NewTextHandler(buf, nil))})
	a, b := dial(t, r.sock), dial(t, r.sock)
	if err := a.call(t, daemonproto.CreateRequest{Type: "create"}, nil); err == nil {
		t.Fatal("create should fail")
	}
	if err := a.call(t, daemonproto.AttachRequest{Type: "attach", Session: "missing"}, nil); err == nil {
		t.Fatal("attach to unknown session should fail")
	}
	if err := a.call(t, daemonproto.DetachRequest{Type: "detach", Session: "missing"}, nil); err == nil {
		t.Fatal("detach of unknown session should fail")
	}
	// Session s1 exists but b does not control it.
	if err := a.call(t, daemonproto.CreateRequest{Type: "create"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.call(t, daemonproto.AttachRequest{Type: "attach", Session: "s1", Controlling: true}, nil); err != nil {
		t.Fatal(err)
	}
	recv(t, s1.attach)
	before := buf.String()
	if err := b.call(t, daemonproto.CloseRequest{Type: "close", Session: "s1"}, nil); err == nil {
		t.Fatal("close by non-controller should fail")
	}
	if err := b.call(t, daemonproto.ClearRequest{Type: "clear", Session: "s1"}, nil); err == nil {
		t.Fatal("clear by non-controller should fail")
	}
	out := buf.String()
	if strings.Count(out, `msg="client `) != 2 {
		t.Fatalf("want only the successful create and attach logged:\n%s", out)
	}
	if after := strings.TrimPrefix(out, before); strings.Contains(after, `msg="client `) {
		t.Fatalf("failed close/clear logged:\n%s", after)
	}
}

func TestClientDisconnectReasons(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		reason string
	}{
		{"nil", nil, "eof"},
		{"eof", io.EOF, "eof"},
		{"partial", io.ErrUnexpectedEOF, "eof"},
		{"closed", daemonproto.ErrClosed, "closed"},
		{"net closed", net.ErrClosed, "closed"},
		{"canceled", context.Canceled, "canceled"},
		{"error", errors.New("boom"), "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := &syncBuffer{}
			d := &Daemon{ctx: context.Background(), log: slog.New(slog.NewTextHandler(buf, nil))}
			c := &client{id: 7, d: d, kind: "tui"}
			c.logDisconnect(tc.err)
			out := buf.String()
			require.Contains(t, out, `level=INFO msg="client disconnect" client=7 client_kind=tui reason=`+tc.reason)
			require.Equal(t, 1, strings.Count(out, `msg="client disconnect"`))
			require.Equal(t, tc.reason == "error", strings.Contains(out, "err=boom"))
		})
	}
}

func TestDaemonLogsDisconnectButNotShutdown(t *testing.T) {
	buf := &syncBuffer{}
	f := sessionmocks.NewMockFactory(t)
	r := startDaemon(t, f, t.TempDir(), Options{Logger: slog.New(slog.NewTextHandler(buf, nil))})
	a := dial(t, r.sock)
	require.NoError(t, a.call(t, daemonproto.HelloRequest{Type: "hello", ProtocolVersion: daemonproto.ProtocolVersion, ClientKind: "tui"}, nil))
	require.NoError(t, a.conn.Close())
	require.Eventually(t, func() bool {
		return strings.Contains(buf.String(), `msg="client disconnect" client=1 client_kind=tui reason=closed`)
	}, 5*time.Second, time.Millisecond)
	b := dial(t, r.sock)
	require.NoError(t, b.call(t, daemonproto.HelloRequest{Type: "hello", ProtocolVersion: daemonproto.ProtocolVersion, ClientKind: "tui"}, nil))
	r.stop(t)
	require.Equal(t, 1, strings.Count(buf.String(), `msg="client disconnect"`))
}
