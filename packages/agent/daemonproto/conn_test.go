package daemonproto_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/provider"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func servePair(t *testing.T, opts daemonproto.Options) (*daemonproto.Conn, *daemonproto.Conn) {
	t.Helper()
	a, b := net.Pipe()
	left := daemonproto.NewConn(a, daemonproto.Options{})
	right := daemonproto.NewConn(b, opts)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	done := make(chan struct{}, 2)
	go func() { _ = left.Serve(ctx); done <- struct{}{} }()
	go func() { _ = right.Serve(ctx); done <- struct{}{} }()
	t.Cleanup(func() { cancel(); left.Close(); right.Close(); <-done; <-done })
	return left, right
}
func TestConnConcurrentCalls(t *testing.T) {
	c, _ := servePair(t, daemonproto.Options{OnRequest: func(ctx context.Context, r daemonproto.Request) (any, error) {
		var v struct {
			Value int `json:"value"`
		}
		if err := json.Unmarshal(r.Raw, &v); err != nil {
			return nil, err
		}
		return v.Value, nil
	}})
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var got int
			if err := c.Call(ctx, map[string]any{"type": "echo", "value": i}, &got); err != nil || got != i {
				t.Errorf("call %d: %d %v", i, got, err)
			}
		}()
	}
	wg.Wait()
}
func TestConnErrorsNotificationsAndContent(t *testing.T) {
	notes := make(chan daemonproto.Notification, 1)
	c, s := servePair(t, daemonproto.Options{OnNotification: func(n daemonproto.Notification) { notes <- n }, OnRequest: func(ctx context.Context, r daemonproto.Request) (any, error) {
		if r.Type == "fail" {
			return nil, fmt.Errorf("synthetic")
		}
		return provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "ok"}}}, nil
	}})
	var remote *daemonproto.RemoteError
	if err := c.Call(context.Background(), map[string]any{"type": "fail"}, nil); !errors.As(err, &remote) || remote.Msg != "synthetic" {
		t.Fatalf("remote error %v", err)
	}
	var msg provider.Message
	if err := c.Call(context.Background(), map[string]any{"type": "message"}, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Content[0].(provider.TextBlock).Text != "ok" {
		t.Fatal(msg)
	}
	if err := c.Notify(map[string]any{"type": "note"}); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-notes:
		if n.Type != "note" || n.ID != "" {
			t.Fatal(n)
		}
	case <-time.After(time.Second):
		t.Fatal("missing notification")
	}
	if err := s.Notify(map[string]any{"type": "note", "id": "bad"}); err == nil {
		t.Fatal("notification accepted id")
	}
}
func TestConnCancelAndPeerClose(t *testing.T) {
	started := make(chan struct{}, 2)
	c, s := servePair(t, daemonproto.Options{OnRequest: func(ctx context.Context, r daemonproto.Request) (any, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Call(ctx, map[string]any{"type": "wait"}, nil) }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	go func() { done <- c.Call(context.Background(), map[string]any{"type": "wait"}, nil) }()
	<-started
	s.Close()
	select {
	case err := <-done:
		if !errors.Is(err, daemonproto.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending call blocked")
	}
}
func TestConnFrameLimit(t *testing.T) {
	for _, wire := range []string{strings.Repeat("x", 65) + "\n", strings.Repeat("x", 65)} {
		t.Run(fmt.Sprint(len(wire)), func(t *testing.T) {
			a, b := net.Pipe()
			c := daemonproto.NewConn(a, daemonproto.Options{MaxFrame: 32})
			defer c.Close()
			defer b.Close()
			done := make(chan error, 1)
			go func() { done <- c.Serve(context.Background()) }()
			go func() { _, _ = b.Write([]byte(wire)); b.Close() }()
			if err := <-done; !errors.Is(err, daemonproto.ErrFrameTooLarge) {
				t.Fatalf("oversize = %v", err)
			}
		})
	}
	c, _ := servePair(t, daemonproto.Options{})
	if err := c.Notify(map[string]any{"type": "large", "data": strings.Repeat("x", 32<<20)}); !errors.Is(err, daemonproto.ErrFrameTooLarge) {
		t.Fatalf("outbound oversize = %v", err)
	}
}

func TestConnServeCancellationAndMalformedFrames(t *testing.T) {
	for _, frame := range []string{"{}\n", "null\n", "[]\n", "{bad}\n", "{\"type\":\"result\"}\n"} {
		a, b := net.Pipe()
		c := daemonproto.NewConn(a, daemonproto.Options{})
		done := make(chan error, 1)
		go func() { done <- c.Serve(context.Background()) }()
		go func() { _, _ = b.Write([]byte(frame)); b.Close() }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("accepted %q", frame)
			}
		case <-time.After(time.Second):
			t.Fatal("malformed frame blocked")
		}
		c.Close()
		b.Close()
	}
	a, b := net.Pipe()
	defer b.Close()
	c := daemonproto.NewConn(a, daemonproto.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Serve(ctx) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not cancel")
	}
	if err := c.Call(context.Background(), map[string]any{"type": "closed"}, nil); !errors.Is(err, daemonproto.ErrClosed) {
		t.Fatal(err)
	}
}
