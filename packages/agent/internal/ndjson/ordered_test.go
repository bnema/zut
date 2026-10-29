package ndjson

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestOrderedPipeWriteTimeoutDisconnects(t *testing.T) {
	r, w := io.Pipe()
	pipe := NewOrderedPipe(w)
	t.Cleanup(func() { _ = r.Close() })
	// Nobody reads r, so the first write blocks in the writer goroutine and
	// the second waits in the queue.
	first := make(chan error, 1)
	go func() {
		_, err := pipe.WriteContext(context.Background(), []byte("a\n"), 30*time.Millisecond)
		first <- err
	}()
	select {
	case err := <-first:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected timeout, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bounded write did not return")
	}
	select {
	case <-pipe.Done():
	case <-time.After(time.Second):
		t.Fatal("transport not disconnected after write timeout")
	}
	if _, err := pipe.Write([]byte("b\n")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write after disconnect = %v", err)
	}
}

func TestOrderedPipeCancelWithdrawsQueuedFrameWithoutDisconnect(t *testing.T) {
	r, w := io.Pipe()
	pipe := NewOrderedPipe(w)
	t.Cleanup(func() { pipe.Close(); _ = r.Close() })

	// Head frame blocks in the writer; the second frame stays queued.
	headAck := make(chan error, 1)
	if _, err := pipe.Enqueue([]byte("head\n"), headAck); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan error, 1)
	go func() {
		_, err := pipe.WriteContext(ctx, []byte("second\n"), time.Minute)
		second <- err
	}()
	cancel()
	select {
	case err := <-second:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancel, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued write did not return on cancel")
	}
	select {
	case <-pipe.Done():
		t.Fatal("cancel of a queued frame must not disconnect the transport")
	default:
	}
	// Reader drains: only the head frame is delivered, the withdrawn one is
	// not, and the transport keeps working.
	go func() {
		_, _ = pipe.Write([]byte("third\n"))
	}()
	sc := bufio.NewScanner(r)
	var got []string
	for len(got) < 2 && sc.Scan() {
		got = append(got, sc.Text())
	}
	if strings.Join(got, ",") != "head,third" {
		t.Fatalf("delivered frames = %v", got)
	}
}

func TestOrderedPipeOverflowDisconnects(t *testing.T) {
	r, w := io.Pipe()
	pipe := NewOrderedPipe(w)
	t.Cleanup(func() { _ = r.Close() })
	var err error
	for i := 0; i <= QueueFrames+1; i++ {
		if _, err = pipe.Enqueue([]byte("x\n"), nil); err != nil {
			break
		}
	}
	if !errors.Is(err, ErrOverflow) {
		t.Fatalf("expected overflow, got %v", err)
	}
	select {
	case <-pipe.Done():
	case <-time.After(time.Second):
		t.Fatal("overflow did not disconnect")
	}
}
