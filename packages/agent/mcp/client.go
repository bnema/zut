package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// LatestProtocolVersion is the MCP revision zut requests during initialize.
const LatestProtocolVersion = "2025-06-18"

// supportedProtocolVersions lists revisions zut accepts from a server.
var supportedProtocolVersions = map[string]bool{
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

const (
	// maxListPages bounds pagination of list requests.
	maxListPages = 100
	// maxListItems bounds the number of tools or resources kept per server.
	maxListItems = 2048
)

// Tool is one MCP tool definition.
type Tool struct {
	Name        string           `json:"name"`
	Title       string           `json:"title,omitempty"`
	Description string           `json:"description,omitempty"`
	InputSchema json.RawMessage  `json:"inputSchema,omitempty"`
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}

// ToolAnnotations are untrusted hints supplied by the server.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// Resource is one entry from resources/list.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// ResourceTemplate is one entry from resources/templates/list.
type ResourceTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// Content is one MCP content block from a tool result or resource read.
type Content struct {
	Type     string           `json:"type"`
	Text     string           `json:"text,omitempty"`
	Data     string           `json:"data,omitempty"`
	MimeType string           `json:"mimeType,omitempty"`
	URI      string           `json:"uri,omitempty"`
	Name     string           `json:"name,omitempty"`
	Resource *ResourceContent `json:"resource,omitempty"`
}

// ResourceContent is the payload of an embedded resource or resources/read.
type ResourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// CallResult is the tools/call result.
type CallResult struct {
	Content           []Content       `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// ServerInfo is what the server reported during initialize.
type ServerInfo struct {
	Name            string
	Version         string
	ProtocolVersion string
	Instructions    string
	HasTools        bool
	HasResources    bool
	ToolsChanged    bool
}

// Client is one initialized MCP session.
type Client struct {
	name string
	t    transport
	cwd  string
	next atomic.Int64
	info ServerInfo

	// onToolsChanged is invoked on notifications/tools/list_changed.
	mu             sync.Mutex
	onToolsChanged func()
}

// ClientInfo identifies zut to servers.
type ClientInfo struct {
	Name    string
	Version string
}

// Connect starts the transport for cfg and performs the initialize handshake.
func Connect(ctx context.Context, name string, cfg ServerConfig, cwd string, info ClientInfo, httpClient *http.Client) (*Client, error) {
	var (
		t   transport
		err error
	)
	switch cfg.Kind() {
	case "stdio":
		var st *stdioTransport
		st, err = startStdio(name, cfg, cwd)
		if err == nil {
			t = st
		}
	case "http":
		var ht *httpTransport
		ht, err = newHTTPTransport(name, cfg, httpClient)
		if err == nil {
			t = ht
		}
	default:
		err = fmt.Errorf("unsupported transport %q", cfg.Kind())
	}
	if err != nil {
		return nil, err
	}
	c := &Client{name: name, t: t, cwd: cwd}
	t.setHandler(c.handleIncoming)
	if err := c.initialize(ctx, info); err != nil {
		tail := ""
		if st, ok := t.(*stdioTransport); ok {
			tail = st.stderrTail()
		}
		_ = t.close()
		if tail != "" {
			return nil, fmt.Errorf("%w\nserver stderr:\n%s", err, tail)
		}
		return nil, err
	}
	if ht, ok := t.(*httpTransport); ok && c.info.ToolsChanged {
		ht.listen()
	}
	return c, nil
}

func (c *Client) initialize(ctx context.Context, info ClientInfo) error {
	if info.Name == "" {
		info.Name = "zut"
	}
	params := map[string]any{
		"protocolVersion": LatestProtocolVersion,
		"capabilities": map[string]any{
			"roots": map[string]any{"listChanged": false},
		},
		"clientInfo": map[string]any{"name": info.Name, "title": "zut", "version": info.Version},
	}
	raw, err := c.call(ctx, "initialize", params)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools *struct {
				ListChanged bool `json:"listChanged"`
			} `json:"tools"`
			Resources *json.RawMessage `json:"resources"`
		} `json:"capabilities"`
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("initialize: decode result: %w", err)
	}
	if !supportedProtocolVersions[result.ProtocolVersion] {
		return fmt.Errorf("initialize: unsupported protocol version %q", result.ProtocolVersion)
	}
	c.info = ServerInfo{
		Name:            result.ServerInfo.Name,
		Version:         result.ServerInfo.Version,
		ProtocolVersion: result.ProtocolVersion,
		Instructions:    result.Instructions,
		HasTools:        result.Capabilities.Tools != nil,
		HasResources:    result.Capabilities.Resources != nil,
		ToolsChanged:    result.Capabilities.Tools != nil && result.Capabilities.Tools.ListChanged,
	}
	c.t.setProtocolVersion(result.ProtocolVersion)
	if err := c.t.notify(ctx, "notifications/initialized", nil); err != nil {
		return fmt.Errorf("initialized notification: %w", err)
	}
	return nil
}

// Info returns the negotiated server information.
func (c *Client) Info() ServerInfo { return c.info }

// Done is closed when the connection becomes unusable.
func (c *Client) Done() <-chan struct{} { return c.t.done() }

// Close ends the session and stops a stdio server process.
func (c *Client) Close() error { return c.t.close() }

// SetToolsChanged installs the tools/list_changed callback.
func (c *Client) SetToolsChanged(fn func()) {
	c.mu.Lock()
	c.onToolsChanged = fn
	c.mu.Unlock()
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return c.t.request(ctx, c.next.Add(1), method, params)
}

func (c *Client) handleIncoming(msg message) (any, *RPCError) {
	switch msg.Method {
	case "ping":
		return struct{}{}, nil
	case "roots/list":
		roots := []map[string]string{}
		if c.cwd != "" {
			if abs, err := filepath.Abs(c.cwd); err == nil {
				u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
				roots = append(roots, map[string]string{"uri": u.String(), "name": filepath.Base(abs)})
			}
		}
		return map[string]any{"roots": roots}, nil
	case "notifications/tools/list_changed":
		c.mu.Lock()
		fn := c.onToolsChanged
		c.mu.Unlock()
		if fn != nil {
			fn()
		}
		return nil, nil
	}
	if len(msg.ID) == 0 {
		return nil, nil // other notifications (logging, progress) are ignored
	}
	return nil, &RPCError{Code: codeMethodNotFound, Message: "method not supported by zut: " + msg.Method}
}

// ListTools returns every tool, following pagination cursors.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	if !c.info.HasTools {
		return nil, nil
	}
	var out []Tool
	err := paginate(ctx, c, "tools/list", func(raw json.RawMessage) (string, error) {
		var page struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		out = append(out, page.Tools...)
		if len(out) > maxListItems {
			out = out[:maxListItems]
			return "", nil
		}
		return page.NextCursor, nil
	})
	return out, err
}

// ListResources returns every resource, following pagination cursors.
func (c *Client) ListResources(ctx context.Context) ([]Resource, error) {
	if !c.info.HasResources {
		return nil, nil
	}
	var out []Resource
	err := paginate(ctx, c, "resources/list", func(raw json.RawMessage) (string, error) {
		var page struct {
			Resources  []Resource `json:"resources"`
			NextCursor string     `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		out = append(out, page.Resources...)
		if len(out) > maxListItems {
			out = out[:maxListItems]
			return "", nil
		}
		return page.NextCursor, nil
	})
	return out, err
}

