package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxStdioFrame bounds one newline-delimited JSON-RPC message from a server.
const maxStdioFrame = 64 << 20

const (
	// stderrTailLines is how many stderr lines are kept for diagnostics.
	stderrTailLines = 20
	// maxStderrLine truncates one retained stderr line; the rest of a longer
	// line is still drained from the pipe, just not kept.
	maxStderrLine = 4096
	// writeQueueSize bounds frames waiting for the writer goroutine.
	writeQueueSize = 64
	// responseWriteTimeout bounds answering one server-initiated request.
	responseWriteTimeout = 30 * time.Second
)

// defaultEnvVars are inherited by stdio servers. Everything else from the host
// environment, including provider API keys, is withheld unless listed in
// envVars or set explicitly in env.
var defaultEnvVars = func() []string {
	if runtime.GOOS == "windows" {
		return []string{
			"APPDATA", "COMPUTERNAME", "COMSPEC", "HOMEDRIVE", "HOMEPATH", "LOCALAPPDATA",
			"NUMBER_OF_PROCESSORS", "OS", "PATH", "PATHEXT", "PROCESSOR_ARCHITECTURE",
			"PROGRAMDATA", "PROGRAMFILES", "PROGRAMFILES(X86)", "SYSTEMDRIVE", "SYSTEMROOT",
			"TEMP", "TMP", "USERDOMAIN", "USERNAME", "USERPROFILE", "WINDIR",
		}
	}
	return []string{
		"HOME", "LOGNAME", "PATH", "SHELL", "USER", "LANG", "LC_ALL", "LC_CTYPE",
		"TERM", "TMPDIR", "TZ", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	}
}()

// writeReq is one frame queued for the writer goroutine.
type writeReq struct {
	ctx   context.Context
	frame []byte
	res   chan error
}

type stdioTransport struct {
	name  string
	cmd   *exec.Cmd
	guard *processGuard
	red   *redactor

	// ctx spans the transport's lifetime; it is cancelled when the
	// connection fails or closes and bounds every background send.
	ctx    context.Context
	cancel context.CancelFunc

	stdin  io.WriteCloser
	outR   *os.File
	errR   *os.File
	writeQ chan *writeReq

	mu      sync.Mutex
	pending map[string]chan message
	handler incomingHandler
	err     error

	stderrMu sync.Mutex
	stderr   []string

	doneCh     chan struct{}
	closeOnce  sync.Once
	exited     chan struct{}
	readDone   chan struct{}
	stderrDone chan struct{}

	// cancelsSent counts notifications/cancelled sends started (diagnostics
	// and tests).
	cancelsSent atomic.Int64
}

