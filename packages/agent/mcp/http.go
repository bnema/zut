package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	maxHTTPBody     = 64 << 20
	maxErrorBody    = 4 << 10
	headerSessionID = "Mcp-Session-Id"
	headerProtocol  = "MCP-Protocol-Version"
)

// httpTransport implements the MCP streamable HTTP transport: every client
// message is a POST; responses arrive as JSON or as an SSE stream. A GET
// stream carries server-initiated messages when the server supports it.
type httpTransport struct {
	name    string
	url     string
	headers map[string]string
	client  *http.Client
	red     *redactor

	mu        sync.Mutex
	sessionID string
	protocol  string
	handler   incomingHandler
	err       error
	// sessionGone is set when the server dropped our session (HTTP 404); the
	// DELETE on close is pointless then.
	sessionGone bool
	deleted     bool

	ctx       context.Context
	cancel    context.CancelFunc
	doneCh    chan struct{}
	closeOnce sync.Once
	listening bool

	// cancelsSent counts notifications/cancelled sends started.
	cancelsSent atomic.Int64
}

func newHTTPTransport(name string, cfg ServerConfig, client *http.Client) (*httpTransport, error) {
	headers := map[string]string{}
	for k, v := range cfg.Headers {
		headers[k] = v
	}
	for header, envName := range cfg.EnvHeaders {
		if v, ok := lookupBaseEnv(cfg.BaseEnv, envName); ok && v != "" {
			headers[header] = v
		}
	}
	if cfg.BearerTokenEnvVar != "" {
		token, ok := lookupBaseEnv(cfg.BaseEnv, cfg.BearerTokenEnvVar)
		if !ok || strings.TrimSpace(token) == "" {
			return nil, fmt.Errorf("bearer token variable %s is not set", cfg.BearerTokenEnvVar)
		}
		headers["Authorization"] = "Bearer " + strings.TrimSpace(token)
	}
	if client == nil {
		client = &http.Client{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &httpTransport{
		name:    name,
		url:     cfg.URL,
		headers: headers,
		client:  client,
		red:     newRedactor(cfg),
		ctx:     ctx,
		cancel:  cancel,
		doneCh:  make(chan struct{}),
	}, nil
}

func (t *httpTransport) newRequest(ctx context.Context, method string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.url, reader)
	if err != nil {
		// url parse errors quote the whole URL, query token included.
		return nil, t.red.scrubError(err)
	}
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("User-Agent", "zut-mcp-client")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	t.mu.Lock()
	if t.sessionID != "" {
		req.Header.Set(headerSessionID, t.sessionID)
	}
	if t.protocol != "" {
		req.Header.Set(headerProtocol, t.protocol)
	}
	t.mu.Unlock()
	return req, nil
}

// bind returns a context that ends when ctx ends or the transport fails or
// closes, so no request outlives the connection.
func (t *httpTransport) bind(ctx context.Context) (context.Context, context.CancelFunc) {
	bound, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(t.ctx, cancel)
	return bound, func() {
		stop()
		cancel()
	}
}

func (t *httpTransport) post(ctx context.Context, frame []byte) (*http.Response, error) {
	req, err := t.newRequest(ctx, http.MethodPost, frame)
	if err != nil {
		return nil, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, t.red.scrubError(err)
	}
	if sid := resp.Header.Get(headerSessionID); sid != "" {
		t.mu.Lock()
		t.sessionID = sid
		t.mu.Unlock()
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		// Read past the display limit by the longest secret's length, so a
		// secret straddling the cut is seen whole and can be redacted.
		limit := maxErrorBody + t.red.overlap()
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, int64(limit)))
		return nil, t.statusFailure(req, resp.StatusCode, resp.Header, snippet, len(snippet) >= limit)
	}
	return resp, nil
}

// statusFailure builds the error for a non-success status. A 404 for a
// request that carried a session id means the server forgot the session
// (expired or restarted): the transport becomes unusable so the manager
// reconnects with a fresh initialize. The failed call is not replayed.
func (t *httpTransport) statusFailure(req *http.Request, status int, headers http.Header, body []byte, truncated bool) error {
	he := t.red.statusErrorBody(status, body, truncated).(*HTTPStatusError)
	he.OAuth = isOAuthChallenge(headers)
	he.Name = t.name
	var err error = he
	if status == http.StatusNotFound && req.Header.Get(headerSessionID) != "" {
		err = fmt.Errorf("mcp session expired: %w", err)
		t.mu.Lock()
		t.sessionGone = true
		t.mu.Unlock()
		t.fail(err)
	}
	return err
}

// HTTPStatusError reports a non-success HTTP status from a server.
type HTTPStatusError struct {
	Status int
	Name   string
	OAuth  bool
	Body   string
}

