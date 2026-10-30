package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/mcp"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/stretchr/testify/require"
)

// gatedMCPServer wraps fakeMCPHTTPServer's handler and holds every request
// until release is closed, simulating a slow-starting server. arrived is
// closed when the first request (initialize) reaches the gate.
func gatedMCPServer(t *testing.T) (*httptest.Server, chan struct{}, <-chan struct{}) {
	t.Helper()
	inner := fakeMCPHTTPServer(t)
	release := make(chan struct{})
	arrived := make(chan struct{})
	var arrivedOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrivedOnce.Do(func() { close(arrived) })
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, inner.URL, r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := inner.Client().Do(req)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		buf := make([]byte, 32<<10)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	return srv, release, arrived
}

func writeMCPConfig(t *testing.T, home string, servers map[string]mcp.ServerConfig) {
	t.Helper()
	require.NoError(t, mcp.WriteConfigFile(mcp.GlobalConfigPath(home), mcp.Config{Servers: servers}))
}

func hasToolPrefix(names []string, prefix string) bool {
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

// liveToolNames reads the assembly's live agent registry.
func liveToolNames(t *testing.T, deps session.Deps) []string {
	t.Helper()
	ref, ok := deps.ToolRefresher.(sessionToolRefresher)
	require.True(t, ok, "deps not built by NewSessionDeps")
	ag := ref.a.liveAgent()
	if ag == nil {
		return nil
	}
	var out []string
	for name := range ag.ToolsSnapshot() {
		out = append(out, name)
	}
	return out
}

func mcpTestSpec(t *testing.T, providerURL string) session.CreateSpec {
	return session.CreateSpec{
		CWD:  t.TempDir(),
		Env:  []string{"PATH=" + os.Getenv("PATH")},
		Args: []string{"--provider", "openai", "--model", "gpt-5", "--base-url", providerURL, "--no-ext", "--api-key", "test-key"},
	}
}

type depsResult struct {
	deps    session.Deps
	cleanup func()
	err     error
}

func createDepsAsync(t *testing.T, spec session.CreateSpec) chan depsResult {
	done := make(chan depsResult, 1)
	go func() {
		deps, cleanup, err := NewSessionDeps(context.Background(), spec, "test")
		done <- depsResult{deps, cleanup, err}
	}()
	return done
}

func TestSessionCreateDoesNotWaitForOptionalMCP(t *testing.T) {
	home := sessionAssemblyEnv(t)
	provider := sessionAssemblyProvider(t)
	slow, release, _ := gatedMCPServer(t)
	writeMCPConfig(t, home, map[string]mcp.ServerConfig{"slow": {Type: "http", URL: slow.URL, StartupTimeoutSec: 30}})

	var r depsResult
	select {
	case r = <-createDepsAsync(t, mcpTestSpec(t, provider.URL)):
		require.NoError(t, r.err)
	case <-time.After(15 * time.Second):
		t.Fatal("session creation waited for the optional MCP server")
	}
	t.Cleanup(r.cleanup)
	ctx, cancel := context.WithCancel(context.Background())
	ctrl := session.NewController(ctx, r.deps)
	t.Cleanup(func() { cancel(); <-ctrl.Done() })
	confirmer, asker := ctrl.HookBindings()
	require.NoError(t, BindSessionHooks(r.deps, ctrl.ExtensionHooks(), confirmer, asker))
	require.NoError(t, BindToolRefresh(r.deps, ctrl.RequestToolRefresh))
	require.False(t, hasToolPrefix(liveToolNames(t, r.deps), "mcp__slow__"), "tools present before the server was ready")

	close(release)
	require.Eventually(t, func() bool { return hasToolPrefix(liveToolNames(t, r.deps), "mcp__slow__") }, 10*time.Second, 10*time.Millisecond,
		"background MCP tools never reached the session agent")
}

func TestSessionCreateFailsOnRequiredMCP(t *testing.T) {
	home := sessionAssemblyEnv(t)
	provider := sessionAssemblyProvider(t)
	writeMCPConfig(t, home, map[string]mcp.ServerConfig{"must": {Command: "/nonexistent/zut-required-mcp", Required: true}})
	_, _, err := NewSessionDeps(context.Background(), mcpTestSpec(t, provider.URL), "test")
	require.ErrorContains(t, err, "required MCP server failed to start")
}

func TestSessionCreateWaitsForRequiredMCP(t *testing.T) {
	home := sessionAssemblyEnv(t)
	provider := sessionAssemblyProvider(t)
	slow, release, arrived := gatedMCPServer(t)
	writeMCPConfig(t, home, map[string]mcp.ServerConfig{"slow": {Type: "http", URL: slow.URL, Required: true, StartupTimeoutSec: 30}})
	done := createDepsAsync(t, mcpTestSpec(t, provider.URL))
	// The required server's initialize is held at the gate: creation must
	// still be in progress.
	select {
	case <-arrived:
	case r := <-done:
		if r.cleanup != nil {
			r.cleanup()
		}
		t.Fatalf("creation returned before contacting the required server: %v", r.err)
	case <-time.After(15 * time.Second):
		t.Fatal("required server never contacted")
	}
	select {
	case r := <-done:
		if r.cleanup != nil {
			r.cleanup()
		}
		t.Fatalf("creation returned while the required server was held: %v", r.err)
	default:
	}
	close(release)
	select {
	case r := <-done:
		require.NoError(t, r.err)
		t.Cleanup(r.cleanup)
		require.True(t, hasToolPrefix(liveToolNames(t, r.deps), "mcp__slow__"), "required server tools missing at creation")
	case <-time.After(15 * time.Second):
		t.Fatal("creation never finished after the required server became ready")
	}
}