// buildStdioEnv returns the restricted child environment.
func buildStdioEnv(cfg ServerConfig) []string {
	env := map[string]string{}
	for _, key := range append(append([]string(nil), defaultEnvVars...), cfg.EnvVars...) {
		if v, ok := os.LookupEnv(key); ok {
			env[key] = v
		}
	}
	for k, v := range cfg.Env {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for _, k := range sortedNames(env) {
		out = append(out, k+"="+env[k])
	}
	return out
}

func startStdio(name string, cfg ServerConfig, cwd string) (*stdioTransport, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Env = buildStdioEnv(cfg)
	dir := cwd
	if cfg.CWD != "" {
		dir = cfg.CWD
		if !filepath.IsAbs(dir) && cwd != "" {
			dir = filepath.Join(cwd, dir)
		}
	}
	cmd.Dir = dir
	isolateProcess(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// Plain OS pipes instead of StdoutPipe/StderrPipe: Wait closes those as
	// soon as the child exits, which can discard output the child wrote just
	// before exiting (a final response, a fatal stderr line).
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, err
	}
	cmd.Stdout = outW
	cmd.Stderr = errW
	red := newRedactor(cfg)
	if err := cmd.Start(); err != nil {
		outR.Close()
		outW.Close()
		errR.Close()
		errW.Close()
		return nil, fmt.Errorf("start %s: %w", cfg.Command, red.scrubError(err))
	}
	outW.Close()
	errW.Close()
	ctx, cancel := context.WithCancel(context.Background())
	t := &stdioTransport{
		name:       name,
		cmd:        cmd,
		guard:      newProcessGuard(cmd),
		red:        red,
		ctx:        ctx,
		cancel:     cancel,
		stdin:      stdin,
		outR:       outR,
		errR:       errR,
		writeQ:     make(chan *writeReq, writeQueueSize),
		pending:    map[string]chan message{},
		doneCh:     make(chan struct{}),
		exited:     make(chan struct{}),
		readDone:   make(chan struct{}),
		stderrDone: make(chan struct{}),
	}
	go t.writeLoop()
	go t.readStderr(errR)
	go t.readLoop(outR)
	go func() {
		waitErr := cmd.Wait()
		close(t.exited)
		if waitErr == nil {
			waitErr = errors.New("process exited")
		}
		// Let the reader hand over whatever the server wrote before exiting,
		// so a response sitting in the pipe wins over the failure.
		select {
		case <-t.readDone:
		case <-time.After(250 * time.Millisecond):
		}
		t.fail(fmt.Errorf("server process ended: %w", waitErr))
	}()
	return t, nil
}

// readStderr drains the child's stderr for its whole life, keeping a bounded
// tail of bounded lines. It never stops early: a child blocked on a full
// stderr pipe would hang.
func (t *stdioTransport) readStderr(r io.Reader) {
	defer close(t.stderrDone)
	reader := bufio.NewReaderSize(r, 4096)
	var line []byte
	truncated := false
	flush := func() {
		text := strings.TrimRight(string(line), "\r")
		if truncated {
			text += "…[line truncated]"
		}
		line, truncated = nil, false
		t.stderrMu.Lock()
		t.stderr = append(t.stderr, text)
		if len(t.stderr) > stderrTailLines {
			t.stderr = t.stderr[len(t.stderr)-stderrTailLines:]
		}
		t.stderrMu.Unlock()
	}
	for {
		chunk, isPrefix, err := reader.ReadLine()
		if room := maxStderrLine - len(line); len(chunk) > room {
			if room > 0 {
				line = append(line, chunk[:room]...)
			}
			truncated = true
		} else {
			line = append(line, chunk...)
		}
		if err != nil {
			if len(line) > 0 || truncated {
				flush()
			}
			return
		}
		if !isPrefix {
			flush()
		}
	}
}

// stderrTail returns recent stderr output, scrubbed for display.
func (t *stdioTransport) stderrTail() string {
	t.stderrMu.Lock()
	lines := append([]string(nil), t.stderr...)
	t.stderrMu.Unlock()
	return t.red.scrub(strings.Join(lines, "\n"))
}

func (t *stdioTransport) readLoop(r io.Reader) {
	defer close(t.readDone)
	reader := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := readLine(reader, maxStdioFrame)
		if len(trimSpace(line)) > 0 {
			frames, perr := parseFrames(line)
			if perr == nil {
				for _, msg := range frames {
					t.dispatch(msg)
				}
			}
			// Non-JSON stdout lines (stray logging) are ignored.
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
				// Output closed. If the process is gone the wait goroutine
				// reports its exit status; otherwise give it a moment.
				select {
				case <-t.exited:
					return
				case <-t.doneCh:
					return
				case <-time.After(time.Second):
				}
			}
			t.fail(fmt.Errorf("read server output: %w", err))
			return
		}
	}
}

func readLine(r *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		buf = append(buf, chunk...)
		if len(buf) > limit {
			return nil, fmt.Errorf("message exceeds %d bytes", limit)
		}
		if err != nil {
			return buf, err
		}
		if !isPrefix {
			return buf, nil
		}
	}
}

func (t *stdioTransport) dispatch(msg message) {
	if msg.isResponse() {
		t.mu.Lock()
		ch := t.pending[idKey(msg.ID)]
		delete(t.pending, idKey(msg.ID))
		t.mu.Unlock()
		if ch != nil {
			ch <- msg
		}
		return
	}
	t.mu.Lock()
	handler := t.handler
	t.mu.Unlock()
	if msg.isRequest() {
		go func() {
			var result any
			rpcErr := &RPCError{Code: codeMethodNotFound, Message: "method not found: " + msg.Method}
			if handler != nil {
				result, rpcErr = handler(msg)
			}
			if frame, err := encodeResponse(msg.ID, result, rpcErr); err == nil {
				ctx, cancel := context.WithTimeout(t.ctx, responseWriteTimeout)
				defer cancel()
				_ = t.write(ctx, frame)
			}
		}()
		return
	}
	if msg.Method != "" && handler != nil {
		go handler(msg)
	}
}

// writeLoop owns all writes to the child's stdin. Callers never block on the
// pipe themselves: they queue a frame and wait for the result or their
// context, so a child that stops reading cannot wedge a request, a timeout, or
// close().
func (t *stdioTransport) writeLoop() {
	for {
		select {
		case <-t.doneCh:
			return
		case req := <-t.writeQ:
			if err := req.ctx.Err(); err != nil {
				req.res <- err // never started: nothing partial on the wire
				continue
			}
			_, err := t.stdin.Write(req.frame)
			req.res <- err
			if err != nil {
				// A failed write may have left half a frame behind.
				t.fail(fmt.Errorf("write to server: %w", err))
				return
			}
		}
	}
}

