package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/daemonclient"
	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// rpcFrames decodes the bridge's NDJSON output.
func rpcFrames(t *testing.T, out string) []map[string]any {
	t.Helper()
	var frames []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var f map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &f), line)
		frames = append(frames, f)
	}
	return frames
}

// offlineBridge is a bridge without a daemon: only paths that answer before
// touching the session port are valid on it.
func offlineBridge(out *bytes.Buffer, info session.Info) *rpcBridge {
	return &rpcBridge{h: &headlessSession{}, out: out, info: info}
}

func TestRPCBridgeExtAlertIsJSONOnly(t *testing.T) {
	var out bytes.Buffer
	b := offlineBridge(&out, session.Info{})
	b.helloDone = true
	events := make(chan session.Event, 1)
	events <- session.Event{Kind: session.EvExtAlert, Data: session.ExtAlertEvent{Ext: "notify", Alert: extproto.AlertRequest{Kind: extproto.AlertKindBell, Reason: "question_ready"}}}
	close(events)
	done := make(chan struct{})
	b.pump(nil, events, done)
	<-done
	require.NotContains(t, out.String(), "\a", "RPC alert contains raw BEL")
	frames := rpcFrames(t, out.String())
	require.Len(t, frames, 1)
	require.Equal(t, "ext_alert", frames[0]["type"])
	require.Equal(t, "notify", frames[0]["extension"])
	require.Equal(t, extproto.AlertKindBell, frames[0]["kind"])
	require.Equal(t, "question_ready", frames[0]["reason"])
}

// A pending request is declined on the port of the stream that raised it,
// even after clear has moved b.h to a successor session.
func TestRPCBridgePendingDeclineUsesStreamPort(t *testing.T) {
	var out bytes.Buffer
	b := offlineBridge(&out, session.Info{})
	old := sessionmocks.NewMockPort(t)
	answered := make(chan string, 1)
	old.EXPECT().AnswerConfirmation(mock.Anything, "req1", core.ConfirmDecision{Allow: true}).RunAndReturn(func(context.Context, string, core.ConfirmDecision) error {
		answered <- "req1"
		return nil
	}).Once()
	events := make(chan session.Event, 1)
	events <- session.Event{Kind: session.EvPending, Data: session.PendingEvent{Pending: session.PendingRequest{ID: "req1", Kind: "confirm"}}}
	close(events)
	done := make(chan struct{})
	go b.pump(old, events, done)
	// clear swaps the session concurrently; the decline must ignore it.
	b.h = &headlessSession{id: "successor"}
	<-done
	select {
	case id := <-answered:
		require.Equal(t, "req1", id)
	case <-time.After(5 * time.Second):
		t.Fatal("pending request not declined on its stream's port")
	}
}

func TestRPCBridgeRequiresAuthentication(t *testing.T) {
	var out bytes.Buffer
	b := offlineBridge(&out, session.Info{})
	b.token = "synthetic-token"
	require.NoError(t, b.handleLine(context.Background(), `{"type":"ping","id":"1"}`))
	require.Error(t, b.handleLine(context.Background(), `{"type":"hello","id":"2","token":"wrong"}`))
	require.False(t, b.authed)
	require.NoError(t, b.handleLine(context.Background(), `{"type":"hello","id":"3","token":"synthetic-token"}`))
	require.True(t, b.authed)
	frames := rpcFrames(t, out.String())
	require.Len(t, frames, 3)
	require.Equal(t, false, frames[0]["success"])
	require.Contains(t, frames[0]["error"], "auth required")
	require.Equal(t, false, frames[1]["success"])
	require.Equal(t, true, frames[2]["success"])
}

