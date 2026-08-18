package provider

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// responsesWebSocketClient uses the public Responses WebSocket mode for one
// logical session at a time. It deliberately wraps only the public OpenAI
// Responses client: ChatGPT Codex and OpenAI-compatible endpoints have
// different authentication and cache contracts and must retain their HTTP/SSE
// transport until they independently declare WebSocket support.
type responsesWebSocketClient struct {
	http *codexClient

	mu       sync.Mutex
	sessions map[string]*responsesWebSocketSession
	dial     func(context.Context) (*websocket.Conn, error)
}

type responsesWebSocketSession struct {
	mu sync.Mutex

	turn chan struct{}

	conn       *websocket.Conn
	raw        chan sseEvent
	readerDone chan struct{}

	// lastRequest is the complete request accepted by the server. Continuation
	// must compare every response-context setting, not merely its transcript.
	lastRequest    Request
	lastOutput     Message
	lastResponseID string
}

func newResponsesWebSocketClient(httpClient *codexClient) Client {
	c := &responsesWebSocketClient{
		http:     httpClient,
		sessions: make(map[string]*responsesWebSocketSession),
	}
	c.dial = c.dialResponses
	return c
}

func (c *responsesWebSocketClient) Name() string { return c.http.Name() }

// Close releases all persistent sockets held by this client. Callers that
// create a client for a bounded runtime (such as a resident child) must call
// this when that runtime stops; otherwise a warm session can outlive its
// owner until the process exits.
func (c *responsesWebSocketClient) Close() error {
	c.mu.Lock()
	sessions := c.sessions
	c.sessions = make(map[string]*responsesWebSocketSession)
	c.mu.Unlock()
	for _, session := range sessions {
		session.invalidate()
	}
	return nil
}

func (c *responsesWebSocketClient) session(id string) *responsesWebSocketSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	if session := c.sessions[id]; session != nil {
		return session
	}
	session := &responsesWebSocketSession{turn: make(chan struct{}, 1)}
	session.turn <- struct{}{}
	c.sessions[id] = session
	return session
}

func (c *responsesWebSocketClient) dialResponses(ctx context.Context) (*websocket.Conn, error) {
	endpoint, err := responsesWebSocketURL(c.http.baseURL)
	if err != nil {
		return nil, err
	}
	config, err := websocket.NewConfig(endpoint, "https://api.openai.com")
	if err != nil {
		return nil, err
	}
	config.Header = make(http.Header)
	config.Header.Set("authorization", "Bearer "+c.http.token)
	if tlsConfig := responsesWebSocketTLSConfig(c.http.http); tlsConfig != nil {
		config.TlsConfig = tlsConfig
	}
	return config.DialContext(ctx)
}

// responsesWebSocketTLSConfig carries the explicit TLS policy supplied by
// WithHTTPClient into the WebSocket dial.
func responsesWebSocketTLSConfig(client *http.Client) *tls.Config {
	if client == nil {
		return nil
	}
	transport := client.Transport
	if public, ok := transport.(*openaiResponsesTransport); ok {
		transport = public.inner
	}
	if transport, ok := transport.(*http.Transport); ok && transport.TLSClientConfig != nil {
		return transport.TLSClientConfig.Clone()
	}
	return nil
}

// responsesWebSocketHTTPCompatible refuses to bypass an injected HTTP
// transport. A raw WebSocket dial can faithfully carry a TLS policy but not a
// custom RoundTripper or configured proxy, so those configurations retain the
// established HTTP/SSE transport instead.
func responsesWebSocketHTTPCompatible(client *http.Client) bool {
	if client == nil || client.Transport == nil {
		return true
	}
	transport := client.Transport
	if public, ok := transport.(*openaiResponsesTransport); ok {
		transport = public.inner
	}
	base, ok := transport.(*http.Transport)
	if !ok {
		return false
	}
	if base.Proxy == nil {
		return true
	}
	request, _ := http.NewRequest(http.MethodGet, "https://api.openai.com", nil)
	proxy, err := base.Proxy(request)
	return err == nil && proxy == nil
}

func responsesWebSocketURL(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse Responses endpoint: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("unsupported Responses endpoint scheme %q", u.Scheme)
	}
	return u.String(), nil
}

