package lsp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ManagerOptions controls process and command timeouts.
type ManagerOptions struct {
	RequestTimeout  time.Duration
	DiagnosticDelay time.Duration
	LinterTimeout   time.Duration
	Settings        map[string]any
	OnLog           func(server, message string)
	CheckWritePath  func(path string) error
}

// Response is one raw request result tagged with the server which produced
// it. Errors are retained so a partially installed workspace remains useful.
type Response struct {
	Server string
	Result json.RawMessage
	Error  error
}

type workspace struct {
	cwd            string
	rootReal       string
	config         Config
	clients        map[string]*Client
	starting       map[string]chan struct{}
	statuses       map[string]string
	errors         map[string]string
	diagnostics    map[string]map[string][]Diagnostic
	cliDiagnostics map[string][]Diagnostic
	opened         map[string]map[string]string // server -> path -> content hash
	surfaced       map[string]map[string]bool   // path -> location-independent diagnostic identities
	logs           []string
}

// Manager owns per-workspace LSP processes. A process is cached by cwd and
// server ID until Close or Reload, so a series of model calls does not spawn a
// new language server for every request.
type Manager struct {
	mu         sync.Mutex
	workspaces map[string]*workspace
	options    ManagerOptions
	closed     bool
}

var errManagerClosed = errors.New("LSP manager is closed")

func NewManager() *Manager {
	return NewManagerWithOptions(ManagerOptions{})
}

func NewManagerWithOptions(options ManagerOptions) *Manager {
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = 12 * time.Second
	}
	if options.DiagnosticDelay <= 0 {
		options.DiagnosticDelay = 300 * time.Millisecond
	}
	if options.LinterTimeout <= 0 {
		options.LinterTimeout = 30 * time.Second
	}
	return &Manager{workspaces: make(map[string]*workspace), options: options}
}

func (m *Manager) workspace(cwd string) (*workspace, error) {
	cwd, err := absDir(cwd)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errManagerClosed
	}
	if value := m.workspaces[cwd]; value != nil {
		m.mu.Unlock()
		return value, nil
	}
	m.mu.Unlock()
	config, err := LoadConfig(cwd)
	if err != nil {
		return nil, err
	}
	rootReal := cwd
	if resolved, resolveErr := filepath.EvalSymlinks(cwd); resolveErr == nil {
		rootReal = resolved
	}
	value := &workspace{cwd: cwd, rootReal: rootReal, config: config, clients: make(map[string]*Client), starting: make(map[string]chan struct{}), statuses: make(map[string]string), errors: make(map[string]string), diagnostics: make(map[string]map[string][]Diagnostic), cliDiagnostics: make(map[string][]Diagnostic), opened: make(map[string]map[string]string), surfaced: make(map[string]map[string]bool)}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errManagerClosed
	}
	if existing := m.workspaces[cwd]; existing != nil {
		value = existing
	} else {
		m.workspaces[cwd] = value
	}
	return value, nil
}

// Config returns the effective merged configuration for cwd.
func (m *Manager) Config(cwd string) (Config, error) {
	ws, err := m.workspace(cwd)
	if err != nil {
		return Config{}, err
	}
	return ws.config, nil
}

func (m *Manager) applicable(ws *workspace, path string) []ServerConfig {
	return ws.config.ApplicableServers(ws.cwd, path)
}