func TestRPCBridgeSetModelRejectsBusyTurn(t *testing.T) {
	var out bytes.Buffer
	b := offlineBridge(&out, session.Info{Provider: provider.ProviderOpenCodeGo, Model: "old-model", Run: session.RunStateRunning})
	b.dispatch(context.Background(), "set_model", "1", []byte(`{"model":"new-model"}`))
	frames := rpcFrames(t, out.String())
	require.Len(t, frames, 1)
	require.Equal(t, false, frames[0]["success"])
	require.Equal(t, "runtime is busy", frames[0]["error"])
}

func TestRPCBridgeSetModelRejectsBlankModel(t *testing.T) {
	for _, model := range []string{"", "   "} {
		var out bytes.Buffer
		b := offlineBridge(&out, session.Info{Provider: provider.ProviderOpenCodeGo, Model: "kimi-k2.6", Run: session.RunStateIdle})
		b.dispatch(context.Background(), "set_model", "1", []byte(`{"model":"`+model+`"}`))
		frames := rpcFrames(t, out.String())
		require.Len(t, frames, 1, "model %q", model)
		require.Equal(t, false, frames[0]["success"], "model %q", model)
	}
}

func TestRPCBridgeSetReasoningRejectsUnknownLevel(t *testing.T) {
	var out bytes.Buffer
	b := offlineBridge(&out, session.Info{})
	b.dispatch(context.Background(), "set_reasoning", "1", []byte(`{"reasoning":"extreme"}`))
	frames := rpcFrames(t, out.String())
	require.Len(t, frames, 1)
	require.Equal(t, false, frames[0]["success"])
}

func TestResolveRPCModelAllowsUnlistedOpenCodeGoModel(t *testing.T) {
	preserveProviderCatalog(t)
	model, err := resolveRPCModel(provider.ProviderOpenCodeGo, "muse-spark-1.2-contributor")
	require.NoError(t, err)
	require.Equal(t, "muse-spark-1.2-contributor", model.ID)
	require.Equal(t, 128000, model.ContextWindow)
	require.Equal(t, 16384, model.MaxOutput)
}

func TestResolveRPCModelUsesKnownModelMetadata(t *testing.T) {
	preserveProviderCatalog(t)
	provider.SetLiveModels([]provider.Model{{Provider: provider.ProviderOpenCodeGo, ID: "served-model", ContextWindow: 300000, MaxOutput: 60000}})
	model, err := resolveRPCModel(provider.ProviderOpenCodeGo, "served-model")
	require.NoError(t, err)
	require.Equal(t, 300000, model.ContextWindow)
	require.Equal(t, 60000, model.MaxOutput)
}