func (e *HTTPStatusError) Error() string {
	msg := fmt.Sprintf("http %d %s", e.Status, http.StatusText(e.Status))
	if e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden {
		if e.OAuth {
			name := e.Name
			if name == "" {
				name = "NAME"
			}
			msg += fmt.Sprintf(" (server requires OAuth sign-in; run zut mcp login %s; explicit headers or bearerTokenEnvVar take precedence)", name)
		} else {
			msg += " (check headers, envHeaders, or bearerTokenEnvVar)"
		}
	}
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

func httpStatusError(status int, body []byte) error {
	return (*redactor)(nil).statusError(status, body)
}

// statusError builds an HTTPStatusError from a complete body.
func (r *redactor) statusError(status int, body []byte) error {
	return r.statusErrorBody(status, body, false)
}

// maxStatusBodyDisplay is how much of an error body is shown.
const maxStatusBodyDisplay = 300

// statusErrorBody builds an HTTPStatusError whose body is redacted, stripped of
// terminal control sequences, and shortened for display. truncated says the
// body was cut by the reader at an arbitrary byte, so it may end inside a
// secret; whatever fragment of one remains at the very end is removed. The
// final display cut gets the same protection.
func (r *redactor) statusErrorBody(status int, body []byte, truncated bool) error {
	text := r.scrubOneLine(string(body))
	if truncated {
		text = r.trimPartialSecret(text)
	}
	text = strings.TrimSpace(text)
	if len(text) > maxStatusBodyDisplay {
		cut := maxStatusBodyDisplay
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = r.trimPartialSecret(text[:cut]) + "…"
	}
	return &HTTPStatusError{Status: status, Body: text}
}

func (t *httpTransport) request(ctx context.Context, id int64, method string, params any) (json.RawMessage, error) {
	if err := t.failure(); err != nil {
		return nil, err
	}
	frame, err := encodeRequest(id, method, params)
	if err != nil {
		return nil, err
	}
	// Bind the request to both the caller and the transport lifetime.
	reqCtx, cancel := t.bind(ctx)
	defer cancel()
	resp, err := t.post(reqCtx, frame)
	if err != nil {
		if ctx.Err() != nil {
			t.cancelRequest(id, method, ctx.Err().Error())
			// Preserve the operation cause (notably credential contention).
			return nil, err
		}
		if t.ctx.Err() != nil {
			return nil, t.failure()
		}
		return nil, err
	}
	defer resp.Body.Close()
	want := fmt.Sprint(id)
	var result *message
	handle := func(msg message) bool {
		if msg.isResponse() && idKey(msg.ID) == want {
			m := msg
			result = &m
			return true
		}
		t.dispatchIncoming(msg)
		return false
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch mediaType {
	case "text/event-stream":
		err = readSSEFrames(resp.Body, handle)
	default:
		var raw []byte
		raw, err = io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody+1))
		if err == nil && len(raw) > maxHTTPBody {
			err = fmt.Errorf("response exceeds %d bytes", maxHTTPBody)
		}
		if err == nil {
			var frames []message
			frames, err = parseFrames(raw)
			for _, msg := range frames {
				if handle(msg) {
					break
				}
			}
		}
	}
	if result == nil {
		if ctx.Err() != nil {
			t.cancelRequest(id, method, ctx.Err().Error())
			return nil, ctx.Err()
		}
		if t.ctx.Err() != nil {
			return nil, t.failure()
		}
		if err != nil {
			return nil, fmt.Errorf("read response: %w", t.red.scrubError(err))
		}
		return nil, errors.New("server closed the response without a result")
	}
	if result.Error != nil {
		return nil, t.red.scrubRPCError(result.Error)
	}
	return result.Result, nil
}

// cancelRequest announces an abandoned request on a best-effort basis. It
// never delays the caller, is bounded by cancelNotifyTimeout, and ends with
// the transport. initialize is never cancelled (MCP lifecycle).
func (t *httpTransport) cancelRequest(id int64, method, reason string) {
	if !shouldSendCancel(method) || t.ctx.Err() != nil {
		return
	}
	t.cancelsSent.Add(1)
	go func() {
		ctx, cancel := context.WithTimeout(t.ctx, cancelNotifyTimeout)
		defer cancel()
		_ = t.notify(ctx, "notifications/cancelled", map[string]any{"requestId": id, "reason": reason})
	}()
}

// errSSEEventTooLarge reports an SSE event whose aggregated data exceeds the
// body limit.
type errSSEEventTooLarge struct{ limit int }

func (e errSSEEventTooLarge) Error() string {
	return fmt.Sprintf("sse event data exceeds %d bytes", e.limit)
}

// readSSEFrames delivers each SSE data payload as JSON-RPC frames until
// handle returns true or the stream ends.
func readSSEFrames(r io.Reader, handle func(message) bool) error {
	return readSSEFramesLimit(r, maxHTTPBody, handle)
}

