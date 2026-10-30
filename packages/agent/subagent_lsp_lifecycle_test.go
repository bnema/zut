package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/lsp"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

// TestResidentFakeLSPProcess is the fake language server used by the
// lifecycle tests below. It dials the test's liveness listener so the test
// observes process exit as EOF on that connection, then serves minimal LSP.
func TestResidentFakeLSPProcess(t *testing.T) {
	address := os.Getenv("ZUT_RESIDENT_FAKE_LSP_ADDR")
	if address == "" {
		return
	}
	liveness, err := net.Dial("tcp", address)
	if err != nil {
		os.Exit(2)
	}
	defer liveness.Close()
	reader := bufio.NewReader(os.Stdin)
	for {
		payload, err := lsp.ReadMessage(reader)
		if err != nil {
			return
		}
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(payload, &message) != nil || len(message.ID) == 0 {
			continue
		}
		result := `null`
		if message.Method == "initialize" {
			result = `{"capabilities":{}}`
		}
		_ = lsp.WriteMessage(os.Stdout, []byte(`{"jsonrpc":"2.0","id":`+string(message.ID)+`,"result":`+result+`}`))
	}
}

// fakeLSPWorkspace isolates zut config, registers the fake server for cwd,
// and returns the listener each started server process connects to.
func fakeLSPWorkspace(t *testing.T, cwd string) net.Listener {
	t.Helper()
	t.Setenv("ZUT_HOME", t.TempDir())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	config := `{"autoDetect":false,"servers":{"fake":{"kind":"lsp","command":` + strconv.Quote(os.Args[0]) +
		`,"args":["-test.run=^TestResidentFakeLSPProcess$"],"env":{"ZUT_RESIDENT_FAKE_LSP_ADDR":` + strconv.Quote(listener.Addr().String()) + `}}}}`
	if err := os.WriteFile(filepath.Join(cwd, "lsp.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	return listener
}

// startFakeLSP starts the fake server through the registry's LSP manager and
// returns the liveness connection of the started process.
func startFakeLSP(t *testing.T, registry core.Registry, cwd string, listener net.Listener) net.Conn {
	t.Helper()
	lspTool, ok := registry["lsp"].(*tools.LSPTool)
	if !ok || lspTool.Manager == nil {
		t.Fatalf("resolved registry has no LSP manager: %#v", registry["lsp"])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := lspTool.Manager.Capabilities(ctx, cwd, "", "fake"); err != nil {
		t.Fatalf("start fake LSP server: %v", err)
	}
	if tcp, ok := listener.(*net.TCPListener); ok {
		_ = tcp.SetDeadline(time.Now().Add(10 * time.Second))
	}
	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("fake LSP server never connected: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// waitFakeLSPExit waits, with a deadline, for the server process to exit.
func waitFakeLSPExit(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err := io.Copy(io.Discard, conn)
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatal("LSP server process is still running after its owner was closed")
	}
}

func TestResidentChildStopTerminatesItsLSPServer(t *testing.T) {
	cwd := t.TempDir()
	listener := fakeLSPWorkspace(t, cwd)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		writeOpenAIChunk(t, w, map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": "done"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 1},
		})
	}))
	defer server.Close()

	started := make(chan net.Conn, 1)
	manager := subagents.NewResidentManager(t.TempDir(), func(spec subagents.ResidentChildSpec, journal *subagents.ResidentJournal) (subagents.ResidentRuntime, error) {
		runner, err := newResidentChildRunner(Args{Provider: "openai", Model: "gpt-4o", APIKey: "synthetic", BaseURL: server.URL, CWD: cwd, NoContextFiles: true, NoSkill: true}, spec, journal)
		if err != nil {
			return nil, err
		}
		resident, ok := runner.(*residentAgentRuntime)
		if !ok {
			t.Fatalf("runner type = %T", runner)
		}
		started <- startFakeLSP(t, resident.tools, cwd, listener)
		return runner, nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	completed := make(chan subagents.ResidentCompletion, 1)
	manager.SetCompletionObserver(func(c subagents.ResidentCompletion) bool { completed <- c; return true })
	spec := subagents.ResidentChildSpec{ID: "lsp-child", SessionID: "lsp-session", Provider: "openai", Model: "gpt-4o", Tools: []string{"read"}}
	if _, err := manager.Spawn(t.Context(), spec, "review"); err != nil {
		t.Fatal(err)
	}
	conn := <-started
	select {
	case completion := <-completed:
		if completion.Err != nil {
			t.Fatal(completion.Err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no completion")
	}
	if err := manager.Stop(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	waitFakeLSPExit(t, conn)
}

func TestResidentChildSpecProbeClosesItsLSPServer(t *testing.T) {
	cwd := t.TempDir()
	listener := fakeLSPWorkspace(t, cwd)
	var conn net.Conn
	previous := residentProbeResolve
	residentProbeResolve = func(args Args, requireCred bool) (Resolved, error) {
		resolved, err := previous(args, requireCred)
		if err == nil {
			conn = startFakeLSP(t, resolved.ToolRegistry, cwd, listener)
		}
		return resolved, err
	}
	t.Cleanup(func() { residentProbeResolve = previous })
	runtime := newSubagentRuntime(subagentRuntimeConfig{
		Args: Args{NoContextFiles: true, NoSkill: true}, Root: t.TempDir(), RepoRoot: cwd,
		Provider: "openai", Model: "gpt-4o",
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	if _, err := runtime.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{Task: "review"}, core.Registry{"read": nil}); err != nil {
		t.Fatal(err)
	}
	if conn == nil {
		t.Fatal("probe resolve did not run")
	}
	waitFakeLSPExit(t, conn)
}

func TestCloseLSPManagersClosesManagerReferencedOnlyByAST(t *testing.T) {
	cwd := t.TempDir()
	listener := fakeLSPWorkspace(t, cwd)
	manager := lsp.NewManager()
	t.Cleanup(func() { _ = manager.Close() })
	conn := startFakeLSP(t, core.Registry{"lsp": tools.NewLSPTool(cwd, manager)}, cwd, listener)
	if err := tools.CloseLSPManagers(core.Registry{"ast": &tools.ASTTool{CWD: cwd, LSP: manager}}); err != nil {
		t.Fatal(err)
	}
	waitFakeLSPExit(t, conn)
}