// ListResourceTemplates returns every resource template.
func (c *Client) ListResourceTemplates(ctx context.Context) ([]ResourceTemplate, error) {
	if !c.info.HasResources {
		return nil, nil
	}
	var out []ResourceTemplate
	err := paginate(ctx, c, "resources/templates/list", func(raw json.RawMessage) (string, error) {
		var page struct {
			ResourceTemplates []ResourceTemplate `json:"resourceTemplates"`
			NextCursor        string             `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		out = append(out, page.ResourceTemplates...)
		if len(out) > maxListItems {
			out = out[:maxListItems]
			return "", nil
		}
		return page.NextCursor, nil
	})
	return out, err
}

// ReadResource reads one resource URI.
func (c *Client) ReadResource(ctx context.Context, uri string) ([]ResourceContent, error) {
	raw, err := c.call(ctx, "resources/read", map[string]any{"uri": uri})
	if err != nil {
		return nil, err
	}
	var result struct {
		Contents []ResourceContent `json:"contents"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode resources/read: %w", err)
	}
	return result.Contents, nil
}

// CallTool invokes one tool by its raw MCP name.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (CallResult, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(args, &probe); err != nil || probe == nil {
		return CallResult{}, errors.New("tool arguments must be a JSON object")
	}
	raw, err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return CallResult{}, err
	}
	var result CallResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return CallResult{}, fmt.Errorf("decode tools/call: %w", err)
	}
	return result, nil
}

func paginate(ctx context.Context, c *Client, method string, page func(json.RawMessage) (string, error)) error {
	cursor := ""
	seen := map[string]bool{}
	for i := 0; i < maxListPages; i++ {
		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		raw, err := c.call(ctx, method, params)
		if err != nil {
			return fmt.Errorf("%s: %w", method, err)
		}
		next, err := page(raw)
		if err != nil {
			return fmt.Errorf("%s: decode: %w", method, err)
		}
		if next == "" {
			return nil
		}
		if seen[next] {
			return fmt.Errorf("%s: server repeated pagination cursor", method)
		}
		seen[next] = true
		cursor = next
	}
	return fmt.Errorf("%s: more than %d pages", method, maxListPages)
}

// withTimeout returns ctx bounded by d when d > 0.
func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}
