package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// ServerStatus is a snapshot of one configured server.
type ServerStatus struct {
	Name      string
	Transport string
	Connected bool
	Tools     int
	Resources bool
	Err       string
	Info      ServerInfo
}

type serverEntry struct {
	name string
	cfg  ServerConfig

	// connectMu serializes connection attempts; mu guards the fields below
	// and is never held across network or process I/O, so Status stays fast.
	connectMu sync.Mutex
	mu        sync.Mutex
	client    *Client
	tools     []Tool
	err       error
	refresher *toolRefresher
}

// Manager owns the MCP connections of one zut process. It is safe for
// concurrent use; resident subagents share the primary's connections.
type Manager struct {
	cwd        string
	info       ClientInfo
	httpClient *http.Client

	servers map[string]*serverEntry
	order   []string

	mu             sync.Mutex
	closed         bool
	stderrWriter   io.Writer
	stderrWriterMu *sync.Mutex
}

// NewManager builds a manager for an already loaded configuration.
func NewManager(cfg Config, cwd string, info ClientInfo, httpClient *http.Client) *Manager {
	m := &Manager{cwd: cwd, info: info, httpClient: httpClient, servers: map[string]*serverEntry{}}
	for _, name := range sortedNames(cfg.Servers) {
		m.servers[name] = &serverEntry{name: name, cfg: cfg.Servers[name]}
		m.order = append(m.order, name)
	}
	return m
}

// Empty reports whether no servers are configured.
func (m *Manager) Empty() bool { return m == nil || len(m.order) == 0 }

// Start connects every server concurrently and lists its tools. Each server
// is bounded by its own startup timeout. Failures are returned per server and
// leave the server disconnected; a later tool call retries the connection.
// The returned error is non-nil only when a server marked required failed.
func (m *Manager) Start(ctx context.Context) (failures []error, requiredErr error) {
	if m.Empty() {
		return nil, nil
	}
	var wg sync.WaitGroup
	errs := make([]error, len(m.order))
	for i, name := range m.order {
		wg.Add(1)
		go func(i int, s *serverEntry) {
			defer wg.Done()
			if _, err := m.connect(ctx, s); err != nil {
				errs[i] = err
			}
		}(i, m.servers[name])
	}
	wg.Wait()
	var required []error
	for i, err := range errs {
		if err == nil {
			continue
		}
		failures = append(failures, err)
		if m.servers[m.order[i]].cfg.Required {
			required = append(required, err)
		}
	}
	return failures, errors.Join(required...)
}

// connect returns a live client for s, connecting (or reconnecting after a
// dropped connection) when needed.
func (m *Manager) connect(ctx context.Context, s *serverEntry) (*Client, error) {
	if c := s.live(); c != nil {
		return c, nil
	}
	s.connectMu.Lock()
	defer s.connectMu.Unlock()
	if c := s.live(); c != nil {
		return c, nil // another caller reconnected while we waited
	}
	s.mu.Lock()
	stale := s.client
	s.client = nil
	staleRefresher := s.refresher
	s.refresher = nil
	s.mu.Unlock()
	if staleRefresher != nil {
		staleRefresher.stop()
	}
	if stale != nil {
		_ = stale.Close()
	}
	m.mu.Lock()
	closed := m.closed
	var sink *stderrSink
	if m.stderrWriter != nil {
		sink = &stderrSink{mu: m.stderrWriterMu, w: m.stderrWriter, prefix: "mcp[" + sanitizeText(s.name, false) + "]: "}
	}
	m.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("mcp server %q: manager closed", s.name)
	}
	startCtx, cancel := withTimeout(ctx, s.cfg.StartupTimeout())
	defer cancel()
	// The refresher exists before the handshake so a list_changed notification
	// that arrives while the first listing is in flight is remembered, and
	// the listing is repeated once the client is published.
	refresher := newToolRefresher()
	client, err := connectWith(startCtx, s.name, s.cfg, m.cwd, m.info, m.httpClient, refresher.notify, sink)
	var tools []Tool
	if err == nil {
		tools, err = client.ListTools(startCtx)
		if err != nil {
			_ = client.Close()
		}
	}
	if err != nil {
		refresher.stop()
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			err = fmt.Errorf("timed out after %s; raise startupTimeoutSec if the server is slow to start", s.cfg.StartupTimeout())
		}
		err = fmt.Errorf("mcp server %q: %w", s.name, err)
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		return nil, err
	}
	// Publish under m.mu so Close either sees this client or we see closed.
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		// Close ran during the handshake; do not leak the new process.
		refresher.stop()
		_ = client.Close()
		return nil, fmt.Errorf("mcp server %q: manager closed", s.name)
	}
	s.mu.Lock()
	s.client = client
	s.tools = filterTools(s.cfg, tools)
	s.err = nil
	old := s.refresher
	s.refresher = refresher
	s.mu.Unlock()
	m.mu.Unlock()
	if old != nil {
		old.stop()
	}
	go func() {
		<-client.Done()
		refresher.stop()
	}()
	refresher.start(func(ctx context.Context) { m.refreshTools(ctx, s, client) })
	return client, nil
}

// live returns the current client when its connection is still usable.
func (s *serverEntry) live() *Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return nil
	}
	select {
	case <-s.client.Done():
		return nil
	default:
		return s.client
	}
}

// refreshTools re-lists tools after notifications/tools/list_changed. New
// definitions reach the model the next time zut builds a tool registry. It is
// only called from the server's refresher, which serializes and coalesces
// calls.
func (m *Manager) refreshTools(ctx context.Context, s *serverEntry, client *Client) {
	ctx, cancel := withTimeout(ctx, s.cfg.StartupTimeout())
	defer cancel()
	tools, err := client.ListTools(ctx)
	if err != nil {
		return
	}
	s.mu.Lock()
	if s.client == client {
		s.tools = filterTools(s.cfg, tools)
	}
	s.mu.Unlock()
}

