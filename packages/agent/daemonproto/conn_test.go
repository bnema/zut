package daemonproto_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/provider"
	"net"
	"runtime"
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

// Reading a prefix synchronizes with an active write, without a test transport.
func TestConnPeerCloseDuringCallWrite(t *testing.T) {
	for i := 0; i < 250; i++ {
		a, b := net.Pipe()
		c := daemonproto.NewConn(a, daemonproto.Options{})
		result := make(chan error, 1)
		go func() {
			result <- c.Call(context.Background(), map[string]any{"type": "write", "payload": strings.Repeat("x", 4096)}, nil)
		}()
		var prefix [1]byte
		if _, err := b.Read(prefix[:]); err != nil {
			t.Fatal(err)
		}
		b.Close()
		select {
		case err := <-result:
			if !errors.Is(err, daemonproto.ErrClosed) {
				t.Fatalf("iteration %d: %v", i, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("iteration %d: Call blocked", i)
		}
		select {
		case <-c.Done():
		default:
			t.Fatalf("iteration %d: connection not synchronously closed", i)
		}
		c.Close()
	}
}

func TestConnExactFrameBoundary(t *testing.T) {
	const limit = 128
	for _, size := range []int{limit, limit + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			frame := `{"type":"note","padding":"` + strings.Repeat("x", size-len(`{"type":"note","padding":""}`)) + `"}`
			if len(frame) != size {
				t.Fatalf("fixture size %d", len(frame))
			}
			a, b := net.Pipe()
			received := make(chan struct{}, 1)
			c := daemonproto.NewConn(a, daemonproto.Options{MaxFrame: limit, OnNotification: func(daemonproto.Notification) { received <- struct{}{} }})
			done := make(chan error, 1)
			go func() { done <- c.Serve(context.Background()) }()
			go func(w net.Conn) { _, _ = w.Write([]byte(frame + "\n")); w.Close() }(b)
			select {
			case err := <-done:
				if size == limit {
					if !errors.Is(err, daemonproto.ErrClosed) {
						t.Fatal(err)
					}
					select {
					case <-received:
					default:
						t.Fatal("exact limit frame not dispatched")
					}
				} else if !errors.Is(err, daemonproto.ErrFrameTooLarge) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("read blocked")
			}
			c.Close()
			b.Close()
			// Verify the same boundary on the outbound path.
			oa, ob := net.Pipe()
			oc := daemonproto.NewConn(oa, daemonproto.Options{MaxFrame: limit})
			read := make(chan string, 1)
			go func(r net.Conn) { line, _ := bufio.NewReader(r).ReadString('\n'); read <- line }(ob)
			err := oc.Notify(json.RawMessage(frame))
			if size == limit {
				if err != nil {
					t.Fatal(err)
				}
				select {
				case line := <-read:
					if len(line) != limit+1 {
						t.Fatalf("wire length %d", len(line))
					}
				case <-time.After(time.Second):
					t.Fatal("write blocked")
				}
			} else if !errors.Is(err, daemonproto.ErrFrameTooLarge) {
				t.Fatal(err)
			}
			oc.Close()
			ob.Close()
			if size > limit {
				select {
				case <-read:
				case <-time.After(time.Second):
					t.Fatal("reader did not exit")
				}
			}
		})
	}
}

func orderedWriterGoroutines() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "/internal/ndjson.(*OrderedPipe).run(")
}

func TestConnCloseCompletesWriterAndHandler(t *testing.T) {
	baseline := orderedWriterGoroutines()
	entered := make(chan struct{})
	handlerDone := make(chan struct{})
	c, s := servePair(t, daemonproto.Options{OnRequest: func(ctx context.Context, _ daemonproto.Request) (any, error) {
		close(entered)
		defer close(handlerDone)
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	callDone := make(chan error, 1)
	go func() { callDone <- c.Call(context.Background(), map[string]any{"type": "wait"}, nil) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	s.Close()
	c.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		handlerExited := false
		select {
		case <-handlerDone:
			handlerExited = true
		default:
		}
		if handlerExited && orderedWriterGoroutines() <= baseline {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines did not exit: handler=%v writers=%d baseline=%d", handlerExited, orderedWriterGoroutines(), baseline)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-callDone:
		if !errors.Is(err, daemonproto.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Call did not exit")
	}
}
