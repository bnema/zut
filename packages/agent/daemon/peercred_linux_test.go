//go:build linux

package daemon

import (
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
)

func TestCheckPeerUID(t *testing.T) {
	ln, err := listenSocket(shortSocket(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.close()
	if err := ln.Listener.(*net.UnixListener).SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("unix", ln.path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	peer, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if uid, err := checkPeerUID(peer, os.Getuid()); err != nil || uid != os.Getuid() {
		t.Fatalf("same UID: uid=%d err=%v", uid, err)
	}
	if uid, err := checkPeerUID(peer, os.Getuid()+1); err == nil || uid != os.Getuid() {
		t.Fatalf("different expected UID: uid=%d err=%v", uid, err)
	}
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := checkPeerUID(peer, os.Getuid()); err == nil {
		t.Fatal("closed connection accepted")
	}
}

func TestDaemonAcceptsSameUID(t *testing.T) {
	r := startDaemon(t, sessionmocks.NewMockFactory(t), t.TempDir(), Options{})
	c := dial(t, r.sock)
	var hello daemonproto.HelloData
	if err := c.call(t, daemonproto.HelloRequest{Type: "hello", ProtocolVersion: daemonproto.ProtocolVersion}, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.ProtocolVersion != daemonproto.ProtocolVersion {
		t.Fatalf("hello: %+v", hello)
	}
}

func TestDaemonRejectsOtherUID(t *testing.T) {
	r := startDaemonWith(t, sessionmocks.NewMockFactory(t), t.TempDir(), Options{}, func(d *Daemon) { d.peerUID = os.Getuid() + 1 })
	conn, err := net.DialTimeout("unix", r.sock, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("{}\n")); err != nil && !errors.Is(err, syscall.EPIPE) && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatal(err)
	}
	if n, err := conn.Read(make([]byte, 1)); n != 0 || (!errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET)) {
		t.Fatalf("connection not closed by daemon: n=%d err=%v", n, err)
	}
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	if r.d.nextClient != 0 || len(r.d.clients) != 0 {
		t.Fatalf("client created for rejected peer: next=%d clients=%d", r.d.nextClient, len(r.d.clients))
	}
}