func (c *responsesWebSocketClient) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	// WebSocket continuation state is isolated per conversation thread. Cache
	// affinity may be shared by root and resident children, but their server
	// response chains must never be pooled.
	threadID := strings.TrimSpace(req.Context.ThreadID)
	if threadID == "" {
		return c.http.Stream(ctx, req)
	}
	cacheSessionID := strings.TrimSpace(req.Context.CacheSessionID)

	session := c.session(threadID)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-session.turn:
	}

	release := true
	defer func() {
		if release {
			session.turn <- struct{}{}
		}
	}()

	streamReq, previousResponseID := session.incrementalRequest(req)
	payload, err := c.websocketPayload(streamReq, cacheSessionID, previousResponseID)
	if err == nil {
		wire, wireErr := c.http.buildRequest(streamReq)
		if wireErr == nil {
			continuation := "full"
			if previousResponseID != "" {
				continuation = "incremental"
			}
			ReportCacheDiagnostics(req.Lifecycle, responsesCacheDiagnostics(wire, "websocket", continuation))
		}
	}
	if err != nil {
		return nil, err
	}

	if !responsesWebSocketHTTPCompatible(c.http.http) {
		return c.http.Stream(ctx, req)
	}
	reportRequestAttempt(req.Lifecycle, 1, 2)
	if err := session.ensureConnected(ctx, c.dial); err != nil {
		// A connection that never became usable has no server-side turn. The
		// SSE fallback therefore receives the complete request and preserves
		// the same session-derived cache key without sharing WebSocket state.
		session.invalidate()
		return c.http.Stream(ctx, req)
	}
	conn, raw := session.connection()
	if err := websocket.Message.Send(conn, string(payload)); err != nil {
		session.invalidate()
		return c.http.Stream(ctx, req)
	}

	out := make(chan Event, 16)
	release = false
	go func() {
		defer func() { session.turn <- struct{}{} }()
		defer close(out)

		first, responseID, ok := awaitResponsesWebSocketResponseStart(ctx, raw)
		if !ok && ctx.Err() != nil {
			session.invalidateConnection(conn)
			c.http.runResponseEvents(ctx, req, out, make(chan sseEvent), nil)
			return
		}
		if !ok || responseWebSocketRecoveryEvent(first) {
			// The server may discard a response ID or close an idle connection.
			// Reconnect once with the full transcript before falling back to
			// HTTP/SSE. That keeps the stable cache key and logical IDs intact
			// without ever reusing the invalid previous_response_id.
			session.invalidate()
			reconnected, reconnectRaw, reconnectFirst, reconnectResponseID, err := c.reconnectFull(ctx, session, req, cacheSessionID)
			if err != nil {
				c.forwardResponsesHTTP(ctx, req, out)
				return
			}
			conn, raw, first, responseID = reconnected, reconnectRaw, reconnectFirst, reconnectResponseID
		}
		var stateMu sync.Mutex
		terminal := false
		finished := make(chan struct{})
		go func(conn *websocket.Conn) {
			select {
			case <-ctx.Done():
				stateMu.Lock()
				if !terminal {
					session.invalidateConnection(conn)
				}
				stateMu.Unlock()
			case <-finished:
			}
		}(conn)

		completed := false
		turnRaw := filterResponsesWebSocketEvents(raw, responseID, finished)
		c.http.runResponseEventsWithFirst(ctx, req, out, turnRaw, &first, func(responseID string, output Message) {
			stateMu.Lock()
			terminal = true
			stateMu.Unlock()
			completed = true
			session.recordCompleted(req, output, responseID)
		})
		close(finished)
		if !completed {
			session.invalidate()
		}
	}()
	return out, nil
}

func (c *responsesWebSocketClient) websocketPayload(req Request, cacheSessionID, previousResponseID string) ([]byte, error) {
	wire, err := c.http.buildRequest(req)
	if err != nil {
		return nil, err
	}
	if c.http.modelName != nil {
		wire.Model = c.http.modelName(wire.Model)
	}
	if c.http.capabilities.StablePromptCacheKey && cacheSessionID != "" {
		wire.PromptCacheKey = cacheSessionID
	}
	wire.Stream = false // WebSocket mode does not send HTTP stream controls.
	wire.PreviousResponseID = previousResponseID
	return json.Marshal(struct {
		Type string `json:"type"`
		*codexRequest
	}{Type: "response.create", codexRequest: wire})
}

