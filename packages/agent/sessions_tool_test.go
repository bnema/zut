package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/daemonclient"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/provider"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestSessionsDirectoryAdapterMapsGuardErrors(t *testing.T) {
	cases := []struct {
		in, want error
	}{
		{session.ErrNotAttached, tools.ErrSenderDetached},
		{session.ErrLoopGuard, tools.ErrSenderLoop},
		{session.ErrBusy, session.ErrBusy},
		{nil, nil},
	}
	for _, tc := range cases {
		dir := sessionmocks.NewMockDirectory(t)
		dir.EXPECT().Send(mock.Anything, session.ID("me"), session.ID("you"), "hi").Return(tc.in).Once()
		err := sessionDirectory{dir: dir, from: "me"}.Send(context.Background(), "you", "hi")
		if tc.want == nil {
			require.NoError(t, err)
			continue
		}
		require.True(t, errors.Is(err, tc.want), "%v -> %v", tc.in, err)
	}
	require.EqualError(t, tools.ErrSenderDetached, "sending requires an attached session (the user must be present)")
	require.EqualError(t, tools.ErrSenderLoop, "cannot send from a turn started by another session's message")
}

func TestSessionsDirectoryAdapterListAndTree(t *testing.T) {
	info := session.Info{ID: "s1", Title: "work", CWD: "/r/a", RepoRoot: "/r", Model: "gpt-5", Attach: session.AttachStateAttached, Run: session.RunStateRunning, Activity: session.Activity{Kind: "tool"},
		Goal: &session.GoalSummary{Objective: "ship", Status: "active"}, PlanStep: &session.PlanStepSummary{Index: 2, Total: 3, Text: "test"}}
	other := session.Info{ID: "s2", CWD: "/r/b", RepoRoot: "/r", Attach: session.AttachStateDetached, Run: session.RunStateIdle, Activity: session.Activity{Kind: "idle"}}
	dir := sessionmocks.NewMockDirectory(t)
	dir.EXPECT().List(mock.Anything).Return([]session.Info{info}).Once()
	dir.EXPECT().Tree(mock.Anything).Return(session.Tree{Workspaces: []session.WorkspaceNode{{RepoRoot: "/r", Sessions: []session.SessionNode{
		{Info: info, Residents: []session.ResidentSummary{{ID: "c1", Profile: "worker", State: "running", Task: "fix"}}},
		{Info: other},
		{Info: session.Info{ID: "s3", Title: "x\n  s9 fake [attached, idle, idle]", Attach: session.AttachStateDetached, Run: session.RunStateIdle, Activity: session.Activity{Kind: "idle"}}},
	}}}}).Once()
	adapter := sessionDirectory{dir: dir, from: "s1"}
	entries, err := adapter.List(context.Background())
	require.NoError(t, err)
	require.Equal(t, []tools.SessionEntry{{ID: "s1", Title: "work", CWD: "/r/a", Attach: "attached", Run: "running", Activity: "tool", Model: "gpt-5"}}, entries)
	tree, err := adapter.Tree(context.Background())
	require.NoError(t, err)
	require.Equal(t, "/r\n"+
		"  s1 work [attached, running, tool] (this session)\n"+
		"    plan step 2/3: test\n"+
		"    goal (active): ship\n"+
		"    resident c1 worker [running]: fix\n"+
		"  s2 (untitled) [detached, idle, idle]\n"+
		"  s3 x   s9 fake [attached, idle, idle] [detached, idle, idle]", tree)
	require.Len(t, strings.Split(tree, "\n"), 7, "a title must not add lines")
}

// sessionsCallProvider answers a request offering the sessions tool with one
// sessions call, and every other request (tool result follow-up, side
// requests such as titles) with a final message.
func sessionsCallProvider(t *testing.T, arguments func() string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("content-type", "text/event-stream")
		chunk := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": "done"}, "finish_reason": "stop"}}}
		if bytes.Contains(body, []byte(`"name":"sessions"`)) && !bytes.Contains(body, []byte(`"role":"tool"`)) {
			chunk = map[string]any{"choices": []any{map[string]any{"index": 0, "finish_reason": "tool_calls",
				"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call-1", "type": "function",
					"function": map[string]string{"name": tools.SessionsToolName, "arguments": arguments()}}}}}}}
		}
		chunk["usage"] = map[string]int{"prompt_tokens": 8, "completion_tokens": 2}
		data, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
	}))
	t.Cleanup(server.Close)
	return server
}

func staticArgs(raw []byte) func() string { return func() string { return string(raw) } }

