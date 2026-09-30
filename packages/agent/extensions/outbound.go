package extensions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// orderedPipe serializes every host→extension frame through one writer
// goroutine so a blocked subprocess stdin never pins a host goroutine for an
// unbounded time. Frames are delivered in enqueue order. Synchronous callers
// wait for their frame's write with a bounded timeout or context; fire-and-
// forget callers only pay for the enqueue.
//
// The queue is bounded in frames and bytes. Overflow disconnects the
// extension transport instead of growing without limit or silently dropping
// a frame in the middle of an ordered stream.
type orderedPipe struct {
	pipe   io.WriteCloser
	mu     sync.Mutex
	queue  []*outboundFrame
	bytes  int
	closed bool
	wake   chan struct{}
	done   chan struct{}
	once   sync.Once
}

type outboundFrame struct {
	data []byte
	ack  chan error
	// writing is set once the writer goroutine has taken ownership of the
	// frame. Until then the frame may still be withdrawn safely.
	writing bool
}

const (
	outboundBytes  = 16 * 1024 * 1024
	outboundFrames = 256

	// transportWriteTimeout bounds how long a synchronous host frame may wait
	// for the extension to drain its stdin. It is independent of any reply
	// deadline: an interactive tool may wait forever for a reply, but the
	// request itself must be accepted by the subprocess promptly.
	transportWriteTimeout = 5 * time.Second

	// abandonGrace is how long a cancelled caller waits for a frame that is
	// already mid-write to finish before the transport is disconnected. A
	// torn frame followed by unrelated bytes would corrupt the stream, so
	// closing is the only safe fallback.
	abandonGrace = 250 * time.Millisecond
)

var errOutboundOverflow = errors.New("extension outbound queue exceeded its limit")

func newOrderedPipe(pipe io.WriteCloser) *orderedPipe {
	p := &orderedPipe{pipe: pipe, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go p.run()
	return p
}

// enqueue appends a frame and returns a handle for withdrawal. ack, when not
// nil, receives exactly one write outcome unless the frame is withdrawn.
func (p *orderedPipe) enqueue(data []byte, ack chan error) (*outboundFrame, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, io.ErrClosedPipe
	}
	if len(p.queue) >= outboundFrames || p.bytes+len(data) > outboundBytes {
		p.mu.Unlock()
		p.Close()
		return nil, errOutboundOverflow
	}
	frame := &outboundFrame{data: append([]byte(nil), data...), ack: ack}
	p.queue = append(p.queue, frame)
	p.bytes += len(data)
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
	return frame, nil
}

// withdraw removes a frame that has not yet started writing. It reports
// false when the writer already owns the frame; in that case the bytes may be
// partially on the wire and cannot be taken back.
func (p *orderedPipe) withdraw(frame *outboundFrame) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if frame.writing {
		return false
	}
	for i, queued := range p.queue {
		if queued == frame {
			p.queue = append(p.queue[:i], p.queue[i+1:]...)
			p.bytes -= len(frame.data)
			return true
		}
	}
	// Not queued and not writing: already written or the pipe was closed.
	return true
}

// Write enqueues and waits for the write with the standard transport bound.
func (p *orderedPipe) Write(data []byte) (int, error) {
	return p.writeContext(context.Background(), data, transportWriteTimeout)
}

// writeContext enqueues data and waits until it is written, the timeout
// elapses, ctx ends, or the pipe closes. A frame still waiting in the queue
// is withdrawn on cancellation without disturbing the transport; a frame
// already being written must finish within the cancellation grace or the
// transport is closed to avoid a torn frame. A write timeout disconnects the
// transport whether the frame is queued or in progress.
func (p *orderedPipe) writeContext(ctx context.Context, data []byte, timeout time.Duration) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("extension outbound write timed out: %w", context.DeadlineExceeded)
	}
	ack := make(chan error, 1)
	frame, err := p.enqueue(data, ack)
	if err != nil {
		return 0, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	abandon := func(reason error) (int, error) {
		if p.withdraw(frame) {
			return 0, reason
		}
		// Mid-write: wait briefly for the writer to finish this frame so a
		// complete frame lands or the pipe error surfaces; otherwise close.
		select {
		case err := <-ack:
			if err != nil {
				return 0, err
			}
			return len(data), nil
		case <-p.done:
			return 0, io.ErrClosedPipe
		case <-time.After(abandonGrace):
			p.Close()
			return 0, reason
		}
	}
	select {
	case err := <-ack:
		if err != nil {
			return 0, err
		}
		return len(data), nil
	case <-p.done:
		// The frame may have completed just before the disconnect. If the
		// writer owns it, its outcome arrives promptly because the closed
		// pipe fails any in-progress write; report that outcome accurately so
		// callers distinguish "request delivered, peer gone" from "never sent".
		if !p.withdraw(frame) {
			if err := <-ack; err != nil {
				return 0, err
			}
			return len(data), nil
		}
		select {
		case err := <-ack:
			if err == nil {
				return len(data), nil
			}
			return 0, err
		default:
		}
		return 0, io.ErrClosedPipe
	case <-ctx.Done():
		return abandon(ctx.Err())
	case <-timer.C:
		p.Close()
		return 0, fmt.Errorf("extension outbound write timed out: %w", context.DeadlineExceeded)
	}
}

// Close disconnects the transport. Queued frames are discarded and every
// waiter observes done. Safe to call more than once.
func (p *orderedPipe) Close() error {
	p.once.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.queue = nil
		p.bytes = 0
		p.mu.Unlock()
		close(p.done)
		_ = p.pipe.Close()
	})
	return nil
}

// Done is closed when the transport has been disconnected.
func (p *orderedPipe) Done() <-chan struct{} { return p.done }

func (p *orderedPipe) run() {
	for {
		select {
		case <-p.done:
			return
		case <-p.wake:
		}
		for {
			p.mu.Lock()
			if p.closed || len(p.queue) == 0 {
				p.mu.Unlock()
				break
			}
			frame := p.queue[0]
			frame.writing = true
			p.queue[0] = nil
			p.queue = p.queue[1:]
			p.bytes -= len(frame.data)
			p.mu.Unlock()
			n, err := p.pipe.Write(frame.data)
			if err == nil && n != len(frame.data) {
				err = io.ErrShortWrite
			}
			if frame.ack != nil {
				frame.ack <- err
			}
			if err != nil {
				p.Close()
				return
			}
		}
	}
}