// readSSEFramesLimit is readSSEFrames with an explicit bound on the data of
// one event, counted across all of its data: lines.
func readSSEFramesLimit(r io.Reader, limit int, handle func(message) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), limit+1)
	var data strings.Builder
	flush := func() bool {
		if data.Len() == 0 {
			return false
		}
		frames, err := parseFrames([]byte(data.String()))
		data.Reset()
		if err != nil {
			return false
		}
		for _, msg := range frames {
			if handle(msg) {
				return true
			}
		}
		return false
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if flush() {
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		if field == "data" {
			extra := len(value)
			if data.Len() > 0 {
				extra++
			}
			if data.Len()+extra > limit {
				return errSSEEventTooLarge{limit: limit}
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return errSSEEventTooLarge{limit: limit}
		}
		return err
	}
	flush()
	return nil
}

func (t *httpTransport) dispatchIncoming(msg message) {
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
			frame, err := encodeResponse(msg.ID, result, rpcErr)
			if err != nil {
				return
			}
			ctx, cancel := context.WithTimeout(t.ctx, 30*time.Second)
			defer cancel()
			if resp, err := t.post(ctx, frame); err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
				resp.Body.Close()
			}
		}()
		return
	}
	if msg.Method != "" && handler != nil {
		go handler(msg)
	}
}

func (t *httpTransport) notify(ctx context.Context, method string, params any) error {
	frame, err := encodeNotification(method, params)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ctx, stop := t.bind(ctx)
	defer stop()
	resp, err := t.post(ctx, frame)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	return resp.Body.Close()
}

// listen opens the optional GET stream for server-initiated notifications
// such as tools/list_changed. Servers that answer 405 do not offer it.
func (t *httpTransport) listen() {
	t.mu.Lock()
	if t.listening {
		t.mu.Unlock()
		return
	}
	t.listening = true
	t.mu.Unlock()
	go func() {
		backoff := time.Second
		for t.ctx.Err() == nil {
			req, err := t.newRequest(t.ctx, http.MethodGet, nil)
			if err != nil {
				return
			}
			req.Header.Set("Accept", "text/event-stream")
			resp, err := t.client.Do(req)
			if err != nil {
				if !sleepCtx(t.ctx, backoff) {
					return
				}
				backoff = min(backoff*2, 30*time.Second)
				continue
			}
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				if resp.StatusCode == http.StatusNotFound && req.Header.Get(headerSessionID) != "" {
					t.mu.Lock()
					t.sessionGone = true
					t.mu.Unlock()
					t.fail(fmt.Errorf("mcp session expired: %w", t.red.statusError(resp.StatusCode, nil)))
				}
				// 405: no server stream. Other statuses: give up quietly; the
				// POST path still works.
				return
			}
			backoff = time.Second
			_ = readSSEFrames(resp.Body, func(msg message) bool {
				t.dispatchIncoming(msg)
				return false
			})
			resp.Body.Close()
			if !sleepCtx(t.ctx, backoff) {
				return
			}
		}
	}()
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (t *httpTransport) setHandler(h incomingHandler) {
	t.mu.Lock()
	t.handler = h
	t.mu.Unlock()
}

func (t *httpTransport) setProtocolVersion(v string) {
	t.mu.Lock()
	t.protocol = v
	t.mu.Unlock()
}

func (t *httpTransport) done() <-chan struct{} { return t.doneCh }

func (t *httpTransport) failure() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// fail marks the transport unusable: pending and future requests error out,
// background streams stop, and done() is closed so the manager reconnects.
func (t *httpTransport) fail(err error) {
	t.mu.Lock()
	if t.err == nil {
		t.err = err
	}
	t.mu.Unlock()
	t.cancel()
	t.closeOnce.Do(func() { close(t.doneCh) })
}

// close ends the session with DELETE (best effort) and stops all streams.
func (t *httpTransport) close() error {
	t.fail(errors.New("connection closed"))
	t.mu.Lock()
	sid, gone := t.sessionID, t.sessionGone
	send := !t.deleted && sid != "" && !gone
	t.deleted = true
	t.mu.Unlock()
	if send {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		// Built like every other request so it carries the protocol version
		// and configured headers as well as the session id.
		if req, err := t.newRequest(ctx, http.MethodDelete, nil); err == nil {
			if resp, err := t.client.Do(req); err == nil {
				resp.Body.Close()
			}
		}
	}
	return nil
}

func isOAuthChallenge(h http.Header) bool {
	for _, v := range h.Values("WWW-Authenticate") {
		if strings.Contains(strings.ToLower(v), "resource_metadata=") {
			return true
		}
	}
	return false
}