// reconnectFull establishes one replacement WebSocket turn after a cached
// continuation failed. A fresh connection cannot rely on the previous server
// response, so it always sends the complete local transcript.
func (c *responsesWebSocketClient) reconnectFull(ctx context.Context, session *responsesWebSocketSession, req Request, cacheSessionID string) (*websocket.Conn, <-chan sseEvent, sseEvent, string, error) {
	payload, err := c.websocketPayload(req, cacheSessionID, "")
	if err != nil {
		return nil, nil, sseEvent{}, "", err
	}
	reportRequestAttempt(req.Lifecycle, 2, 2)
	if err := session.ensureConnected(ctx, c.dial); err != nil {
		return nil, nil, sseEvent{}, "", err
	}
	conn, raw := session.connection()
	if err := websocket.Message.Send(conn, string(payload)); err != nil {
		session.invalidateConnection(conn)
		return nil, nil, sseEvent{}, "", err
	}
	first, responseID, ok := awaitResponsesWebSocketResponseStart(ctx, raw)
	if !ok || responseWebSocketRecoveryEvent(first) {
		session.invalidateConnection(conn)
		if first.Err != nil {
			return nil, nil, sseEvent{}, "", first.Err
		}
		return nil, nil, sseEvent{}, "", fmt.Errorf("responses WebSocket reconnect did not produce a usable event")
	}
	return conn, raw, first, responseID, nil
}

func awaitResponsesWebSocketEvent(ctx context.Context, raw <-chan sseEvent) (sseEvent, bool) {
	select {
	case <-ctx.Done():
		return sseEvent{}, false
	case event, ok := <-raw:
		return event, ok
	}
}

