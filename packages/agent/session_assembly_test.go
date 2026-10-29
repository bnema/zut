package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// sessionAssemblyProvider is an OpenAI-compatible streaming endpoint; the
// assembly never needs a turn, so it only answers with a final message.
func sessionAssemblyProvider(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "text/event-stream")
		chunk := map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": "done"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 8, "completion_tokens": 2},
		}
		data, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
	}))
	t.Cleanup(server.Close)
	return server
}

func sessionAssemblyEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	t.Setenv("HOME", t.TempDir())
	return home
}

func sessionAssemblyArgs(server *httptest.Server) []string {
	return []string{"--provider", "openai", "--model", "gpt-5", "--base-url", server.URL, "--api-key", "test-key", "--no-ext", "--no-mcp"}
}

func newTestSessionDeps(t *testing.T, spec session.CreateSpec) (session.Deps, func()) {
	t.Helper()
	deps, cleanup, err := NewSessionDeps(context.Background(), spec, "test")
	if err != nil {
		t.Fatalf("NewSessionDeps: %v", err)
	}
	t.Cleanup(cleanup)
	return deps, cleanup
}

func TestNewSessionDepsRejectsRelativeCWD(t *testing.T) {
	sessionAssemblyEnv(t)
	if _, _, err := NewSessionDeps(context.Background(), session.CreateSpec{CWD: "relative", Env: []string{}}, "test"); err == nil {
		t.Fatal("relative cwd accepted")
	}
}

func TestNewSessionDepsTwoCWDs(t *testing.T) {
	sessionAssemblyEnv(t)
	server := sessionAssemblyProvider(t)
	pathEnv := "PATH=" + os.Getenv("PATH")
	type built struct {
		cwd, mark string
		deps      session.Deps
		agent     *core.Agent
	}
	var sessions []built
	for _, mark := range []string{"alpha", "beta"} {
		cwd := t.TempDir()
		env := []string{pathEnv, "ZUT_L3_MARK=" + mark}
		deps, _ := newTestSessionDeps(t, session.CreateSpec{CWD: cwd, Env: env, Args: sessionAssemblyArgs(server)})
		ag, providerName, model, err := deps.AgentBuilder.BuildAgent()
		if err != nil || ag == nil {
			t.Fatalf("BuildAgent: %v", err)
		}
		if providerName != "openai" || model != "gpt-5" {
			t.Fatalf("selection = %s/%s", providerName, model)
		}
		if deps.CWD != cwd {
			t.Fatalf("deps cwd = %q, want %q", deps.CWD, cwd)
		}
		if strings.Join(deps.Env, "\n") != strings.Join(env, "\n") {
			t.Fatalf("deps env = %v, want %v", deps.Env, env)
		}
		if got := deps.SessionStore.CurrentSessionPath(); got == "" {
			t.Fatal("no session file opened")
		}
		if deps.ConfirmGate == nil || deps.Extensions == nil || deps.AuthManager == nil {
			t.Fatal("deps missing gate, extensions or auth manager")
		}
		sessions = append(sessions, built{cwd: cwd, mark: mark, deps: deps, agent: ag})
	}
	if sessions[0].deps.SessionStore.CurrentSessionPath() == sessions[1].deps.SessionStore.CurrentSessionPath() {
		t.Fatal("sessions share a session file")
	}
	for _, s := range sessions {
		reg := s.agent.ToolsSnapshot()
		bash, ok := reg["bash"].(*tools.BashTool)
		if !ok {
			t.Fatal("bash tool missing")
		}
		if bash.CWD != s.cwd {
			t.Fatalf("bash cwd = %q, want %q", bash.CWD, s.cwd)
		}
		read, ok := reg["read"].(*tools.ReadTool)
		if !ok || read.CWD != s.cwd {
			t.Fatalf("read tool not rooted at %q", s.cwd)
		}
		if runtime.GOOS == "windows" {
			continue
		}
		res, err := bash.Execute(context.Background(), json.RawMessage(`{"command":"pwd; echo mark=$ZUT_L3_MARK","timeout":10}`), nil)
		if err != nil {
			t.Fatalf("bash: %v", err)
		}
		var out strings.Builder
		for _, block := range res.Content {
			if text, ok := block.(provider.TextBlock); ok {
				out.WriteString(text.Text)
			}
		}
		resolved, _ := filepath.EvalSymlinks(s.cwd)
		if !strings.Contains(out.String(), "mark="+s.mark) {
			t.Fatalf("bash env leaked: %q", out.String())
		}
		if !strings.Contains(out.String(), s.cwd) && !strings.Contains(out.String(), resolved) {
			t.Fatalf("bash ran outside %q: %q", s.cwd, out.String())
		}
		other := "alpha"
		if s.mark == "alpha" {
			other = "beta"
		}
		if strings.Contains(out.String(), "mark="+other) {
			t.Fatalf("bash saw the other session env: %q", out.String())
		}
	}
}