func (m *Manager) ensureClient(ctx context.Context, ws *workspace, spec ServerConfig) (*Client, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		m.mu.Lock()
		if m.closed || m.workspaces[ws.cwd] != ws {
			m.mu.Unlock()
			return nil, errManagerClosed
		}
		if client := ws.clients[spec.ID]; client != nil {
			m.mu.Unlock()
			return client, nil
		}
		if waiting := ws.starting[spec.ID]; waiting != nil {
			m.mu.Unlock()
			select {
			case <-waiting:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		starting := make(chan struct{})
		ws.starting[spec.ID] = starting
		ws.statuses[spec.ID] = "starting"
		m.mu.Unlock()
		root := FindRoot(ws.cwd, spec.RootMarkers)
		opts := ClientOptions{
			Settings:              wsSettings(m.options.Settings, spec.Settings),
			InitializationOptions: spec.InitializationOptions,
			OnDiagnostics:         func(diagnostics []Diagnostic) { m.storeDiagnostics(ws, spec.ID, diagnostics) },
			OnLog:                 func(message string) { m.log(ws, spec.ID, message) },
			// A server's LSP root may be an ancestor of the session cwd.
			// Keep server-initiated edits inside the session workspace rather
			// than trusting that broader project root.
			ApplyEdit: func(edit WorkspaceEdit) error {
				return applyWorkspaceEdit(ws.cwd, edit, m.options.CheckWritePath)
			},
		}
		startCtx, cancel := context.WithTimeout(ctx, m.options.RequestTimeout)
		client, err := NewClientWithContext(startCtx, spec, root, opts)
		cancel()
		m.mu.Lock()
		delete(ws.starting, spec.ID)
		close(starting)
		if err != nil {
			if m.closed || m.workspaces[ws.cwd] != ws {
				m.mu.Unlock()
				return nil, errManagerClosed
			}
			ws.statuses[spec.ID] = "missing"
			ws.errors[spec.ID] = err.Error()
			m.mu.Unlock()
			return nil, err
		}
		if m.closed || m.workspaces[ws.cwd] != ws {
			m.mu.Unlock()
			_ = client.Close()
			return nil, errManagerClosed
		}
		if existing := ws.clients[spec.ID]; existing != nil {
			m.mu.Unlock()
			_ = client.Close()
			return existing, nil
		}
		ws.clients[spec.ID] = client
		ws.statuses[spec.ID] = "ready"
		delete(ws.errors, spec.ID)
		m.mu.Unlock()
		return client, nil
	}
}

func wsSettings(global, local map[string]any) map[string]any {
	if len(global) == 0 && len(local) == 0 {
		return nil
	}
	out := make(map[string]any, len(global)+len(local))
	for key, value := range global {
		out[key] = value
	}
	for key, value := range local {
		out[key] = value
	}
	return out
}

func (m *Manager) storeDiagnostics(ws *workspace, server string, diagnostics []Diagnostic) {
	byPath := make(map[string][]Diagnostic)
	cleared := make(map[string]bool)
	for _, diagnostic := range diagnostics {
		if diagnostic.Path == "" || !diagnosticInWorkspace(ws.rootReal, diagnostic.Path) {
			continue
		}
		if diagnostic.Clear {
			cleared[diagnostic.Path] = true
			continue
		}
		byPath[diagnostic.Path] = append(byPath[diagnostic.Path], diagnostic)
	}
	m.mu.Lock()
	if ws.diagnostics[server] == nil {
		ws.diagnostics[server] = make(map[string][]Diagnostic)
	}
	for path := range cleared {
		delete(ws.diagnostics[server], path)
	}
	for path, values := range byPath {
		ws.diagnostics[server][path] = values
	}
	m.mu.Unlock()
}

func (m *Manager) log(ws *workspace, server, message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	if len(message) > 4096 {
		message = message[:4095] + "…"
	}
	m.mu.Lock()
	ws.logs = append(ws.logs, server+": "+message)
	if len(ws.logs) > 100 {
		ws.logs = ws.logs[len(ws.logs)-100:]
	}
	m.mu.Unlock()
	if m.options.OnLog != nil {
		m.options.OnLog(server, message)
	}
}

// Open opens a document in every applicable LSP server.
func (m *Manager) Open(ctx context.Context, cwd, path string) error {
	ws, err := m.workspace(cwd)
	if err != nil {
		return err
	}
	path, err = absoluteFile(ws.cwd, path)
	if err != nil {
		return err
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, spec := range m.applicable(ws, path) {
		if spec.Kind != "lsp" {
			continue
		}
		client, clientErr := m.ensureClient(ctx, ws, spec)
		if clientErr != nil {
			if err == nil {
				err = clientErr
			}
			continue
		}
		hash := contentHash(text)
		m.mu.Lock()
		if ws.opened[spec.ID] == nil {
			ws.opened[spec.ID] = make(map[string]string)
		}
		old, open := ws.opened[spec.ID][path]
		m.mu.Unlock()
		if !open {
			languageID := spec.LanguageID
			if languageID == "" {
				languageID = languageForExtension(filepath.Ext(path))
			}
			if clientErr = client.DidOpen(path, languageID, string(text)); clientErr == nil {
				m.mu.Lock()
				ws.opened[spec.ID][path] = hash
				m.mu.Unlock()
			}
		} else if old != hash {
			if clientErr = client.DidChange(path, string(text)); clientErr == nil {
				m.mu.Lock()
				ws.opened[spec.ID][path] = hash
				m.mu.Unlock()
			}
		}
		if clientErr != nil && err == nil {
			err = clientErr
		}
	}
	return err
}

func (m *Manager) Save(ctx context.Context, cwd, path string) error {
	openErr := m.Open(ctx, cwd, path)
	ws, err := m.workspace(cwd)
	if err != nil {
		return err
	}
	path, err = absoluteFile(ws.cwd, path)
	if err != nil {
		return err
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, spec := range m.applicable(ws, path) {
		if spec.Kind != "lsp" {
			continue
		}
		m.mu.Lock()
		client := ws.clients[spec.ID]
		m.mu.Unlock()
		if client != nil {
			_ = client.DidSave(path, string(text))
		}
	}
	return openErr
}

// Diagnostics obtains current LSP and CLI diagnostics. It deliberately waits
// only a bounded debounce interval for asynchronous publishDiagnostics.
func (m *Manager) Diagnostics(ctx context.Context, cwd, path string) ([]Diagnostic, error) {
	return m.DiagnosticsWithOptions(ctx, cwd, path, true)
}

func (m *Manager) DiagnosticsWithOptions(ctx context.Context, cwd, path string, runCLI bool) ([]Diagnostic, error) {
	ws, err := m.workspace(cwd)
	if err != nil {
		return nil, err
	}
	var firstErr error
	if path != "" {
		if saveErr := m.Save(ctx, ws.cwd, path); saveErr != nil && !errors.Is(saveErr, os.ErrNotExist) {
			// Keep cached diagnostics, but make an unavailable or failed
			// server visible to the model-facing lsp action.
			firstErr = saveErr
		}
	}
	for _, spec := range m.applicable(ws, path) {
		if spec.Kind != "lsp" {
			continue
		}
		m.mu.Lock()
		client := ws.clients[spec.ID]
		m.mu.Unlock()
		if client != nil {
			if waitErr := client.WaitForDiagnostics(ctx, m.options.DiagnosticDelay); waitErr != nil && firstErr == nil && !errors.Is(waitErr, context.Canceled) {
				firstErr = waitErr
			}
		}
	}
	if runCLI {
		if err := m.RunLinters(ctx, ws.cwd, path); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return m.snapshot(ws, path), firstErr
}

// ReduceDiagnostics returns only diagnostics that have not already been
// surfaced for this workspace path. The ledger deliberately ignores line
// and column changes, matching the behavior users want after a harmless edit:
// unchanged problems do not consume another tool-result context window.
// Passing an empty snapshot clears the path so a later recurrence is fresh.
func (m *Manager) ReduceDiagnostics(cwd, path string, diagnostics []Diagnostic) []Diagnostic {
	ws, err := m.workspace(cwd)
	if err != nil {
		return diagnostics
	}
	path, err = absoluteFile(ws.cwd, path)
	if err != nil {
		return diagnostics
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(diagnostics) == 0 {
		delete(ws.surfaced, path)
		return nil
	}
	previous := ws.surfaced[path]
	current := make(map[string]bool, len(diagnostics))
	fresh := make([]Diagnostic, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		identity := DiagnosticIdentity(diagnostic)
		current[identity] = true
		if !previous[identity] {
			fresh = append(fresh, diagnostic)
		}
	}
	ws.surfaced[path] = current
	return fresh
}

func (m *Manager) snapshot(ws *workspace, path string) []Diagnostic {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []Diagnostic
	for _, files := range ws.diagnostics {
		for file, diagnostics := range files {
			if path == "" || samePath(file, path) {
				all = append(all, diagnostics...)
			}
		}
	}
	for _, diagnostics := range ws.cliDiagnostics {
		for _, diagnostic := range diagnostics {
			if path == "" || samePath(diagnostic.Path, path) {
				all = append(all, diagnostic)
			}
		}
	}
	return DeduplicateDiagnostics(all)
}

// Request sends one request to a named LSP server.
func (m *Manager) Request(ctx context.Context, cwd, server, method string, params any) (json.RawMessage, error) {
	ws, err := m.workspace(cwd)
	if err != nil {
		return nil, err
	}
	spec, ok := ws.config.Servers[server]
	if !ok || spec.Disabled {
		return nil, fmt.Errorf("unknown or disabled LSP server %q", server)
	}
	if spec.Kind != "lsp" || spec.IsLinter {
		return nil, fmt.Errorf("%s is a linter and cannot serve navigation requests", server)
	}
	client, err := m.ensureClient(ctx, ws, spec)
	if err != nil {
		return nil, err
	}
	return m.requestWithTimeout(ctx, client, method, params)
}

func (m *Manager) requestWithTimeout(ctx context.Context, client *Client, method string, params any) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := context.WithTimeout(ctx, m.options.RequestTimeout)
	defer cancel()
	return client.Request(requestCtx, method, params)
}

// RequestAll sends a request to every applicable LSP server. If path is set,
// the document is opened first so servers have current text and URI context.
func (m *Manager) RequestAll(ctx context.Context, cwd, path, method string, params any) []Response {
	ws, err := m.workspace(cwd)
	if err != nil {
		return []Response{{Error: err}}
	}
	if path != "" {
		_ = m.Open(ctx, ws.cwd, path)
	}
	var responses []Response
	for _, spec := range m.applicable(ws, path) {
		if spec.Kind != "lsp" || spec.IsLinter {
			continue
		}
		client, clientErr := m.ensureClient(ctx, ws, spec)
		if clientErr != nil {
			responses = append(responses, Response{Server: spec.ID, Error: clientErr})
			continue
		}
		result, requestErr := m.requestWithTimeout(ctx, client, method, params)
		responses = append(responses, Response{Server: spec.ID, Result: result, Error: requestErr})
	}
	return responses
}

// ApplyEdit applies a workspace edit under the selected workspace root.
func (m *Manager) ApplyEdit(cwd string, edit WorkspaceEdit) error {
	return m.ApplyEdits(cwd, []WorkspaceEdit{edit})
}

// ApplyEdits preflights and applies multiple workspace edits as one operation.
// This prevents a later unsafe edit from leaving earlier edits applied.
func (m *Manager) ApplyEdits(cwd string, edits []WorkspaceEdit) error {
	ws, err := m.workspace(cwd)
	if err != nil {
		return err
	}
	merged := WorkspaceEdit{Changes: make(map[string][]TextEdit)}
	for _, edit := range edits {
		for uri, changes := range edit.Changes {
			merged.Changes[uri] = append(merged.Changes[uri], changes...)
		}
		merged.DocumentChanges = append(merged.DocumentChanges, edit.DocumentChanges...)
	}
	return applyWorkspaceEdit(ws.cwd, merged, m.options.CheckWritePath)
}

// Status describes configured and cached servers without starting missing
// processes. It is safe to call for a workspace with no installed tools.
type ServerStatus struct {
	ID           string          `json:"id"`
	Kind         string          `json:"kind"`
	Command      string          `json:"command"`
	Root         string          `json:"root,omitempty"`
	State        string          `json:"state"`
	Error        string          `json:"error,omitempty"`
	Diagnostics  int             `json:"diagnostics,omitempty"`
	Capabilities json.RawMessage `json:"capabilities,omitempty"`
}

func (m *Manager) Status(cwd, path string) ([]ServerStatus, error) {
	ws, err := m.workspace(cwd)
	if err != nil {
		return nil, err
	}
	specs := m.applicable(ws, path)
	roots := make(map[string]string, len(specs))
	resolveErrors := make(map[string]error, len(specs))
	for _, spec := range specs {
		roots[spec.ID] = displayWorkspacePath(ws.cwd, FindRoot(ws.cwd, spec.RootMarkers))
		if _, resolveErr := ResolveCommand(ws.cwd, spec.Command); resolveErr != nil {
			resolveErrors[spec.ID] = resolveErr
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ServerStatus, 0, len(specs))
	for _, spec := range specs {
		status := ServerStatus{ID: spec.ID, Kind: spec.Kind, Command: spec.Command, Root: roots[spec.ID], State: ws.statuses[spec.ID]}
		if status.State == "" {
			status.State = "stopped"
		}
		if status.State == "stopped" {
			if resolveErr := resolveErrors[spec.ID]; resolveErr != nil {
				status.State = "missing"
				status.Error = resolveErr.Error()
			}
		}
		if client := ws.clients[spec.ID]; client != nil {
			status.Capabilities = client.Capabilities()
		}
		for _, values := range ws.diagnostics[spec.ID] {
			status.Diagnostics += len(values)
		}
		status.Diagnostics += len(ws.cliDiagnostics[spec.ID])
		if configuredErr := ws.errors[spec.ID]; configuredErr != "" {
			status.Error = configuredErr
		}
		out = append(out, status)
	}
	return out, nil
}

func (m *Manager) Capabilities(ctx context.Context, cwd, path, server string) (map[string]json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ws, err := m.workspace(cwd)
	if err != nil {
		return nil, err
	}
	ensure := func(spec ServerConfig) (*Client, error) {
		if spec.Kind != "lsp" || spec.IsLinter {
			return nil, fmt.Errorf("%s is not a navigable LSP server", spec.ID)
		}
		m.mu.Lock()
		client := ws.clients[spec.ID]
		m.mu.Unlock()
		if client != nil {
			return client, nil
		}
		return m.ensureClient(ctx, ws, spec)
	}
	if server != "" {
		spec, ok := ws.config.Servers[server]
		if !ok || spec.Disabled {
			return nil, fmt.Errorf("unknown or disabled server %q", server)
		}
		client, err := ensure(spec)
		if err != nil {
			return nil, err
		}
		return map[string]json.RawMessage{server: client.Capabilities()}, nil
	}
	statuses, err := m.Status(cwd, path)
	if err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage)
	var firstErr error
	for _, status := range statuses {
		spec := ws.config.Servers[status.ID]
		if spec.Kind != "lsp" || spec.IsLinter {
			continue
		}
		client, clientErr := ensure(spec)
		if clientErr != nil {
			if firstErr == nil {
				firstErr = clientErr
			}
			continue
		}
		if capabilities := client.Capabilities(); len(capabilities) > 0 {
			out[status.ID] = capabilities
		}
	}
	return out, firstErr
}

// Reload terminates cached processes for cwd and reloads all lsp.json layers.
func (m *Manager) Reload(cwd string) error {
	cwd, err := absDir(cwd)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errManagerClosed
	}
	ws := m.workspaces[cwd]
	delete(m.workspaces, cwd)
	m.mu.Unlock()
	if ws != nil {
		for _, client := range m.clientsSnapshot(ws) {
			_ = client.Close()
		}
	}
	return nil
}

func (m *Manager) clientsSnapshot(ws *workspace) []*Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	clients := make([]*Client, 0, len(ws.clients))
	for _, client := range ws.clients {
		clients = append(clients, client)
	}
	return clients
}

func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	workspaces := make([]*workspace, 0, len(m.workspaces))
	for _, ws := range m.workspaces {
		workspaces = append(workspaces, ws)
	}
	m.workspaces = make(map[string]*workspace)
	m.mu.Unlock()
	var first error
	for _, ws := range workspaces {
		for _, client := range m.clientsSnapshot(ws) {
			if err := client.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

// RunLinters executes configured CLI providers without a shell. A non-zero
// linter exit is normal when it emitted parseable findings.
func (m *Manager) RunLinters(ctx context.Context, cwd, path string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ws, err := m.workspace(cwd)
	if err != nil {
		return err
	}
	var files []string
	if path != "" {
		if abs, pathErr := absoluteFile(ws.cwd, path); pathErr == nil {
			files = []string{abs}
		}
	}
	var firstErr error
	for _, spec := range m.applicable(ws, path) {
		if spec.Kind != "cli" || spec.Disabled {
			continue
		}
		command, resolveErr := ResolveCommand(ws.cwd, spec.Command)
		if resolveErr != nil {
			m.mu.Lock()
			ws.statuses[spec.ID] = "missing"
			ws.errors[spec.ID] = resolveErr.Error()
			m.mu.Unlock()
			if firstErr == nil {
				firstErr = resolveErr
			}
			continue
		}
		args := make([]string, 0, len(spec.Args)+len(files))
		containsFiles := false
		relative := relativeFiles(ws.cwd, files)
		for _, arg := range spec.Args {
			if arg == "{files}" {
				containsFiles = true
				args = append(args, relative...)
				continue
			}
			if strings.Contains(arg, "{files}") {
				containsFiles = true
				if len(relative) == 0 {
					args = append(args, strings.ReplaceAll(arg, "{files}", ""))
				} else {
					for _, file := range relative {
						args = append(args, strings.ReplaceAll(arg, "{files}", file))
					}
				}
				continue
			}
			args = append(args, arg)
		}
		if spec.Mode == "workspace" || (spec.Mode == "" && len(files) == 0) {
			if !containsFiles && (len(args) == 0 || args[len(args)-1] != ".") {
				args = append(args, ".")
			}
		} else if !containsFiles {
			args = append(args, relative...)
		}
		commandCtx, cancel := context.WithTimeout(ctx, m.options.LinterTimeout)
		cmd := exec.CommandContext(commandCtx, command, args...)
		cmd.Dir = ws.cwd
		cmd.Env = mergedEnvironment(spec.Env)
		var stdout, stderr limitedBuffer
		stdout.limit = 8 * 1024 * 1024
		stderr.limit = 8 * 1024 * 1024
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		m.mu.Lock()
		ws.statuses[spec.ID] = "running"
		m.mu.Unlock()
		runErr := cmd.Run()
		cancel()
		text := strings.TrimSpace(stdout.String())
		if text == "" {
			text = strings.TrimSpace(stderr.String())
		}
		diagnostics := ParseDiagnostics(spec.Parser, spec.ID, ws.cwd, text)
		filtered := diagnostics[:0]
		for _, diagnostic := range diagnostics {
			if diagnostic.Path != "" && diagnosticInWorkspace(ws.rootReal, diagnostic.Path) {
				filtered = append(filtered, diagnostic)
			}
		}
		diagnostics = filtered
		m.mu.Lock()
		ws.cliDiagnostics[spec.ID] = diagnostics
		if runErr != nil && len(diagnostics) == 0 {
			ws.errors[spec.ID] = runErr.Error()
			ws.statuses[spec.ID] = "error"
		} else {
			delete(ws.errors, spec.ID)
			ws.statuses[spec.ID] = "ready"
		}
		m.mu.Unlock()
		if runErr != nil && len(diagnostics) == 0 && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", spec.ID, runErr)
		}
	}
	return firstErr
}

func contentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func displayWorkspacePath(root, path string) string {
	if path == "" {
		return ""
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return ""
	}
	if rel == "." {
		return "."
	}
	return filepath.ToSlash(rel)
}

func relativeFiles(cwd string, files []string) []string {
	out := make([]string, len(files))
	for i, path := range files {
		rel, err := filepath.Rel(cwd, path)
		if err != nil {
			out[i] = path
		} else {
			out[i] = filepath.ToSlash(rel)
		}
	}
	return out
}
func absoluteFile(cwd, path string) (string, error) {
	if path == "" {
		return "", errors.New("path is required")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	return filepath.Abs(path)
}
func diagnosticInWorkspace(rootReal, path string) bool {
	return safeWorkspacePath(rootReal, path) == nil
}

func samePath(a, b string) bool {
	aa, _ := filepath.Abs(a)
	bb, _ := filepath.Abs(b)
	return filepath.Clean(aa) == filepath.Clean(bb)
}

type limitedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *limitedBuffer) Len() int { return b.buffer.Len() }

func (b *limitedBuffer) String() string { return b.buffer.String() }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	if b.limit <= b.buffer.Len() {
		return original, nil
	}
	room := b.limit - b.buffer.Len()
	if len(p) > room {
		p = p[:room]
	}
	_, _ = b.buffer.Write(p)
	return original, nil
}
