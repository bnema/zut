package lsp

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
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResponse struct {
	Result json.RawMessage
	Error  *rpcError
}

type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// ClientOptions configures notifications and the small set of client-side
// handlers required by real language servers.
type ClientOptions struct {
	// Env is the session base environment; nil preserves process inheritance.
	Env                   []string
	Settings              map[string]any
	InitializationOptions any
	OnDiagnostics         func([]Diagnostic)
	OnLog                 func(string)
	OnProgress            func(string, json.RawMessage)
	ApplyEdit             func(WorkspaceEdit) error
}

// Client is one stdio JSON-RPC LSP process. A Client is safe for concurrent
// requests; the process itself is shared by all open documents in its root.
type Client struct {
	Spec    ServerConfig
	Root    string
	Command string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stderr  io.ReadCloser

	writeMu     sync.Mutex
	applyEditMu sync.Mutex
	mu          sync.Mutex
	pending     map[int64]chan rpcResponse
	nextID      int64
	closed      bool
	closeOnce   sync.Once
	done        chan struct{}
	doneOnce    sync.Once
	readErr     atomic.Value // string; one concrete type keeps atomic.Value safe

	options ClientOptions
	capMu   sync.RWMutex
	caps    json.RawMessage
	docsMu  sync.Mutex
	docs    map[string]int
}

// NewClient starts a stdio server but does not initialize it. This separation
// is convenient for tests and lets callers choose an initialization timeout.
func NewClient(spec ServerConfig, root string) (*Client, error) {
	return NewClientWithOptions(spec, root, ClientOptions{})
}

// NewClientWithOptions starts a client with notification callbacks installed.
func NewClientWithOptions(spec ServerConfig, root string, options ClientOptions) (*Client, error) {
	if strings.EqualFold(spec.Kind, "cli") {
		return nil, fmt.Errorf("%s is a CLI provider, not an LSP server", spec.ID)
	}
	if options.Env != nil && !filepath.IsAbs(root) {
		return nil, fmt.Errorf("session root must be absolute: %q", root)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	command, err := ResolveCommand(root, spec.Command, options.Env)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", spec.ID, err)
	}
	cmd := exec.Command(command, spec.Args...)
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = root
	cmd.Env = mergedEnvironment(spec.Env, options.Env)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("LSP stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("LSP stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("LSP stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start %s: %w", spec.ID, err)
	}
	c := &Client{
		Spec: spec, Root: root, Command: command, cmd: cmd,
		stdin: stdin, stdout: stdout, stderr: stderr,
		pending: make(map[int64]chan rpcResponse), done: make(chan struct{}),
		options: options, docs: make(map[string]int),
	}
	go c.readLoop()
	go c.stderrLoop()
	return c, nil
}

// NewClientWithContext starts and initializes a client.
func NewClientWithContext(ctx context.Context, spec ServerConfig, root string, options ClientOptions) (*Client, error) {
	client, err := NewClientWithOptions(spec, root, options)
	if err != nil {
		return nil, err
	}
	if err := client.Initialize(ctx); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

func (c *Client) Initialize(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	folders := []map[string]any{{"uri": mustURI(c.Root), "name": filepath.Base(c.Root)}}
	params := map[string]any{
		"processId":        os.Getpid(),
		"rootUri":          mustURI(c.Root),
		"workspaceFolders": folders,
		"capabilities": map[string]any{
			"workspace": map[string]any{
				"applyEdit":        true,
				"workspaceFolders": true,
				"configuration":    true,
			},
			"textDocument": map[string]any{
				"publishDiagnostics": map[string]any{"relatedInformation": true},
			},
		},
		"initializationOptions": c.options.InitializationOptions,
		"clientInfo":            map[string]any{"name": "zut", "version": "lsp"},
	}
	result, err := c.Request(ctx, "initialize", params)
	if err != nil {
		return fmt.Errorf("initialize %s: %w", c.Spec.ID, err)
	}
	var response struct {
		Capabilities json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(result, &response); err == nil {
		c.capMu.Lock()
		c.caps = append(c.caps[:0], response.Capabilities...)
		c.capMu.Unlock()
	}
	if err := c.Notify("initialized", map[string]any{}); err != nil {
		return err
	}
	return nil
}

// Capabilities returns the initialize result's capabilities.
func (c *Client) Capabilities() json.RawMessage {
	c.capMu.RLock()
	defer c.capMu.RUnlock()
	return append(json.RawMessage(nil), c.caps...)
}

// Request sends a raw JSON-RPC request and waits for its response.
func (c *Client) Request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	id := atomic.AddInt64(&c.nextID, 1)
	responseCh := make(chan rpcResponse, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("LSP client is closed")
	}
	c.pending[id] = responseCh
	c.mu.Unlock()
	request := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	if err := c.send(request); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	select {
	case response := <-responseCh:
		if response.Error != nil {
			return nil, fmt.Errorf("LSP %s (%d): %s", method, response.Error.Code, response.Error.Message)
		}
		return response.Result, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		_ = c.Notify("$/cancelRequest", map[string]any{"id": id})
		return nil, ctx.Err()
	case <-c.done:
		return nil, c.processError()
	}
}

// Notify sends a JSON-RPC notification.
func (c *Client) Notify(method string, params any) error {
	return c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *Client) send(message any) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return errors.New("LSP client is closed")
	}
	if err := WriteMessage(c.stdin, payload); err != nil {
		return fmt.Errorf("write LSP message: %w", err)
	}
	return nil
}

