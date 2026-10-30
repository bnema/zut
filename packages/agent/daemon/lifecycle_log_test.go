package daemon

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"

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