func TestNewSessionDepsResume(t *testing.T) {
	home := sessionAssemblyEnv(t)
	server := sessionAssemblyProvider(t)
	cwd := t.TempDir()
	sess, err := core.NewSession(home, cwd, "openai", "gpt-5", "test")
	if err != nil {
		t.Fatal(err)
	}
	transcript := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "plan it"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{
			ID: "plan-1", Name: "plan", Arguments: []byte(`{"action":"set","steps":[{"step":"one","status":"pending"}]}`),
		}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{
			CallID: "plan-1", Content: []provider.Content{provider.TextBlock{Text: "Plan updated"}},
		}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "planned"}}},
	}
	for _, m := range transcript {
		if err := sess.AppendMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	path, id := sess.Path, sess.ID
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	deps, _ := newTestSessionDeps(t, session.CreateSpec{CWD: cwd, Env: []string{}, Args: sessionAssemblyArgs(server), ResumePath: path})
	if got := deps.SessionStore.CurrentSessionPath(); got != path {
		t.Fatalf("session path = %q, want %q", got, path)
	}
	if _, ok := deps.SessionStore.InitialPlanSnapshots()["plan-1"]; !ok {
		t.Fatalf("plan snapshots = %#v", deps.SessionStore.InitialPlanSnapshots())
	}
	ag, _, _, err := deps.AgentBuilder.BuildAgent()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(ag.Messages()); got != len(transcript) {
		t.Fatalf("resumed messages = %d, want %d", got, len(transcript))
	}
	if ag.SessionID() != id {
		t.Fatalf("agent session id = %q, want %q", ag.SessionID(), id)
	}
}

func TestNewSessionDepsCleanup(t *testing.T) {
	sessionAssemblyEnv(t)
	server := sessionAssemblyProvider(t)
	// Warm package-level lazies (HTTP transports, catalogs) before counting.
	_, warm, err := NewSessionDeps(context.Background(), session.CreateSpec{CWD: t.TempDir(), Env: []string{}, Args: sessionAssemblyArgs(server)}, "test")
	if err != nil {
		t.Fatal(err)
	}
	warm()
	before := runtime.NumGoroutine()

	deps, cleanup, err := NewSessionDeps(context.Background(), session.CreateSpec{CWD: t.TempDir(), Env: []string{}, Args: sessionAssemblyArgs(server)}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := deps.AgentBuilder.BuildAgent(); err != nil {
		t.Fatal(err)
	}
	path := deps.SessionStore.CurrentSessionPath()
	cleanup()
	cleanup() // idempotent

	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			t.Fatalf("goroutines = %d, want <= %d\n%s", runtime.NumGoroutine(), before, buf[:n])
		}
		time.Sleep(10 * time.Millisecond)
	}
	if deps.SessionStore.CurrentSessionPath() != "" {
		t.Fatal("session file still open after cleanup")
	}
	// A fresh, empty session file is removed on close.
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("empty session %s was not cleaned up", path)
	}
}