// DidOpen publishes a complete document. Callers which already opened the URI
// should use DidChange for subsequent full-content updates.
func (c *Client) DidOpen(path, languageID, text string) error {
	uri, err := pathToURI(path)
	if err != nil {
		return err
	}
	version, alreadyOpen := c.nextDocumentVersion(uri)
	if alreadyOpen {
		return c.Notify("textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": uri, "version": version},
			"contentChanges": []map[string]any{{"text": text}},
		})
	}
	return c.Notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": uri, "languageId": languageID, "version": version, "text": text},
	})
}

// DidChange sends a full-content change, which avoids version/range drift
// between zut and servers that use different incremental-sync policies.
func (c *Client) DidChange(path, text string) error {
	uri, err := pathToURI(path)
	if err != nil {
		return err
	}
	version, _ := c.nextDocumentVersion(uri)
	return c.Notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": version},
		"contentChanges": []map[string]any{{"text": text}},
	})
}

func (c *Client) nextDocumentVersion(uri string) (int, bool) {
	c.docsMu.Lock()
	defer c.docsMu.Unlock()
	version := c.docs[uri]
	alreadyOpen := version != 0
	if version == 0 {
		version = 1
	} else {
		version++
	}
	c.docs[uri] = version
	return version, alreadyOpen
}

func (c *Client) DidSave(path, text string) error {
	uri, err := pathToURI(path)
	if err != nil {
		return err
	}
	params := map[string]any{"textDocument": map[string]any{"uri": uri}}
	if text != "" {
		params["text"] = text
	}
	return c.Notify("textDocument/didSave", params)
}

func (c *Client) readLoop() {
	reader := bufio.NewReaderSize(c.stdout, 64*1024)
	for {
		payload, err := ReadMessage(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				c.readErr.Store(err.Error())
			}
			c.failPending(err)
			c.signalDone()
			return
		}
		var envelope rpcEnvelope
		if err := json.Unmarshal(payload, &envelope); err != nil {
			c.readErr.Store(fmt.Sprintf("decode LSP message: %v", err))
			continue
		}
		if envelope.Method == "" && len(envelope.ID) > 0 {
			var id int64
			if json.Unmarshal(envelope.ID, &id) == nil {
				c.mu.Lock()
				channel := c.pending[id]
				delete(c.pending, id)
				c.mu.Unlock()
				if channel != nil {
					channel <- rpcResponse{Result: envelope.Result, Error: envelope.Error}
				}
			}
			continue
		}
		if envelope.Method != "" {
			// Notifications carry ordered state, so handle them on the
			// reader goroutine. Server requests may block while they wait
			// for an edit response, so those remain asynchronous.
			if len(envelope.ID) == 0 || string(envelope.ID) == "null" {
				c.handleServerMessage(envelope)
			} else {
				go c.handleServerMessage(envelope)
			}
		}
	}
}

