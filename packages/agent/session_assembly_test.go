package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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
	return append(sessionAssemblyKeylessArgs(server), "--api-key", "test-key")
}

func sessionAssemblyKeylessArgs(server *httptest.Server) []string {
	return []string{"--provider", "openai", "--model", "gpt-5", "--base-url", server.URL, "--no-ext", "--no-mcp"}
}

// toolCallProvider answers the first request with one bash call and later
// requests with a final message, recording each Authorization header.
func toolCallProvider(t *testing.T, command string, auths *[]string) *httptest.Server {
	args, _ := json.Marshal(map[string]any{"command": command, "timeout": 10})
	return toolCallProviderFor(t, "bash", string(args), auths)
}

func toolCallProviderFor(t *testing.T, tool, arguments string, auths *[]string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		*auths = append(*auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("content-type", "text/event-stream")
		var chunk map[string]any
		if calls.Add(1) == 1 {
			chunk = map[string]any{"choices": []any{map[string]any{"index": 0, "finish_reason": "tool_calls",
				"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call-1", "type": "function",
					"function": map[string]string{"name": tool, "arguments": arguments}}}}}}}
		} else {
			chunk = map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": "done"}, "finish_reason": "stop"}}}
		}
		chunk["usage"] = map[string]int{"prompt_tokens": 8, "completion_tokens": 2}
		data, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
	}))
	t.Cleanup(server.Close)
	return server
}