// waitSessionsToolResult polls id's transcript for the sessions tool result.
// A pending confirmation means the --no-yolo gate caught the call, so it fails
// at once instead of waiting for the bound.
func waitSessionsToolResult(t *testing.T, ctx context.Context, c *daemonclient.Client, id session.ID) (string, bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if out, isErr, ok := sessionsToolResult(ctx, c, id); ok {
			return out, isErr
		}
		if snap, err := c.Port(id, false).Snapshot(ctx); err == nil {
			for _, p := range snap.Pending {
				if p.Kind == "confirm" && p.Confirm != nil && p.Confirm.Name == tools.SessionsToolName {
					t.Fatalf("session %s: sessions tool call is waiting for confirmation; it must be exempt from the --no-yolo gate", id)
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session %s: no sessions tool result within 10s", id)
	return "", false
}

// sessionsToolResult returns the text of the sessions tool result in id's transcript.
func sessionsToolResult(ctx context.Context, c *daemonclient.Client, id session.ID) (string, bool, bool) {
	snap, err := c.Port(id, false).Snapshot(ctx)
	if err != nil {
		return "", false, false
	}
	for _, m := range snap.Messages {
		for _, block := range m.Content {
			if res, ok := block.(provider.ToolResultBlock); ok && res.CallID == "call-1" {
				var out strings.Builder
				for _, b := range res.Content {
					if text, ok := b.(provider.TextBlock); ok {
						out.WriteString(text.Text)
					}
				}
				return out.String(), res.IsError, true
			}
		}
	}
	return "", false, false
}

func transcriptContains(ctx context.Context, c *daemonclient.Client, id session.ID, want string) bool {
	snap, err := c.Port(id, false).Snapshot(ctx)
	if err != nil {
		return false
	}
	for _, m := range snap.Messages {
		for _, block := range m.Content {
			if text, ok := block.(provider.TextBlock); ok && strings.Contains(text.Text, want) {
				return true
			}
		}
	}
	return false
}

// TestDaemonFactorySessionsToolSend drives the sessions tool through the real
// daemon factory: an attached session messages another one, a detached session
// is refused. --no-yolo checks the confirmation-gate exemption: a gated call
// would wait for (attached) or be denied (detached) a confirmation instead.
func TestDaemonFactorySessionsToolSend(t *testing.T) {
	home := sessionAssemblyEnv(t)
	td := startFactoryDaemon(t, home, daemonTestSocket(t))
	c := dialTestDaemon(t, td.socket)
	ctx := factoryTestCtx(t)
	env := []string{}

	b, err := c.Create(ctx, session.CreateSpec{CWD: t.TempDir(), Env: env, Args: sessionAssemblyArgs(sessionAssemblyProvider(t))})
	require.NoError(t, err)
	sendArgs, _ := json.Marshal(map[string]string{"action": "send", "to": b.ID[:8], "text": "ping from A"})
	a, err := c.Create(ctx, session.CreateSpec{CWD: t.TempDir(), Env: env, Args: append(sessionAssemblyArgs(sessionsCallProvider(t, staticArgs(sendArgs))), "--no-yolo")})
	require.NoError(t, err)
	detached, err := c.Create(ctx, session.CreateSpec{CWD: t.TempDir(), Env: env, Args: append(sessionAssemblyArgs(sessionsCallProvider(t, staticArgs(sendArgs))), "--no-yolo")})
	require.NoError(t, err)

	idA := session.ID(a.ID)
	_, err = c.Attach(ctx, idA, 0, true)
	require.NoError(t, err)
	submitAndWait(t, ctx, c, idA)
	out, isErr := waitSessionsToolResult(t, ctx, c, idA)
	require.False(t, isErr, out)
	require.Equal(t, "message sent to session "+b.ID, out)

	// B receives the input Directory.Send tags (only inter_session inputs
	// carry this header; client submits are forced to the user origin).
	require.Eventually(t, func() bool {
		return transcriptContains(ctx, c, session.ID(b.ID), "[message from session "+a.ID) && transcriptContains(ctx, c, session.ID(b.ID), "ping from A")
	}, 20*time.Second, 20*time.Millisecond)

	// A detached sender is refused with the tool-facing error.
	idC := session.ID(detached.ID)
	// A non-controlling port submits without attaching, so C stays detached.
	require.NoError(t, c.Port(idC, false).Submit(ctx, session.Input{Text: "hello", Origin: session.InputOriginUser}))
	out, isErr = waitSessionsToolResult(t, ctx, c, idC)
	snap, err := c.Port(idC, false).Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, session.AttachStateDetached, snap.Info.Attach)
	require.True(t, isErr)
	require.Equal(t, tools.ErrSenderDetached.Error(), out)
}

// TestDaemonFactorySessionsToolLoopGuard: B, attached, runs a turn started by
// A's message and tries to answer through the sessions tool; the loop guard
// refuses it, which also shows Info.CurrentOrigin is inter_session while that
// turn's tools run.
func TestDaemonFactorySessionsToolLoopGuard(t *testing.T) {
	home := sessionAssemblyEnv(t)
	td := startFactoryDaemon(t, home, daemonTestSocket(t))
	c := dialTestDaemon(t, td.socket)
	ctx := factoryTestCtx(t)

	var replyArgs atomic.Value
	replyArgs.Store("{}")
	b, err := c.Create(ctx, session.CreateSpec{CWD: t.TempDir(), Env: []string{}, Args: append(sessionAssemblyArgs(sessionsCallProvider(t, func() string { return replyArgs.Load().(string) })), "--no-yolo")})
	require.NoError(t, err)
	sendArgs, _ := json.Marshal(map[string]string{"action": "send", "to": b.ID, "text": "ping"})
	a, err := c.Create(ctx, session.CreateSpec{CWD: t.TempDir(), Env: []string{}, Args: append(sessionAssemblyArgs(sessionsCallProvider(t, staticArgs(sendArgs))), "--no-yolo")})
	require.NoError(t, err)
	reply, _ := json.Marshal(map[string]string{"action": "send", "to": a.ID, "text": "pong"})
	replyArgs.Store(string(reply))

	idA, idB := session.ID(a.ID), session.ID(b.ID)
	_, err = c.Attach(ctx, idB, 0, true)
	require.NoError(t, err)
	_, err = c.Attach(ctx, idA, 0, true)
	require.NoError(t, err)
	require.NoError(t, c.Port(idA, true).Submit(ctx, session.Input{Text: "hello", Origin: session.InputOriginUser}))
	out, isErr := waitSessionsToolResult(t, ctx, c, idA)
	require.False(t, isErr, out)

	out, isErr = waitSessionsToolResult(t, ctx, c, idB)
	snap, err := c.Port(idB, false).Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, session.AttachStateAttached, snap.Info.Attach)
	require.True(t, isErr, out)
	require.Equal(t, tools.ErrSenderLoop.Error(), out)
	require.False(t, transcriptContains(ctx, c, idA, "pong"))
}