func (c *Client) handleServerMessage(message rpcEnvelope) {
	switch message.Method {
	case "textDocument/publishDiagnostics":
		c.handleDiagnostics(message.Params)
		return
	case "$/progress":
		if c.options.OnProgress != nil {
			c.options.OnProgress(message.Method, message.Params)
		}
		return
	case "window/logMessage", "window/showMessage", "telemetry/event":
		if c.options.OnLog != nil {
			var value struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(message.Params, &value)
			c.options.OnLog(value.Message)
		}
		if len(message.ID) == 0 || string(message.ID) == "null" {
			return
		}
		_ = c.respond(message.ID, nil, nil)
		return
	case "workspace/configuration":
		_ = c.respond(message.ID, c.configuration(message.Params), nil)
	case "workspace/workspaceFolders":
		_ = c.respond(message.ID, []map[string]any{{"uri": mustURI(c.Root), "name": filepath.Base(c.Root)}}, nil)
	case "workspace/applyEdit":
		var params struct {
			Edit WorkspaceEdit `json:"edit"`
		}
		if err := json.Unmarshal(message.Params, &params); err != nil {
			_ = c.respond(message.ID, map[string]any{"applied": false, "failureReason": err.Error()}, nil)
			return
		}
		c.applyEditMu.Lock()
		err := c.applyEdit(params.Edit)
		c.applyEditMu.Unlock()
		result := map[string]any{"applied": err == nil}
		if err != nil {
			result["failureReason"] = err.Error()
		}
		_ = c.respond(message.ID, result, nil)
	case "client/registerCapability", "window/workDoneProgress/create":
		_ = c.respond(message.ID, map[string]any{}, nil)
	default:
		if len(message.ID) > 0 && string(message.ID) != "null" {
			_ = c.respond(message.ID, nil, &rpcError{Code: -32601, Message: "method not supported"})
		}
	}
}

func (c *Client) configuration(params json.RawMessage) []any {
	var request struct {
		Items []struct {
			Section string `json:"section"`
		} `json:"items"`
	}
	_ = json.Unmarshal(params, &request)
	out := make([]any, len(request.Items))
	for i, item := range request.Items {
		if item.Section == "" {
			out[i] = c.options.Settings
			continue
		}
		out[i] = lookupSetting(c.options.Settings, item.Section)
	}
	return out
}

func lookupSetting(settings map[string]any, section string) any {
	if settings == nil {
		return nil
	}
	if value, ok := settings[section]; ok {
		return value
	}
	var value any = settings
	for _, part := range strings.Split(section, ".") {
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value, ok = object[part]
		if !ok {
			return nil
		}
	}
	return value
}

func (c *Client) applyEdit(edit WorkspaceEdit) error {
	if c.options.ApplyEdit != nil {
		return c.options.ApplyEdit(edit)
	}
	return ApplyWorkspaceEdit(c.Root, edit)
}

func (c *Client) respond(id json.RawMessage, result any, rpcErr *rpcError) error {
	message := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		message["error"] = rpcErr
	} else {
		message["result"] = result
	}
	return c.send(message)
}

func (c *Client) handleDiagnostics(params json.RawMessage) {
	var value struct {
		URI         string       `json:"uri"`
		Diagnostics []Diagnostic `json:"diagnostics"`
	}
	if err := json.Unmarshal(params, &value); err != nil {
		return
	}
	path, err := uriToPath(value.URI)
	if err != nil {
		return
	}
	for i := range value.Diagnostics {
		value.Diagnostics[i].Path = path
		value.Diagnostics[i].Server = c.Spec.ID
	}
	if len(value.Diagnostics) == 0 {
		// Preserve the URI on a clear so Manager can remove only this file's
		// snapshot rather than accidentally retaining stale diagnostics.
		value.Diagnostics = []Diagnostic{{Path: path, Server: c.Spec.ID, Clear: true}}
	}
	if c.options.OnDiagnostics != nil {
		c.options.OnDiagnostics(value.Diagnostics)
	}
}

func (c *Client) stderrLoop() {
	if c.stderr == nil {
		return
	}
	var output limitedBuffer
	output.limit = 1 << 20
	_, _ = io.Copy(&output, c.stderr)
	if output.Len() > 0 && c.options.OnLog != nil {
		c.options.OnLog(strings.TrimSpace(output.String()))
	}
}

func (c *Client) failPending(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, channel := range c.pending {
		delete(c.pending, id)
		channel <- rpcResponse{Error: &rpcError{Code: -32000, Message: err.Error()}}
	}
}

func (c *Client) processError() error {
	if err, ok := c.readErr.Load().(string); ok && err != "" {
		return fmt.Errorf("LSP process stopped: %s", err)
	}
	return errors.New("LSP process stopped")
}

func (c *Client) signalDone() { c.doneOnce.Do(func() { close(c.done) }) }