// toolRefresher coalesces list_changed notifications for one connection into
// serialized re-listings. Any number of notifications that arrive while a
// listing runs cause exactly one more listing afterwards. It does nothing
// until start installs the listing function, so notifications that arrive
// during the initial handshake are remembered rather than raced.
type toolRefresher struct {
	mu      sync.Mutex
	dirty   bool
	running bool
	stopped bool
	list    func(ctx context.Context)
	ctx     context.Context
	cancel  context.CancelFunc
}

func newToolRefresher() *toolRefresher {
	ctx, cancel := context.WithCancel(context.Background())
	return &toolRefresher{ctx: ctx, cancel: cancel}
}

// notify records that the server's tool list changed.
func (r *toolRefresher) notify() {
	r.mu.Lock()
	r.dirty = true
	r.spawnLocked()
	r.mu.Unlock()
}

// start enables listing with fn and runs it once if a notification arrived
// earlier.
func (r *toolRefresher) start(fn func(ctx context.Context)) {
	r.mu.Lock()
	r.list = fn
	r.spawnLocked()
	r.mu.Unlock()
}

// stop cancels any running listing and ignores later notifications.
func (r *toolRefresher) stop() {
	r.mu.Lock()
	r.stopped = true
	r.dirty = false
	r.mu.Unlock()
	r.cancel()
}

func (r *toolRefresher) spawnLocked() {
	if r.stopped || r.running || !r.dirty || r.list == nil {
		return
	}
	r.running = true
	go r.run()
}

func (r *toolRefresher) run() {
	for {
		r.mu.Lock()
		if r.stopped || !r.dirty {
			r.running = false
			r.mu.Unlock()
			return
		}
		r.dirty = false
		list := r.list
		r.mu.Unlock()
		list(r.ctx)
	}
}

func filterTools(cfg ServerConfig, tools []Tool) []Tool {
	out := make([]Tool, 0, len(tools))
	seen := map[string]bool{}
	for _, tool := range tools {
		if tool.Name == "" || seen[tool.Name] || !cfg.ToolAllowed(tool.Name) {
			continue
		}
		seen[tool.Name] = true
		out = append(out, tool)
	}
	return out
}

// client returns a live client for server, reconnecting when necessary.
func (m *Manager) client(ctx context.Context, server string) (*Client, ServerConfig, error) {
	if m == nil {
		return nil, ServerConfig{}, errors.New("mcp is not enabled")
	}
	s, ok := m.servers[server]
	if !ok {
		return nil, ServerConfig{}, fmt.Errorf("unknown mcp server %q", server)
	}
	c, err := m.connect(ctx, s)
	return c, s.cfg, err
}

// Servers returns configured server names in stable order.
func (m *Manager) Servers() []string {
	if m == nil {
		return nil
	}
	return append([]string(nil), m.order...)
}

// Status returns a snapshot of every server.
func (m *Manager) Status() []ServerStatus {
	if m == nil {
		return nil
	}
	out := make([]ServerStatus, 0, len(m.order))
	for _, name := range m.order {
		s := m.servers[name]
		s.mu.Lock()
		st := ServerStatus{Name: name, Transport: s.cfg.Kind(), Tools: len(s.tools)}
		if s.client != nil {
			select {
			case <-s.client.Done():
			default:
				st.Connected = true
			}
			st.Info = s.client.Info()
			st.Resources = st.Info.HasResources
		}
		if s.err != nil {
			st.Err = s.err.Error()
		}
		s.mu.Unlock()
		out = append(out, st)
	}
	return out
}

// ServerTools returns the known tool definitions of one server.
func (m *Manager) ServerTools(server string) []Tool {
	if m == nil {
		return nil
	}
	s, ok := m.servers[server]
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Tool(nil), s.tools...)
}

// hasResources reports whether any connected server advertises resources.
func (m *Manager) hasResources() bool {
	for _, st := range m.Status() {
		if st.Resources {
			return true
		}
	}
	return false
}

// Close stops every connection. It is idempotent.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, name := range m.order {
		s := m.servers[name]
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.mu.Lock()
			client := s.client
			s.client = nil
			refresher := s.refresher
			s.refresher = nil
			s.mu.Unlock()
			if refresher != nil {
				refresher.stop()
			}
			if client != nil {
				_ = client.Close()
			}
		}()
	}
	wg.Wait()
	return nil
}

// FormatStatus renders a short human-readable status table. Server-derived
// text (names, errors) is stripped of terminal control sequences.
func FormatStatus(statuses []ServerStatus) string {
	if len(statuses) == 0 {
		return "no MCP servers configured"
	}
	sort.SliceStable(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	var b strings.Builder
	for _, st := range statuses {
		state := "disconnected"
		if st.Connected {
			state = "connected"
		}
		fmt.Fprintf(&b, "%s (%s): %s, %d tools", sanitizeText(st.Name, false), sanitizeText(st.Transport, false), state, st.Tools)
		if st.Resources {
			b.WriteString(", resources")
		}
		if st.Err != "" {
			b.WriteString("\n  error: " + sanitizeText(firstLine(st.Err), false))
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// SetStderrWriter enables live stdio server diagnostics. Call before Start.
func (m *Manager) SetStderrWriter(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stderrWriter = w
	if m.stderrWriterMu == nil {
		m.stderrWriterMu = &sync.Mutex{}
	}
}
