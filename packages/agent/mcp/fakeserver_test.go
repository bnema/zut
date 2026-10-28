package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// This file holds the fake MCP servers used by the package tests: a
// newline-delimited JSON-RPC stdio server (the test binary re-executes itself)
// and a streamable HTTP server built on httptest. Both share fakeBackend.

const fakeServerEnv = "ZUT_MCP_FAKE_SERVER"

// fakeToolCount is the number of tools the fake server lists in total.
const fakeToolCount = 18

var fakeImageBytes = []byte("\x89PNG\r\n\x1a\nfake-image-bytes")

func TestMain(m *testing.M) {
	switch os.Getenv(fakeServerEnv) {
	case "1":
		runFakeStdioServer()
		os.Exit(0)
	case "sleeper", "sleeper-stubborn":
		// A descendant of the fake server that outlives it.
		if os.Getenv(fakeServerEnv) == "sleeper-stubborn" {
			signal.Ignore(syscall.SIGTERM)
		}
		time.Sleep(2 * time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeBackend implements the protocol methods that do not depend on the
// transport. Behaviour switches come from the environment (stdio) or fields.
type fakeBackend struct {
	noResources bool
	protocol    string
	repeatPage  bool
	extraTool   atomic.Bool
	// manyResources makes resources/list return this many long entries.
	manyResources int
	// hugeDescription, when > 0, gives the first resource a description of
	// that many bytes.
	hugeDescription int
	events          string
}

func fakeBackendFromEnv() *fakeBackend {
	b := &fakeBackend{
		noResources: os.Getenv("FAKE_NO_RESOURCES") == "1",
		protocol:    os.Getenv("FAKE_PROTO"),
		repeatPage:  os.Getenv("FAKE_BAD_CURSOR") == "1",
		events:      os.Getenv("FAKE_EVENT_FILE"),
	}
	if n, err := strconv.Atoi(os.Getenv("FAKE_MANY_RESOURCES")); err == nil {
		b.manyResources = n
	}
	if n, err := strconv.Atoi(os.Getenv("FAKE_HUGE_DESC")); err == nil {
		b.hugeDescription = n
	}
	return b
}

func fakeToolDef(name, desc string, readOnly bool) map[string]any {
	def := map[string]any{
		"name":        name,
		"description": desc,
		"inputSchema": map[string]any{
			"$schema": "http://json-schema.org/draft-07/schema#",
			"type":    "object",
			"anyOf":   []any{map[string]any{"required": []string{"text"}}},
			"properties": map[string]any{
				"text": map[string]any{"type": "string"},
			},
		},
	}
	if readOnly {
		def["annotations"] = map[string]any{"readOnlyHint": true}
	}
	return def
}

func (b *fakeBackend) toolPage(cursor string) map[string]any {
	if cursor == "" {
		return map[string]any{
			"tools": []any{
				fakeToolDef("echo", "Echo text back", true),
				fakeToolDef("image", "Return an image", false),
				fakeToolDef("resource", "Return an embedded resource", false),
				fakeToolDef("err", "Return an isError result", false),
			},
			"nextCursor": "p2",
		}
	}
	tools := []any{
		fakeToolDef("structured", "Return structuredContent only", false),
		fakeToolDef("slow", "Block until cancelled", false),
		fakeToolDef("exit", "Kill the server process", false),
		fakeToolDef("big", "Return a very large text", false),
		fakeToolDef("ask_roots", "Ask the client for roots and ping", false),
		fakeToolDef("ask_ping", "Ask the client for a ping over the response stream", false),
		fakeToolDef("add_tool", "Add a tool and notify list_changed", false),
		fakeToolDef("getenv", "Read an environment variable", false),
		fakeToolDef("pid", "Return the server process id", false),
		fakeToolDef("spawn", "Start a descendant process and return its pid", false),
		fakeToolDef("stall", "Stop reading stdin", false),
		fakeToolDef("stderr_flood", "Write a huge stderr line", false),
		fakeToolDef("stderr_secret", "Write a stderr line with FAKE_SECRET straddling byte 4096", false),
		fakeToolDef("cwd", "Return the server working directory", false),
	}
	if b.extraTool.Load() {
		tools = append(tools, fakeToolDef("extra", "Added later", false))
	}
	page := map[string]any{"tools": tools}
	if b.repeatPage {
		page["nextCursor"] = "p2"
	}
	return page
}

func fakeText(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func fakeCallResult(content ...any) map[string]any {
	return map[string]any{"content": content}
}

// callTool handles the tools that need no transport-specific behaviour.
func (b *fakeBackend) callTool(name string, args json.RawMessage) (any, *RPCError) {
	var in struct {
		Text string `json:"text"`
		Name string `json:"name"`
	}
	_ = json.Unmarshal(args, &in)
	switch name {
	case "echo":
		return fakeCallResult(fakeText("echo: " + in.Text)), nil
	case "image":
		return fakeCallResult(fakeText("an image"), map[string]any{
			"type": "image", "mimeType": "image/png",
			"data": base64.StdEncoding.EncodeToString(fakeImageBytes),
		}), nil
	case "resource":
		return fakeCallResult(map[string]any{
			"type": "resource",
			"resource": map[string]any{
				"uri": "mem://emb", "mimeType": "text/plain", "text": "embedded body",
			},
		}), nil
	case "err":
		return map[string]any{"isError": true, "content": []any{fakeText("tool failed")}}, nil
	case "structured":
		return map[string]any{"content": []any{}, "structuredContent": map[string]any{"answer": 42}}, nil
	case "big":
		return fakeCallResult(fakeText(strings.Repeat("a", 60*1024)), fakeText("tail")), nil
	case "pid":
		return fakeCallResult(fakeText(strconv.Itoa(os.Getpid()))), nil
	case "cwd":
		dir, _ := os.Getwd()
		return fakeCallResult(fakeText(dir)), nil
	case "getenv":
		return fakeCallResult(fakeText("[" + os.Getenv(in.Name) + "]")), nil
	}
	return nil, &RPCError{Code: -32602, Message: "unknown tool: " + name}
}

func (b *fakeBackend) handle(method string, params json.RawMessage) (any, *RPCError) {
	var p struct {
		Cursor string          `json:"cursor"`
		Name   string          `json:"name"`
		Args   json.RawMessage `json:"arguments"`
		URI    string          `json:"uri"`
	}
	_ = json.Unmarshal(params, &p)
	notFound := &RPCError{Code: codeMethodNotFound, Message: "method not found: " + method}
	switch method {
	case "initialize":
		proto := b.protocol
		if proto == "" {
			proto = "2025-06-18"
		}
		caps := map[string]any{"tools": map[string]any{"listChanged": true}}
		if !b.noResources {
			caps["resources"] = map[string]any{}
		}
		return map[string]any{
			"protocolVersion": proto,
			"capabilities":    caps,
			"serverInfo":      map[string]any{"name": "fake", "version": "1.2.3"},
			"instructions":    "Be nice",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return b.toolPage(p.Cursor), nil
	case "tools/call":
		return b.callTool(p.Name, p.Args)
	case "resources/list":
		if b.noResources {
			return nil, notFound
		}
		fakeEvent(b.events, "list-resources")
		if b.hugeDescription > 0 {
			return map[string]any{"resources": []any{map[string]any{
				"uri": "mem://huge", "name": "huge", "description": strings.Repeat("h", b.hugeDescription),
			}, map[string]any{"uri": "mem://after", "name": "after"}}}, nil
		}
		if b.manyResources > 0 {
			list := make([]any, 0, b.manyResources)
			for i := 0; i < b.manyResources; i++ {
				list = append(list, map[string]any{
					"uri": fmt.Sprintf("mem://item/%d", i), "name": "item", "description": strings.Repeat("d", 100),
				})
			}
			return map[string]any{"resources": list}, nil
		}
		return map[string]any{"resources": []any{map[string]any{
			"uri": "mem://a", "name": "A", "mimeType": "text/plain", "description": "first",
		}}}, nil
	case "resources/templates/list":
		if b.noResources {
			return nil, notFound
		}
		fakeEvent(b.events, "list-templates")
		return map[string]any{"resourceTemplates": []any{map[string]any{
			"uriTemplate": "mem://item/{id}", "name": "Item",
		}}}, nil
	case "resources/read":
		if b.noResources {
			return nil, notFound
		}
		return map[string]any{"contents": []any{map[string]any{
			"uri": p.URI, "mimeType": "text/plain", "text": "content of " + p.URI,
		}}}, nil
	}
	return nil, notFound
}

// ---- stdio server ----

func fakeEvent(path, line string) {
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line + "\n")
	_ = f.Close()
}

func runFakeStdioServer() {
	backend := fakeBackendFromEnv()
	events := os.Getenv("FAKE_EVENT_FILE")
	fakeEvent(events, "start")
	if os.Getenv("FAKE_JUNK") == "1" {
		// Stray non-JSON output on stdout must be ignored by the client.
		fmt.Println("starting up... not json")
	}

	var writeMu sync.Mutex
	send := func(v any) {
		raw, err := json.Marshal(v)
		if err != nil {
			return
		}
		writeMu.Lock()
		_, _ = os.Stdout.Write(append(raw, '\n'))
		writeMu.Unlock()
	}
	respond := func(id json.RawMessage, result any, rpcErr *RPCError) {
		if frame, err := encodeResponse(id, result, rpcErr); err == nil {
			writeMu.Lock()
			_, _ = os.Stdout.Write(append(frame, '\n'))
			writeMu.Unlock()
		}
	}

	var mu sync.Mutex
	waiters := map[string]chan message{}
	cancels := map[string]context.CancelFunc{}

	// askClient sends a server->client request and waits for the answer.
	askClient := func(id, method string) (json.RawMessage, bool) {
		key := fmt.Sprintf("%q", id)
		ch := make(chan message, 1)
		mu.Lock()
		waiters[key] = ch
		mu.Unlock()
		send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method})
		select {
		case msg := <-ch:
			if msg.Error != nil {
				return json.RawMessage(`"error"`), true
			}
			return msg.Result, true
		case <-time.After(10 * time.Second):
			return nil, false
		}
	}

	serve := func(msg message) {
		key := idKey(msg.ID)
		ctx, cancel := context.WithCancel(context.Background())
		mu.Lock()
		cancels[key] = cancel
		mu.Unlock()
		defer func() {
			mu.Lock()
			delete(cancels, key)
			mu.Unlock()
			cancel()
		}()

		if msg.Method == "initialize" {
			if line := os.Getenv("FAKE_STDERR_LINE"); line != "" {
				if data, err := os.ReadFile(line); err == nil {
					fmt.Fprintln(os.Stderr, string(data))
				}
			}
			if ms, _ := time.ParseDuration(os.Getenv("FAKE_INIT_DELAY_MS") + "ms"); ms > 0 {
				time.Sleep(ms)
			}
			if os.Getenv("FAKE_INIT_ERR") == "1" {
				line := "fatal: missing credentials"
				if echo := os.Getenv("FAKE_ERR_ECHO"); echo != "" {
					line += " token=" + echo + " see http://example.invalid/x?k=" + echo
				}
				if os.Getenv("FAKE_ANSI") == "1" {
					line += " \x1b[31mred\x1b[0m\x1b]0;evil-title\x07 end"
				}
				fmt.Fprintln(os.Stderr, line)
				respond(msg.ID, nil, &RPCError{Code: -32000, Message: "init refused " + os.Getenv("FAKE_ERR_ECHO")})
				return
			}
		}
		if msg.Method == "tools/call" {
			var p struct {
				Name string          `json:"name"`
				Args json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			switch p.Name {
			case "slow":
				fakeEvent(events, "slow-started")
				select {
				case <-ctx.Done():
				case <-time.After(30 * time.Second):
					respond(msg.ID, fakeCallResult(fakeText("slow done")), nil)
				}
				return
			case "exit":
				fakeEvent(events, "exit-tool")
				os.Exit(0)
			case "spawn":
				var in struct {
					Mode string `json:"mode"`
				}
				_ = json.Unmarshal(p.Args, &in)
				child := exec.Command(os.Args[0], "-test.run=^$")
				child.Env = append(os.Environ(), fakeServerEnv+"="+in.Mode)
				if err := child.Start(); err != nil {
					respond(msg.ID, nil, &RPCError{Code: -32000, Message: err.Error()})
					return
				}
				respond(msg.ID, fakeCallResult(fakeText(strconv.Itoa(child.Process.Pid))), nil)
				return
			case "stderr_secret":
				secret := os.Getenv("FAKE_SECRET")
				pad := 4096 - len(secret)/2
				fmt.Fprintln(os.Stderr, strings.Repeat("x", pad)+secret+strings.Repeat("y", 200))
				fmt.Fprintln(os.Stderr, "after-secret")
				respond(msg.ID, fakeCallResult(fakeText("written")), nil)
				return
			case "stderr_flood":
				os.Stderr.WriteString(strings.Repeat("x", 3<<20) + "\n")
				fmt.Fprintln(os.Stderr, "after-flood")
				respond(msg.ID, fakeCallResult(fakeText("flooded")), nil)
				return
			case "ask_roots":
				roots, ok1 := askClient("srv-roots", "roots/list")
				ping, ok2 := askClient("srv-ping", "ping")
				if !ok1 || !ok2 {
					respond(msg.ID, nil, &RPCError{Code: -32000, Message: "client did not answer"})
					return
				}
				respond(msg.ID, fakeCallResult(fakeText(fmt.Sprintf("roots=%s ping=%s", roots, ping))), nil)
				return
			case "add_tool":
				backend.extraTool.Store(true)
				send(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
				respond(msg.ID, fakeCallResult(fakeText("added")), nil)
				return
			}
		}
		result, rpcErr := backend.handle(msg.Method, msg.Params)
		respond(msg.ID, result, rpcErr)
		if msg.Method == "tools/list" && os.Getenv("FAKE_NOTIFY_ON_LIST") == "1" {
			var p struct {
				Cursor string `json:"cursor"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			if p.Cursor == "p2" && !backend.extraTool.Load() {
				// The tool set changes right after the initial listing.
				backend.extraTool.Store(true)
				send(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
			}
		}
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		var msg message
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			continue
		}
		switch {
		case msg.isResponse():
			mu.Lock()
			ch := waiters[idKey(msg.ID)]
			delete(waiters, idKey(msg.ID))
			mu.Unlock()
			if ch != nil {
				ch <- msg
			}
		case msg.isRequest():
			if msg.Method == "tools/call" && bytes.Contains(msg.Params, []byte(`"name":"stall"`)) {
				// Stop reading stdin so the client's writes back up.
				fakeEvent(events, "stalled")
				time.Sleep(time.Minute)
			}
			go serve(msg)
		case msg.Method == "notifications/cancelled":
			var p struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			fakeEvent(events, "cancelled:"+idKey(p.RequestID))
			mu.Lock()
			cancel := cancels[idKey(p.RequestID)]
			mu.Unlock()
			if cancel != nil {
				cancel()
			}
		case msg.Method == "notifications/initialized":
			fakeEvent(events, "initialized")
		}
	}
	fakeEvent(events, "stdin-closed")
}

// fakeStdioConfig returns a ServerConfig that launches the fake stdio server
// and the path of its event log. extra entries become the server's Env.
func fakeStdioConfig(t *testing.T, extra map[string]string) (ServerConfig, string) {
	t.Helper()
	events := filepath.Join(t.TempDir(), "events.log")
	// GORACE: under -race the runtime sleeps at exit, which would make every
	// graceful shutdown look like it needed SIGTERM.
	env := map[string]string{fakeServerEnv: "1", "FAKE_EVENT_FILE": events, "GORACE": "atexit_sleep_ms=0"}
	for k, v := range extra {
		env[k] = v
	}
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	return ServerConfig{
		Command: exe,
		Args:    []string{"-test.run=^$"},
		Env:     env,
	}, events
}

func readEvents(path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Fields(string(raw))
}

func countEvents(path, want string) int {
	n := 0
	for _, e := range readEvents(path) {
		if e == want {
			n++
		}
	}
	return n
}

// eventually polls cond until it holds or five seconds pass.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitEvent(t *testing.T, path, prefix string) {
	t.Helper()
	eventually(t, "event "+prefix, func() bool {
		for _, e := range readEvents(path) {
			if strings.HasPrefix(e, prefix) {
				return true
			}
		}
		return false
	})
}

// ---- HTTP server ----

type httpRecord struct {
	Method    string
	Session   string
	Protocol  string
	Auth      string
	Static    string
	EnvHeader string
	RPCMethod string
}

// httpFake is a streamable HTTP MCP server. With sse set every POST answer
// is a text/event-stream, otherwise application/json.
type httpFake struct {
	backend   *fakeBackend
	sse       bool
	wantAuth  string
	challenge string
	sessionID string

	// expireNextCall makes the next tools/call that carries a session id
	// answer 404 (session expired).
	expireNextCall atomic.Bool
	// hangInit makes initialize block until the client goes away.
	hangInit bool
	// cancelGate, when set, makes notifications/cancelled block until it is
	// closed or the client disconnects.
	cancelGate chan struct{}
	// errorBody, when set, is returned with a 500 for every tools/list.
	errorBody string

	mu          sync.Mutex
	records     []httpRecord
	pingReplies chan message
	deleted     chan string
	initCount   int
	callCount   int
}

func newHTTPFake(sse bool) *httpFake {
	return &httpFake{
		backend:     &fakeBackend{},
		sse:         sse,
		sessionID:   "sess-abc",
		pingReplies: make(chan message, 4),
		deleted:     make(chan string, 4),
	}
}

func (f *httpFake) start(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv
}

func (f *httpFake) counts() (inits, calls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.initCount, f.callCount
}

func (f *httpFake) snapshot() []httpRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]httpRecord(nil), f.records...)
}

func (f *httpFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := httpRecord{
		Method:    r.Method,
		Session:   r.Header.Get("Mcp-Session-Id"),
		Protocol:  r.Header.Get("MCP-Protocol-Version"),
		Auth:      r.Header.Get("Authorization"),
		Static:    r.Header.Get("X-Static"),
		EnvHeader: r.Header.Get("X-Env"),
	}
	if f.wantAuth != "" && rec.Auth != f.wantAuth {
		f.mu.Lock()
		f.records = append(f.records, rec)
		f.mu.Unlock()
		w.Header().Set("WWW-Authenticate", f.challenge)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		f.mu.Lock()
		f.records = append(f.records, rec)
		f.mu.Unlock()
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "no server stream", http.StatusMethodNotAllowed)
	case http.MethodDelete:
		f.mu.Lock()
		f.records = append(f.records, rec)
		f.mu.Unlock()
		f.deleted <- rec.Session
		w.WriteHeader(http.StatusOK)
	case http.MethodPost:
		f.servePost(w, r, rec)
	default:
		http.Error(w, "bad method", http.StatusMethodNotAllowed)
	}
}

func (f *httpFake) servePost(w http.ResponseWriter, r *http.Request, rec httpRecord) {
	var msg message
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	rec.RPCMethod = msg.Method
	f.mu.Lock()
	f.records = append(f.records, rec)
	f.mu.Unlock()

	if msg.isResponse() {
		f.pingReplies <- msg
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !msg.isRequest() {
		if msg.Method == "notifications/cancelled" && f.cancelGate != nil {
			select {
			case <-f.cancelGate:
			case <-r.Context().Done():
			}
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	var p struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	if msg.Method == "initialize" {
		if f.hangInit {
			<-r.Context().Done()
			return
		}
		f.mu.Lock()
		f.initCount++
		n := f.initCount
		f.mu.Unlock()
		sid := f.sessionID
		if n > 1 {
			sid = fmt.Sprintf("%s-%d", f.sessionID, n)
		}
		w.Header().Set("Mcp-Session-Id", sid)
	}
	if msg.Method == "tools/call" {
		f.mu.Lock()
		f.callCount++
		f.mu.Unlock()
		if rec.Session != "" && f.expireNextCall.CompareAndSwap(true, false) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
	}
	if msg.Method == "tools/list" && f.errorBody != "" {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(f.errorBody))
		return
	}

	if msg.Method == "tools/call" && p.Name == "slow" {
		// Hold the request open until the client gives up (disconnects).
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
		return
	}

	if !f.sse {
		result, rpcErr := f.backend.handle(msg.Method, msg.Params)
		frame, _ := encodeResponse(msg.ID, result, rpcErr)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(frame)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	event := func(payload []byte) {
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
		if flusher != nil {
			flusher.Flush()
		}
	}
	fmt.Fprint(w, ": keepalive\n\n")
	if msg.Method == "tools/call" {
		// A notification before the response must not end the wait.
		note, _ := encodeNotification("notifications/message", map[string]any{"level": "info"})
		event(note)
	}
	var (
		result any
		rpcErr *RPCError
	)
	if msg.Method == "tools/call" && p.Name == "ask_ping" {
		ping, _ := encodeRequest(777, "ping", nil)
		event(ping)
		select {
		case reply := <-f.pingReplies:
			result = fakeCallResult(fakeText("ping reply: " + string(reply.Result)))
		case <-time.After(5 * time.Second):
			rpcErr = &RPCError{Code: -32000, Message: "no ping reply"}
		}
	} else {
		result, rpcErr = f.backend.handle(msg.Method, msg.Params)
	}
	frame, _ := encodeResponse(msg.ID, result, rpcErr)
	event(frame)
}