// Close stops the process and releases all pipes. It is safe to call more
// than once and is also used by Manager.Reload.
func (c *Client) Close() error {
	var waitErr error
	c.closeOnce.Do(func() {
		// Give a cooperative server a short shutdown window before killing
		// the process. Close remains bounded for servers that do not support
		// shutdown or are already wedged.
		if c.cmd.ProcessState == nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			_, _ = c.Request(shutdownCtx, "shutdown", nil)
			cancel()
			_ = c.Notify("exit", nil)
		}
		// Serialize the closed transition with send so a writer that has
		// passed its closed check cannot race stdin.Close.
		c.writeMu.Lock()
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		c.signalDone()
		_ = c.stdin.Close()
		c.writeMu.Unlock()
		killed := false
		process := c.cmd.Process
		exited := make(chan error, 1)
		go func() { exited <- c.cmd.Wait() }()
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case waitErr = <-exited:
			timer.Stop()
		case <-timer.C:
			if process != nil {
				killed = process.Kill() == nil
			}
			waitErr = <-exited
		}
		if killed {
			// A process terminated by this cleanup path normally returns an
			// ExitError; preserve unrelated wait failures such as pipe
			// errors so callers still get useful lifecycle diagnostics.
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				waitErr = nil
			}
		}
		_ = c.stdout.Close()
		_ = c.stderr.Close()
		c.failPending(errors.New("LSP client closed"))
	})
	return waitErr
}

func mustURI(path string) string {
	uri, _ := pathToURI(path)
	return uri
}

func mergedEnvironment(overrides map[string]string, base ...[]string) []string {
	env := os.Environ()
	if len(base) > 0 && base[0] != nil {
		env = append([]string{}, base[0]...)
	}
	if len(overrides) == 0 {
		return env
	}
	index := make(map[string]int, len(env))
	for i, item := range env {
		if key, _, ok := strings.Cut(item, "="); ok {
			index[key] = i
		}
	}
	for key, value := range overrides {
		item := key + "=" + value
		if i, ok := index[key]; ok {
			env[i] = item
		} else {
			env = append(env, item)
		}
	}
	return env
}

// ResolveCommand searches workspace-local tool directories before PATH.
func ResolveCommand(cwd, command string, base ...[]string) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", errors.New("empty command")
	}
	cwd, _ = filepath.Abs(cwd)
	candidates := []string{}
	if filepath.IsAbs(command) || strings.ContainsAny(command, `/\\`) {
		if filepath.IsAbs(command) {
			candidates = append(candidates, command)
		} else {
			candidates = append(candidates, filepath.Join(cwd, command))
		}
	} else {
		// A model often runs in a package subdirectory while the project's
		// executable lives at the repository root. Search each workspace
		// ancestor before falling back to PATH.
		for dir := cwd; dir != ""; dir = filepath.Dir(dir) {
			for _, local := range []string{filepath.Join("node_modules", ".bin"), filepath.Join(".venv", "bin"), filepath.Join(".venv", "Scripts"), "bin", "Scripts"} {
				candidates = append(candidates, filepath.Join(dir, local, command))
			}
			next := filepath.Dir(dir)
			if next == dir {
				break
			}
		}
	}
	for _, candidate := range candidates {
		for _, path := range executableVariants(candidate) {
			if isRunnable(path) {
				return path, nil
			}
		}
	}
	if len(base) > 0 && base[0] != nil {
		for _, item := range base[0] {
			k, v, ok := strings.Cut(item, "=")
			if !ok || !strings.EqualFold(k, "PATH") {
				continue
			}
			for _, dir := range filepath.SplitList(v) {
				if !filepath.IsAbs(dir) {
					dir = filepath.Join(cwd, dir)
				}
				for _, path := range executableVariants(filepath.Join(dir, command)) {
					if isRunnable(path) {
						return path, nil
					}
				}
			}
		}
		return "", fmt.Errorf("command %q not found in session PATH", command)
	}
	if path, err := exec.LookPath(command); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("command %q not found in node_modules/.bin, .venv/bin, bin, or PATH", command)
}

func executableVariants(path string) []string {
	if filepath.Ext(path) != "" {
		return []string{path}
	}
	return []string{path, path + ".exe", path + ".cmd", path + ".bat"}
}

func isRunnable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if info.Mode()&0111 != 0 || filepath.Ext(path) == ".cmd" || filepath.Ext(path) == ".bat" || filepath.Ext(path) == ".exe" {
		return true
	}
	return false
}

// WaitForDiagnostics gives asynchronous LSP servers a bounded opportunity to
// publish diagnostics after didOpen/didChange.
func (c *Client) WaitForDiagnostics(ctx context.Context, delay time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.processError()
	}
}