// write queues one frame (newline appended) and waits until it has been
// written, ctx ends, or the connection fails.
func (t *stdioTransport) write(ctx context.Context, frame []byte) error {
	req := &writeReq{ctx: ctx, frame: append(append(make([]byte, 0, len(frame)+1), frame...), '\n'), res: make(chan error, 1)}
	select {
	case t.writeQ <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-t.doneCh:
		return t.failure()
	}
	select {
	case err := <-req.res:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-t.doneCh:
		select {
		case err := <-req.res:
			return err
		default:
		}
		return t.failure()
	}
}

func (t *stdioTransport) request(ctx context.Context, id int64, method string, params any) (json.RawMessage, error) {
	frame, err := encodeRequest(id, method, params)
	if err != nil {
		return nil, err
	}
	key := fmt.Sprint(id)
	ch := make(chan message, 1)
	t.mu.Lock()
	if t.err != nil {
		err := t.err
		t.mu.Unlock()
		return nil, err
	}
	t.pending[key] = ch
	t.mu.Unlock()
	cleanup := func() {
		t.mu.Lock()
		delete(t.pending, key)
		t.mu.Unlock()
	}
	if err := t.write(ctx, frame); err != nil {
		cleanup()
		if ctx.Err() != nil {
			t.cancelRequest(id, method, ctx.Err().Error())
			return nil, ctx.Err()
		}
		return nil, err
	}
	result := func(msg message) (json.RawMessage, error) {
		if msg.Error != nil {
			return nil, t.red.scrubRPCError(msg.Error)
		}
		return msg.Result, nil
	}
	select {
	case msg := <-ch:
		return result(msg)
	case <-ctx.Done():
		cleanup()
		t.cancelRequest(id, method, ctx.Err().Error())
		return nil, ctx.Err()
	case <-t.doneCh:
		// A response that arrived together with the failure still counts.
		select {
		case msg := <-ch:
			return result(msg)
		default:
		}
		cleanup()
		return nil, t.failure()
	}
}

// cancelRequest announces an abandoned request on a best-effort basis. It
// never blocks the caller, is bounded by cancelNotifyTimeout, and dies with
// the transport.
func (t *stdioTransport) cancelRequest(id int64, method, reason string) {
	if !shouldSendCancel(method) {
		return
	}
	t.cancelsSent.Add(1)
	go func() {
		ctx, cancel := context.WithTimeout(t.ctx, cancelNotifyTimeout)
		defer cancel()
		_ = t.notify(ctx, "notifications/cancelled", map[string]any{"requestId": id, "reason": reason})
	}()
}

func (t *stdioTransport) notify(ctx context.Context, method string, params any) error {
	frame, err := encodeNotification(method, params)
	if err != nil {
		return err
	}
	return t.write(ctx, frame)
}

func (t *stdioTransport) setHandler(h incomingHandler) {
	t.mu.Lock()
	t.handler = h
	t.mu.Unlock()
}

func (t *stdioTransport) setProtocolVersion(string) {}

func (t *stdioTransport) done() <-chan struct{} { return t.doneCh }

func (t *stdioTransport) failure() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err == nil {
		return errors.New("connection closed")
	}
	return t.err
}

func (t *stdioTransport) fail(err error) {
	t.mu.Lock()
	if t.err == nil {
		t.err = err
	}
	t.mu.Unlock()
	t.closeOnce.Do(func() {
		t.cancel()
		close(t.doneCh)
	})
}

func (t *stdioTransport) waitExit(d time.Duration) bool {
	select {
	case <-t.exited:
		return true
	case <-time.After(d):
		return false
	}
}

// close shuts the server down: close stdin (the MCP stdio shutdown signal)
// without waiting behind any blocked write, wait briefly, then terminate the
// process group. Descendants that outlive the direct child are signalled too.
func (t *stdioTransport) close() error {
	t.fail(errors.New("connection closed"))
	_ = t.stdin.Close() // also unblocks a writer stuck on a full pipe
	if !t.waitExit(500 * time.Millisecond) {
		t.guard.terminate(false)
		if !t.waitExit(1500 * time.Millisecond) {
			t.guard.terminate(true)
			t.waitExit(time.Second)
		}
	}
	t.reapGroup()
	// With the child gone its stderr hits EOF once buffered data is read; give
	// the reader a moment so diagnostics are complete. A descendant holding the
	// pipe open only costs the timeout.
	select {
	case <-t.exited:
		select {
		case <-t.stderrDone:
		case <-time.After(500 * time.Millisecond):
		}
	default:
	}
	_ = t.outR.Close()
	_ = t.errR.Close()
	t.guard.release()
	return nil
}

// reapGroup stops processes the direct child left behind. The process group
// outlives its leader, so this is meaningful even after the leader has been
// reaped (the group id cannot be reused while any member remains).
func (t *stdioTransport) reapGroup() {
	if !t.guard.alive() {
		return
	}
	t.guard.terminate(false)
	deadline := time.Now().Add(300 * time.Millisecond)
	for t.guard.alive() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if t.guard.alive() {
		t.guard.terminate(true)
	}
}
