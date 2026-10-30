package daemonproto

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/bnema/zut/packages/agent/internal/ndjson"
)

// ErrClosed is returned to calls when the transport disconnects.
var ErrClosed = errors.New("daemon connection closed")

// ErrFrameTooLarge indicates a frame exceeding the configured byte limit.
var ErrFrameTooLarge = errors.New("daemon frame too large")

// Request contains the dispatch envelope and the complete original JSON frame.
// Raw includes params and request-specific fields from the P0 frame structs.
type Request struct {
	Envelope
	Raw json.RawMessage
}

// Notification is an inbound frame without a request ID.
type Notification = Request

// RequestHandler handles inbound requests concurrently. Its context ends when
// Serve returns; handlers must honor cancellation. The result is encoded losslessly.
type RequestHandler func(context.Context, Request) (any, error)

// NotificationHandler receives notifications in wire order on the read loop.
// It must not block or synchronously Call on this connection.
type NotificationHandler func(Notification)

// Options configures dispatch and the maximum JSON frame size, excluding LF.
// MaxFrame defaults to 32 MiB; larger values are capped at the protocol limit.
type Options struct {
	OnRequest      RequestHandler
	OnNotification NotificationHandler
	MaxFrame       int
}

// RemoteError is the diagnostic returned by an unsuccessful peer result.
type RemoteError struct{ Msg string }

func (e *RemoteError) Error() string { return e.Msg }

// Conn multiplexes calls and notifications over one NDJSON connection. Exactly
// one goroutine must run Serve; Call and Notify may run concurrently with it.
type Conn struct {
	conn    net.Conn
	writer  *ndjson.OrderedPipe
	opts    Options
	mu      sync.Mutex
	pending map[string]chan Result
	done    chan struct{}
	once    sync.Once
	serving atomic.Bool
	next    atomic.Uint64
}

// NewConn creates a connection. It does not start the read loop.
func NewConn(c net.Conn, opts Options) *Conn {
	if opts.MaxFrame <= 0 || opts.MaxFrame > 32<<20 {
		opts.MaxFrame = 32 << 20
	}
	p := &Conn{conn: c, opts: opts, pending: make(map[string]chan Result), done: make(chan struct{})}
	p.writer = ndjson.NewOrderedPipeLimit(c, opts.MaxFrame+1)
	go func() {
		select {
		case <-p.writer.Done():
			p.Close()
		case <-p.done:
		}
	}()
	return p
}

// Done closes when the transport disconnects.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Close disconnects and unblocks all pending calls. It is idempotent.
func (c *Conn) Close() error { c.once.Do(func() { close(c.done); c.writer.Close() }); return nil }

func (c *Conn) write(ctx context.Context, frame any) error {
	b, err := marshalPayload(frame)
	if err != nil {
		return err
	}
	if len(b) > c.opts.MaxFrame {
		return ErrFrameTooLarge
	}
	select {
	case <-c.done:
		return ErrClosed
	default:
	}
	_, err = c.writer.WriteContext(ctx, append(b, '\n'), ndjson.WriteTimeout)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		c.Close()
		return ErrClosed
	}
	return nil
}

// Notify writes a frame without an id. Frames containing an id are rejected.
func (c *Conn) Notify(frame any) error {
	b, err := marshalPayload(frame)
	if err != nil {
		return err
	}
	var e Envelope
	if err = json.Unmarshal(b, &e); err != nil {
		return err
	}
	if e.ID != "" || e.Type == "" {
		return fmt.Errorf("notification requires type and no id")
	}
	return c.write(context.Background(), json.RawMessage(b))
}

// Call assigns a fresh opaque id, sends frame and waits for its result. out may
// be nil; otherwise typed content in result data is restored by the codec.
func (c *Conn) Call(ctx context.Context, frame any, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := marshalPayload(frame)
	if err != nil {
		return err
	}
	var m map[string]json.RawMessage
	if err = json.Unmarshal(b, &m); err != nil {
		return err
	}
	if m == nil || len(m["type"]) == 0 {
		return fmt.Errorf("request must be an object with type")
	}
	id := strconv.FormatUint(c.next.Add(1), 10)
	m["id"], _ = json.Marshal(id)
	ch := make(chan Result, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	if err = c.write(ctx, m); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return ErrClosed
	case r := <-ch:
		if !r.OK {
			return &RemoteError{Msg: r.Error}
		}
		if out != nil && len(r.Data) > 0 {
			return unmarshalPayload(r.Data, out)
		}
		return nil
	}
}

// Serve reads and dispatches frames until cancellation, peer close, or a protocol
// error. Returning always closes the connection and cancels request handlers.
func (c *Conn) Serve(ctx context.Context) error {
	if !c.serving.CompareAndSwap(false, true) {
		return fmt.Errorf("connection already serving")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer c.Close()
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
		case <-c.done:
		}
	}()
	r := bufio.NewReader(c.conn)
	for {
		var frame []byte
		for {
			part, err := r.ReadSlice('\n')
			if len(frame)+len(part) > c.opts.MaxFrame+1 {
				return ErrFrameTooLarge
			}
			frame = append(frame, part...)
			if err == bufio.ErrBufferFull {
				continue
			}
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if len(frame) > 0 && errors.Is(err, io.EOF) {
					return io.ErrUnexpectedEOF
				}
				return ErrClosed
			}
			break
		}
		frame = frame[:len(frame)-1]
		if len(frame) > c.opts.MaxFrame {
			return ErrFrameTooLarge
		}
		var e Envelope
		if err := json.Unmarshal(frame, &e); err != nil {
			return fmt.Errorf("decode daemon frame: %w", err)
		}
		if e.Type == "" {
			return fmt.Errorf("daemon frame has no type")
		}
		if e.Type == "result" {
			if e.ID == "" {
				return fmt.Errorf("result has no id")
			}
			var res Result
			if err := json.Unmarshal(frame, &res); err != nil {
				return err
			}
			c.mu.Lock()
			ch := c.pending[e.ID]
			c.mu.Unlock()
			if ch != nil {
				select {
				case ch <- res:
				default:
				}
			}
			continue
		}
		req := Request{Envelope: e, Raw: append(json.RawMessage(nil), frame...)}
		if e.ID == "" {
			if c.opts.OnNotification != nil {
				c.opts.OnNotification(req)
			}
			continue
		}
		go c.handle(ctx, req)
	}
}
func (c *Conn) handle(ctx context.Context, r Request) {
	var data any
	var err error
	if c.opts.OnRequest == nil {
		err = fmt.Errorf("unsupported request type %q", r.Type)
	} else {
		data, err = c.opts.OnRequest(ctx, r)
	}
	res := Result{ID: r.ID, Type: "result", OK: err == nil}
	if err != nil {
		res.Error = err.Error()
	} else if data != nil {
		res.Data, err = marshalPayload(data)
		if err != nil {
			res.OK = false
			res.Error = "encode result data: " + err.Error()
		}
	}
	if err = c.write(ctx, res); err != nil {
		c.Close()
	}
}