// syncWriter lets the test read bridge output while the bridge writes it.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestRPCBridgeSettingsAndClearOnDaemon drives set_reasoning, set_model,
// prompt and clear through a real daemon session, one command at a time:
// settings reach the session and clear leaves an empty successor.
func TestRPCBridgeSettingsAndClearOnDaemon(t *testing.T) {
	home := sessionAssemblyEnv(t)
	server := sessionAssemblyProvider(t)
	td := startFactoryDaemon(t, home, daemonTestSocket(t))
	cwd := t.TempDir()
	raw := []string{"--rpc", "--provider", "openai", "--model", "gpt-5", "--base-url", server.URL, "--api-key", "test-key",
		"--no-ext", "--no-mcp", "--no-lsp", "--no-context-files", "--cwd", cwd}
	args, err := ParseArgs(raw)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	in, feed := io.Pipe()
	stdout, stderr := &syncWriter{}, &bytes.Buffer{}
	hio := headlessIO{stdout: stdout, stderr: stderr, env: parityEnv(),
		ensure: daemonclient.EnsureOptions{SocketPath: td.socket, Options: daemonclient.Options{ClientVersion: "test", ClientKind: "headless"}}}
	done := make(chan error, 1)
	go func() { done <- runRPCBridge(ctx, hio, args, raw, "test", in) }()

	frame := func(match func(map[string]any) bool) map[string]any {
		t.Helper()
		var found map[string]any
		require.Eventually(t, func() bool {
			for _, f := range rpcFrames(t, stdout.String()) {
				if match(f) {
					found = f
					return true
				}
			}
			return false
		}, 30*time.Second, 10*time.Millisecond, "stdout: %s", stdout.String())
		return found
	}
	send := func(id, line string) map[string]any {
		t.Helper()
		_, err := io.WriteString(feed, line+"\n")
		require.NoError(t, err)
		f := frame(func(f map[string]any) bool { return f["type"] == "response" && f["id"] == id })
		require.Equal(t, true, f["success"], "response %s: %v", id, f)
		return f
	}
	data := func(f map[string]any) map[string]any { return f["data"].(map[string]any) }

	require.Equal(t, "max", data(send("1", `{"id":"1","type":"set_reasoning","reasoning":"max"}`))["reasoning"])
	require.Equal(t, "gpt-5-mini", data(send("2", `{"id":"2","type":"set_model","model":"  gpt-5-mini  "}`))["model"])
	// Info reaches the bridge as an event after the setting response. The
	// session autosaves into its active profile slot, which clamps reasoning
	// to the new model (gpt-5-mini tops out at high).
	n := 0
	require.Eventually(t, func() bool {
		n++
		id := fmt.Sprintf("3.%d", n)
		st := data(send(id, `{"id":"`+id+`","type":"get_state"}`))
		return st["model"] == "gpt-5-mini" && st["reasoning"] == "high"
	}, 10*time.Second, 20*time.Millisecond)
	send("4", `{"id":"4","type":"prompt","message":"hello"}`)
	frame(func(f map[string]any) bool { return f["type"] == "done" })
	require.Len(t, data(send("5", `{"id":"5","type":"get_messages"}`))["messages"], 3, "time context, prompt and reply")
	send("6", `{"id":"6","type":"clear"}`)
	require.Empty(t, data(send("7", `{"id":"7","type":"get_messages"}`))["messages"], "clear kept the transcript")
	require.NoError(t, feed.Close())
	require.NoError(t, <-done, "stderr: %s", stderr.String())
}

func TestRPCBridgeAlertsWaitForHello(t *testing.T) {
	for _, token := range []string{"", "synthetic-token"} {
		t.Run("token="+token, func(t *testing.T) {
			var out bytes.Buffer
			b := offlineBridge(&out, session.Info{})
			b.token = token
			alert := func(reason string) {
				events := make(chan session.Event, 1)
				events <- session.Event{Kind: session.EvExtAlert, Data: session.ExtAlertEvent{Ext: "notify", Alert: extproto.AlertRequest{Kind: extproto.AlertKindBell, Reason: reason}}}
				close(events)
				b.pump(nil, events, make(chan struct{}))
			}
			for i := 0; i < 66; i++ {
				alert(fmt.Sprint(i))
			}
			require.Empty(t, out.String(), "alerts must not precede hello")
			if token != "" {
				require.Error(t, b.handleLine(context.Background(), `{"type":"hello","token":"wrong"}`))
				require.Len(t, rpcFrames(t, out.String()), 1, "bad auth must not flush alerts")
				out.Reset()
			}
			hello := fmt.Sprintf(`{"type":"hello","token":%q}`, token)
			require.NoError(t, b.handleLine(context.Background(), hello))
			frames := rpcFrames(t, out.String())
			require.Len(t, frames, 65)
			require.Equal(t, "hello", frames[0]["command"])
			for i, frame := range frames[1:] {
				require.Equal(t, "ext_alert", frame["type"])
				require.Equal(t, fmt.Sprint(i+2), frame["reason"])
			}
			out.Reset()
			alert("live")
			require.NoError(t, b.handleLine(context.Background(), hello))
			frames = rpcFrames(t, out.String())
			require.Len(t, frames, 2, "repeated hello must not replay alerts")
			require.Equal(t, "live", frames[0]["reason"])
			require.NotContains(t, out.String(), "\a")
		})
	}
}
