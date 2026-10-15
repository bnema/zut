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
	"time"
)

// maxStdioFrame bounds one newline-delimited JSON-RPC message from a server.
const maxStdioFrame = 64 << 20

// stderrTailLines is how many stderr lines are kept for startup diagnostics.
const stderrTailLines = 20

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

type stdioTransport struct {
	name string
	cmd  *exec.Cmd

	writeMu sync.Mutex
	stdin   io.WriteCloser

	mu      sync.Mutex
	pending map[string]chan message
	handler incomingHandler
	err     error

	stderrMu sync.Mutex
	stderr   []string

	doneCh    chan struct{}
	closeOnce sync.Once
	exited    chan struct{}
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
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", cfg.Command, err)
	}
	t := &stdioTransport{
		name:    name,
		cmd:     cmd,
		stdin:   stdin,
		pending: map[string]chan message{},
		doneCh:  make(chan struct{}),
		exited:  make(chan struct{}),
	}
	go t.readStderr(stderr)
	go t.readLoop(stdout)
	go func() {
		waitErr := cmd.Wait()
		close(t.exited)
		if waitErr == nil {
			waitErr = errors.New("process exited")
		}
		t.fail(fmt.Errorf("server process ended: %w", waitErr))
	}()
	return t, nil
}

func (t *stdioTransport) readStderr(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		t.stderrMu.Lock()
		t.stderr = append(t.stderr, line)
		if len(t.stderr) > stderrTailLines {
			t.stderr = t.stderr[len(t.stderr)-stderrTailLines:]
		}
		t.stderrMu.Unlock()
	}
}

// stderrTail returns recent stderr output for diagnostics.
func (t *stdioTransport) stderrTail() string {
	t.stderrMu.Lock()
	defer t.stderrMu.Unlock()
	return strings.Join(t.stderr, "\n")
}

func (t *stdioTransport) readLoop(r io.Reader) {
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
				_ = t.write(frame)
			}
		}()
		return
	}
	if msg.Method != "" && handler != nil {
		go handler(msg)
	}
}

func (t *stdioTransport) write(frame []byte) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	select {
	case <-t.doneCh:
		return t.failure()
	default:
	}
	_, err := t.stdin.Write(append(frame, '\n'))
	return err
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
	if err := t.write(frame); err != nil {
		cleanup()
		return nil, err
	}
	select {
	case msg := <-ch:
		if msg.Error != nil {
			return nil, msg.Error
		}
		return msg.Result, nil
	case <-ctx.Done():
		cleanup()
		_ = t.notify(context.Background(), "notifications/cancelled", map[string]any{"requestId": id, "reason": ctx.Err().Error()})
		return nil, ctx.Err()
	case <-t.doneCh:
		cleanup()
		return nil, t.failure()
	}
}

func (t *stdioTransport) notify(_ context.Context, method string, params any) error {
	frame, err := encodeNotification(method, params)
	if err != nil {
		return err
	}
	return t.write(frame)
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
	t.closeOnce.Do(func() { close(t.doneCh) })
}

// close shuts the server down: close stdin (the MCP stdio shutdown signal),
// wait briefly, then terminate the whole process group.
func (t *stdioTransport) close() error {
	t.fail(errors.New("connection closed"))
	t.writeMu.Lock()
	_ = t.stdin.Close()
	t.writeMu.Unlock()
	select {
	case <-t.exited:
		return nil
	case <-time.After(500 * time.Millisecond):
	}
	terminateProcess(t.cmd, false)
	select {
	case <-t.exited:
		return nil
	case <-time.After(1500 * time.Millisecond):
	}
	terminateProcess(t.cmd, true)
	select {
	case <-t.exited:
	case <-time.After(time.Second):
	}
	return nil
}