// promptToolResult runs one turn and returns the text of the bash result.
func promptToolResult(t *testing.T, ag *core.Agent) (string, bool) {
	t.Helper()
	if err := ag.Prompt(context.Background(), "run it", nil, func(core.AgentEvent) {}); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	for _, m := range ag.Messages() {
		for _, c := range m.Content {
			if res, ok := c.(provider.ToolResultBlock); ok && res.CallID == "call-1" {
				var out strings.Builder
				for _, b := range res.Content {
					if text, ok := b.(provider.TextBlock); ok {
						out.WriteString(text.Text)
					}
				}
				return out.String(), res.IsError
			}
		}
	}
	t.Fatal("no tool result")
	return "", true
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

func TestNewSessionDepsResumeRequiresID(t *testing.T) {
	sessionAssemblyEnv(t)
	server := sessionAssemblyProvider(t)
	_, _, err := NewSessionDeps(context.Background(), session.CreateSpec{CWD: t.TempDir(), Env: []string{}, Args: append(sessionAssemblyArgs(server), "--resume")}, "test")
	if err == nil || !strings.Contains(err.Error(), "pick on the client") {
		t.Fatalf("err = %v, want resume-needs-id error", err)
	}
}

// The confirmation preview of worktree list runs git under the agent hook's
// context, so a hook bound to the create context fails once it is cancelled.
func TestNewSessionDepsOutlivesCreateContext(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	sessionAssemblyEnv(t)
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", repo}, {"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command(gitPath, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	var auths []string
	server := toolCallProviderFor(t, "worktree", `{"action":"list"}`, &auths)
	ctx, cancel := context.WithCancel(context.Background())
	deps, cleanup, err := NewSessionDeps(ctx, session.CreateSpec{CWD: repo, Env: []string{"PATH=" + os.Getenv("PATH")}, Args: append(sessionAssemblyArgs(server), "--no-yolo")}, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	cancel()
	deps.ConfirmGate.AllowAll()
	ag, _, _, err := deps.AgentBuilder.BuildAgent()
	if err != nil {
		t.Fatal(err)
	}
	out, isErr := promptToolResult(t, ag)
	if isErr || strings.Contains(out, "context canceled") {
		t.Fatalf("tool result after create ctx cancel = %q (error=%v)", out, isErr)
	}
}

func TestNewSessionDepsCredentialsFollowSpecEnv(t *testing.T) {
	sessionAssemblyEnv(t)
	t.Setenv("OPENAI_API_KEY", "")
	var auths []string
	server := toolCallProvider(t, "true", &auths)
	deps, _ := newTestSessionDeps(t, session.CreateSpec{CWD: t.TempDir(), Env: []string{"PATH=" + os.Getenv("PATH"), "OPENAI_API_KEY=spec-only-key"}, Args: sessionAssemblyKeylessArgs(server)})
	ag, _, _, err := deps.AgentBuilder.BuildAgent()
	if err != nil {
		t.Fatalf("spec env key not used: %v", err)
	}
	promptToolResult(t, ag)
	if len(auths) == 0 || auths[0] != "Bearer spec-only-key" {
		t.Fatalf("authorization = %v", auths)
	}

	t.Setenv("OPENAI_API_KEY", "daemon-only-key")
	other, _ := newTestSessionDeps(t, session.CreateSpec{CWD: t.TempDir(), Env: []string{"PATH=" + os.Getenv("PATH")}, Args: sessionAssemblyKeylessArgs(server)})
	if ag, _, _, err := other.AgentBuilder.BuildAgent(); err == nil {
		t.Fatalf("daemon env key leaked into session (agent %v)", ag != nil)
	}
}

// boundHooks binds the late hooks to a real controller and returns its events.
func boundHooks(t *testing.T, b *sessionBindings) <-chan session.Event {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := session.NewController(ctx, session.Deps{})
	events, err := c.Subscribe(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	b.setHooks(c.ExtensionHooks())
	return events
}

func nextEvent(t *testing.T, events <-chan session.Event) session.Event {
	t.Helper()
	select {
	case ev := <-events:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for event")
		return session.Event{}
	}
}

func TestSessionHooksKeepNotifyAndReportDrops(t *testing.T) {
	b := newSessionBindings()
	h := b.hooks()
	h.Notify("ext", "info", "first")
	for i := 0; i < maxBufferedSessionHookCalls+3; i++ {
		h.Display("ext", fmt.Sprintf("display-%d", i))
	}
	h.Notify("ext", "info", "last")
	events := boundHooks(t, b)

	var notes, displays []string
	for len(notes) < 3 || len(displays) < maxBufferedSessionHookCalls {
		ev := nextEvent(t, events)
		switch data := ev.Data.(type) {
		case session.NoteEvent:
			notes = append(notes, data.Note.Text)
		case session.ExtDisplayEvent:
			displays = append(displays, data.Text)
		}
	}
	if !strings.Contains(notes[0], "first") || !strings.Contains(notes[1], "last") || !strings.Contains(notes[2], "dropped 3") {
		t.Fatalf("notes = %q", notes)
	}
	if displays[0] != "display-3" || displays[len(displays)-1] != fmt.Sprintf("display-%d", maxBufferedSessionHookCalls+2) {
		t.Fatalf("displays kept = %q .. %q", displays[0], displays[len(displays)-1])
	}
}

func TestSessionHooksReplayPreservesOrderUnderConcurrency(t *testing.T) {
	b := newSessionBindings()
	h := b.hooks()
	for i := 0; i < 10; i++ {
		h.Notify("ext", "info", fmt.Sprintf("pre-%d", i))
	}
	const writers, perWriter = 4, 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < perWriter; i++ {
				h.Notify("ext", "info", fmt.Sprintf("w%d-%d", w, i))
			}
		}(w)
	}
	close(start)
	events := boundHooks(t, b)
	wg.Wait()

	next := map[string]int{}
	pre := 0
	for seen := 0; seen < 10+writers*perWriter; seen++ {
		text := strings.TrimPrefix(nextEvent(t, events).Data.(session.NoteEvent).Note.Text, "[ext] ")
		var w, i int
		if _, err := fmt.Sscanf(text, "w%d-%d", &w, &i); err == nil {
			key := fmt.Sprint(w)
			if i != next[key] {
				t.Fatalf("writer %d out of order: got %d want %d", w, i, next[key])
			}
			next[key]++
			continue
		}
		if text != fmt.Sprintf("pre-%d", pre) {
			t.Fatalf("buffered note %d = %q", pre, text)
		}
		pre++
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
	before := packageGoroutines()

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
	for {
		after := packageGoroutines()
		if len(after) <= len(before) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines in zut packages = %d, want <= %d\n%s", len(after), len(before), strings.Join(after, "\n\n"))
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

// packageGoroutines returns stacks of goroutines running zut code, excluding
// the test runner itself, so unrelated runtime/net goroutines do not count.
func packageGoroutines() []string {
	buf := make([]byte, 1<<22)
	buf = buf[:runtime.Stack(buf, true)]
	var out []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if !strings.Contains(g, "github.com/bnema/zut/packages/") {
			continue
		}
		if strings.Contains(g, "packageGoroutines") || strings.Contains(g, "testing.tRunner") && strings.Contains(g, "TestNewSessionDepsCleanup") {
			continue
		}
		out = append(out, g)
	}
	return out
}
