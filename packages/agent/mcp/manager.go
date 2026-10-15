package mcp

import (
	"context"
	"errors"
	"fmt"
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
}

// Manager owns the MCP connections of one zut process. It is safe for
// concurrent use; resident subagents share the primary's connections.
type Manager struct {
	cwd        string
	info       ClientInfo
	httpClient *http.Client

	servers map[string]*serverEntry
	order   []string

	mu     sync.Mutex
	closed bool
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
	s.mu.Unlock()
	if stale != nil {
		_ = stale.Close()
	}
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("mcp server %q: manager closed", s.name)
	}
	startCtx, cancel := withTimeout(ctx, s.cfg.StartupTimeout())
	defer cancel()
	client, err := Connect(startCtx, s.name, s.cfg, m.cwd, m.info, m.httpClient)
	var tools []Tool
	if err == nil {
		tools, err = client.ListTools(startCtx)
		if err != nil {
			_ = client.Close()
		}
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			err = fmt.Errorf("timed out after %s; raise startupTimeoutSec if the server is slow to start", s.cfg.StartupTimeout())
		}
		err = fmt.Errorf("mcp server %q: %w", s.name, err)
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		return nil, err
	}
	client.SetToolsChanged(func() { m.refreshTools(s, client) })
	// Publish under m.mu so Close either sees this client or we see closed.
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		// Close ran during the handshake; do not leak the new process.
		_ = client.Close()
		return nil, fmt.Errorf("mcp server %q: manager closed", s.name)
	}
	s.mu.Lock()
	s.client = client
	s.tools = filterTools(s.cfg, tools)
	s.err = nil
	s.mu.Unlock()
	m.mu.Unlock()
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
// definitions reach the model the next time zut builds a tool registry.
func (m *Manager) refreshTools(s *serverEntry, client *Client) {
	ctx, cancel := withTimeout(context.Background(), s.cfg.StartupTimeout())
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
			s.mu.Unlock()
			if client != nil {
				_ = client.Close()
			}
		}()
	}
	wg.Wait()
	return nil
}

// FormatStatus renders a short human-readable status table.
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
		fmt.Fprintf(&b, "%s (%s): %s, %d tools", st.Name, st.Transport, state, st.Tools)
		if st.Resources {
			b.WriteString(", resources")
		}
		if st.Err != "" {
			b.WriteString("\n  error: " + firstLine(st.Err))
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