// awaitResponsesWebSocketResponseStart discards frames left behind by an
// earlier response until the Responses protocol announces the response this
// request created. The API runs responses sequentially on a socket, but this
// guard keeps a late terminal frame from completing the next local turn.
func awaitResponsesWebSocketResponseStart(ctx context.Context, raw <-chan sseEvent) (sseEvent, string, bool) {
	for {
		event, ok := awaitResponsesWebSocketEvent(ctx, raw)
		if !ok || responseWebSocketRecoveryEvent(event) {
			return event, "", ok
		}
		var payload struct {
			Type     string `json:"type"`
			Response struct {
				ID string `json:"id"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
			continue
		}
		if payload.Type != "response.created" {
			continue
		}
		if strings.TrimSpace(payload.Response.ID) == "" {
			return sseEvent{Err: fmt.Errorf("responses WebSocket created event has no response ID")}, "", false
		}
		return event, payload.Response.ID, true
	}
}

// filterResponsesWebSocketEvents retains only frames for the response that
// started this turn when the protocol supplies an identity. Regular Responses
// streaming events do not all repeat response_id; the preceding created-event
// barrier is therefore also required by the sequential WebSocket contract.
func filterResponsesWebSocketEvents(raw <-chan sseEvent, responseID string, done <-chan struct{}) <-chan sseEvent {
	out := make(chan sseEvent, 16)
	go func() {
		defer close(out)
		for {
			select {
			case <-done:
				return
			case event, ok := <-raw:
				if !ok {
					return
				}
				if !responseWebSocketEventMatches(event, responseID) {
					continue
				}
				select {
				case <-done:
					return
				case out <- event:
				}
			}
		}
	}()
	return out
}

func responseWebSocketEventMatches(event sseEvent, responseID string) bool {
	if event.Err != nil {
		return true
	}
	var payload struct {
		ResponseID string `json:"response_id"`
		Response   struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	if json.Unmarshal([]byte(event.Data), &payload) != nil {
		return true
	}
	if payload.ResponseID != "" && payload.ResponseID != responseID {
		return false
	}
	return payload.Response.ID == "" || payload.Response.ID == responseID
}

func responseWebSocketRecoveryEvent(event sseEvent) bool {
	if event.Err != nil {
		return true
	}
	var payload struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(event.Data), &payload) != nil || payload.Type != "error" {
		return false
	}
	return true
}

func (c *responsesWebSocketClient) forwardResponsesHTTP(ctx context.Context, req Request, out chan<- Event) {
	stream, err := c.http.Stream(ctx, req)
	if err != nil {
		providerName := c.http.providerName
		if providerName == "" {
			providerName = "openai"
		}
		out <- EventStart{Model: req.Model, Provider: providerName}
		out <- EventDone{Stop: StopError, Err: err}
		return
	}
	for event := range stream {
		out <- event
	}
}

func (s *responsesWebSocketSession) incrementalRequest(req Request) (Request, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.lastRequest
	if s.lastResponseID == "" || !sameResponsesContinuationConfig(previous, req) || len(req.Messages) < len(previous.Messages) || !reflect.DeepEqual(req.Messages[:len(previous.Messages)], previous.Messages) {
		return req, ""
	}
	// The cached response already contains the exact assistant output from the
	// last generation. It must be replayed verbatim in the caller transcript
	// before we can safely send only the strict new tool-result/user suffix.
	req.Messages = append([]Message(nil), req.Messages[len(previous.Messages):]...)
	if len(req.Messages) > 0 && req.Messages[0].Role == RoleAssistant {
		if !sameResponsesOutput(req.Messages[0], s.lastOutput) {
			return req, ""
		}
		req.Messages = req.Messages[1:]
	}
	return req, s.lastResponseID
}

func sameResponsesOutput(current, previous Message) bool {
	// Provider message timestamps are local presentation metadata, not server
	// response state. All visible content, call IDs, reasoning replay, and
	// metadata remain part of the strict baseline.
	current.Time = time.Time{}
	previous.Time = time.Time{}
	return reflect.DeepEqual(current, previous)
}

func sameResponsesContinuationConfig(previous, current Request) bool {
	previous.Messages = nil
	previous.Context.TurnID = ""
	previous.Lifecycle = nil
	current.Messages = nil
	current.Context.TurnID = ""
	current.Lifecycle = nil
	return reflect.DeepEqual(previous, current)
}

func (s *responsesWebSocketSession) ensureConnected(ctx context.Context, dial func(context.Context) (*websocket.Conn, error)) error {
	s.mu.Lock()
	if s.conn != nil && s.raw != nil {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	conn, err := dial(ctx)
	if err != nil {
		return err
	}
	raw := make(chan sseEvent, 16)
	readerDone := make(chan struct{})
	s.mu.Lock()
	s.conn = conn
	s.raw = raw
	s.readerDone = readerDone
	s.mu.Unlock()
	go readResponsesWebSocket(conn, raw, readerDone)
	return nil
}

func (s *responsesWebSocketSession) connection() (*websocket.Conn, <-chan sseEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn, s.raw
}

func (s *responsesWebSocketSession) recordCompleted(request Request, output Message, responseID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	request.Messages = append([]Message(nil), request.Messages...)
	request.Lifecycle = nil
	s.lastRequest = request
	s.lastOutput = output
	s.lastResponseID = responseID
}

func (s *responsesWebSocketSession) invalidateConnection(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != conn {
		return
	}
	s.invalidateLocked()
}

func (s *responsesWebSocketSession) invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidateLocked()
}

func (s *responsesWebSocketSession) invalidateLocked() {
	if s.readerDone != nil {
		close(s.readerDone)
	}
	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.conn = nil
	s.raw = nil
	s.readerDone = nil
	s.lastRequest = Request{}
	s.lastOutput = Message{}
	s.lastResponseID = ""
}

func readResponsesWebSocket(conn *websocket.Conn, out chan<- sseEvent, done <-chan struct{}) {
	defer close(out)
	for {
		var data json.RawMessage
		if err := websocket.JSON.Receive(conn, &data); err != nil {
			select {
			case out <- sseEvent{Err: err}:
			case <-done:
			}
			return
		}
		select {
		case out <- sseEvent{Data: string(data)}:
		case <-done:
			return
		}
	}
}
