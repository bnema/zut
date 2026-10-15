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
	"os"
	"strings"
	"sync"
	"time"
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

	mu        sync.Mutex
	sessionID string
	protocol  string
	handler   incomingHandler
	err       error

	ctx       context.Context
	cancel    context.CancelFunc
	doneCh    chan struct{}
	closeOnce sync.Once
	listening bool
}

func newHTTPTransport(name string, cfg ServerConfig, client *http.Client) (*httpTransport, error) {
	headers := map[string]string{}
	for k, v := range cfg.Headers {
		headers[k] = v
	}
	for header, envName := range cfg.EnvHeaders {
		if v, ok := os.LookupEnv(envName); ok && v != "" {
			headers[header] = v
		}
	}
	if cfg.BearerTokenEnvVar != "" {
		token, ok := os.LookupEnv(cfg.BearerTokenEnvVar)
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
		return nil, err
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

func (t *httpTransport) post(ctx context.Context, frame []byte) (*http.Response, error) {
	req, err := t.newRequest(ctx, http.MethodPost, frame)
	if err != nil {
		return nil, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	if sid := resp.Header.Get(headerSessionID); sid != "" {
		t.mu.Lock()
		t.sessionID = sid
		t.mu.Unlock()
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, httpStatusError(resp.StatusCode, snippet)
	}
	return resp, nil
}

// HTTPStatusError reports a non-success HTTP status from a server.
type HTTPStatusError struct {
	Status int
	Body   string
}

func (e *HTTPStatusError) Error() string {
	msg := fmt.Sprintf("http %d %s", e.Status, http.StatusText(e.Status))
	if e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden {
		msg += " (check headers, envHeaders, or bearerTokenEnvVar)"
	}
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

func httpStatusError(status int, body []byte) error {
	text := strings.TrimSpace(string(body))
	if len(text) > 300 {
		text = text[:300] + "…"
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
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(t.ctx, cancel)
	defer stop()
	resp, err := t.post(reqCtx, frame)
	if err != nil {
		if ctx.Err() != nil {
			_ = t.notify(context.Background(), "notifications/cancelled", map[string]any{"requestId": id, "reason": ctx.Err().Error()})
			return nil, ctx.Err()
		}
		if t.ctx.Err() != nil {
			return nil, errors.New("connection closed")
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
		raw, err = io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody))
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
			_ = t.notify(context.Background(), "notifications/cancelled", map[string]any{"requestId": id, "reason": ctx.Err().Error()})
			return nil, ctx.Err()
		}
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}
		return nil, errors.New("server closed the response without a result")
	}
	if result.Error != nil {
		return nil, result.Error
	}
	return result.Result, nil
}

// readSSEFrames delivers each SSE data payload as JSON-RPC frames until
// handle returns true or the stream ends.
func readSSEFrames(r io.Reader, handle func(message) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxHTTPBody)
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
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		}
	}
	if flush() {
		return nil
	}
	return sc.Err()
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

// close ends the session with DELETE (best effort) and stops all streams.
func (t *httpTransport) close() error {
	t.mu.Lock()
	if t.err == nil {
		t.err = errors.New("connection closed")
	}
	sid := t.sessionID
	t.mu.Unlock()
	t.cancel()
	t.closeOnce.Do(func() { close(t.doneCh) })
	if sid != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.url, nil); err == nil {
			for k, v := range t.headers {
				req.Header.Set(k, v)
			}
			req.Header.Set(headerSessionID, sid)
			if resp, err := t.client.Do(req); err == nil {
				resp.Body.Close()
			}
		}
	}
	return nil
}
