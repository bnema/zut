package modes

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

type btwToolTestClient struct {
	mu       sync.Mutex
	requests []provider.Request
	done     chan struct{}
}

func (c *btwToolTestClient) Name() string { return "btw-test" }

func (c *btwToolTestClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	call := len(c.requests)
	c.mu.Unlock()

	out := make(chan provider.Event, 4)
	go func() {
		defer close(out)
		if call == 1 {
			out <- provider.EventToolStart{ID: "tool-1", Name: "echo"}
			out <- provider.EventToolArgs{ID: "tool-1", Delta: `{"text":"hello"}`}
			out <- provider.EventToolEnd{ID: "tool-1"}
			out <- provider.EventDone{Stop: provider.StopToolUse, Message: provider.Message{
				Role: provider.RoleAssistant,
				Content: []provider.Content{provider.ToolCallBlock{
					ID: "tool-1", Name: "echo", Arguments: json.RawMessage(`{"text":"hello"}`),
				}},
			}}
			return
		}
		out <- provider.EventTextDelta{Delta: "tool finished"}
		out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
			Role: provider.RoleAssistant,
			Content: []provider.Content{
				provider.TextBlock{Text: "tool finished"},
			},
		}}
		close(c.done)
	}()
	return out, nil
}

type btwEchoTool struct {
	runs atomic.Int32
}

func (t *btwEchoTool) Name() string            { return "echo" }
func (t *btwEchoTool) Description() string     { return "echo text" }
func (t *btwEchoTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *btwEchoTool) Execute(context.Context, json.RawMessage, func(string)) (core.ToolResult, error) {
	t.runs.Add(1)
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "hello"}}}, nil
}

func TestBtwToolExpansionUsesSideChatView(t *testing.T) {
	result := strings.Repeat("output line\n", tui.ToolCollapseLines+10)
	d := &btwDialog{
		active:   true,
		toolView: &tui.View{Theme: tui.Dark},
		turns: []btwTurn{{
			User: "show output",
			Tools: []tui.ToolCallView{{
				ID: "tool-1", Name: "bash", Result: result, Done: true,
			}},
		}},
		editor: tui.NewEditor(""),
	}

	if rendered := strings.Join(d.Render(tui.Dark, 80), "\n"); !strings.Contains(rendered, "ctrl+o to expand") {
		t.Fatalf("long /btw tool result was not initially collapsed:\n%s", rendered)
	}
	d.ToggleToolExpansion()
	if rendered := strings.Join(d.Render(tui.Dark, 80), "\n"); strings.Contains(rendered, "ctrl+o to expand") {
		t.Fatalf("long /btw tool result remained collapsed after expansion:\n%s", rendered)
	}
}

func TestBtwCtrlOTogglesSideViewDuringConfirmation(t *testing.T) {
	resp := make(chan core.ConfirmDecision, 1)
	confirm := newConfirmDialog()
	confirm.Enqueue(&confirmRequest{toolName: "bash", resp: resp})
	btw := &btwDialog{active: true, toolView: &tui.View{}}
	i := &Interactive{
		view:          &tui.View{},
		btwDialog:     btw,
		confirmDialog: confirm,
		dirty:         make(chan struct{}, 1),
	}

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlO})
	if !btw.toolView.ExpandAll {
		t.Fatal("ctrl+o did not expand /btw tool output during confirmation")
	}
	if i.view.ExpandAll {
		t.Fatal("ctrl+o expanded the main view instead of the active /btw view")
	}
	if !confirm.Active() {
		t.Fatal("ctrl+o closed the confirmation dialog")
	}
	select {
	case decision := <-resp:
		t.Fatalf("ctrl+o unexpectedly answered confirmation: %+v", decision)
	default:
	}
}

func TestBtwDialogRunsToolsWithMainAgentPolicyInIsolatedTranscript(t *testing.T) {
	client := &btwToolTestClient{done: make(chan struct{})}
	tool := &btwEchoTool{}
	main := core.NewAgent(client, "main-model", "main-system", core.Registry{"echo": tool})
	main.SetMessages([]provider.Message{{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "frozen context"}},
	}})
	var policyCalls atomic.Int32
	var policyOrigin string
	main.BeforeToolExecute = func(call provider.ToolCallBlock) (bool, string, json.RawMessage) {
		policyCalls.Add(1)
		policyOrigin = call.Origin
		return true, "", nil
	}

	d := newBtwDialog()
	d.Open(tui.Theme{}, main, main.System, main.Model, t.TempDir(), "use the echo tool", false, false, true, nil)

	select {
	case <-client.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for /btw tool turn")
	}
	deadline := time.Now().Add(2 * time.Second)
	for d.Loading() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if d.Loading() {
		t.Fatal("/btw remained loading after the agent turn completed")
	}

	if got := tool.runs.Load(); got != 1 {
		t.Fatalf("tool executions = %d, want 1", got)
	}
	if got := policyCalls.Load(); got != 1 {
		t.Fatalf("main agent tool-policy calls = %d, want 1", got)
	}
	if policyOrigin != btwOrigin(1) {
		t.Fatalf("side-chat tool policy origin = %q, want %q", policyOrigin, btwOrigin(1))
	}

	client.mu.Lock()
	requests := append([]provider.Request(nil), client.requests...)
	client.mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(requests))
	}
	if len(requests[0].Tools) != 1 || requests[0].Tools[0].Name != "echo" {
		t.Fatalf("first /btw request tools = %#v, want echo", requests[0].Tools)
	}
	if got := len(main.Messages()); got != 1 {
		t.Fatalf("main transcript messages = %d, want frozen length 1", got)
	}

	d.mu.Lock()
	if len(d.turns) != 1 {
		t.Fatalf("side-chat turns = %d, want 1", len(d.turns))
	}
	turn := d.turns[0]
	if turn.Assistant != "tool finished" {
		t.Fatalf("assistant = %q, want %q", turn.Assistant, "tool finished")
	}
	if len(turn.Tools) != 1 || !turn.Tools[0].Done || turn.Tools[0].Error {
		t.Fatalf("rendered tool state = %#v, want one successful completed tool", turn.Tools)
	}
	d.mu.Unlock()

	const paneWidth = 62
	rows := d.Render(tui.Theme{}, paneWidth)
	rendered := strings.Join(rows, "\n")
	if !strings.Contains(rendered, "┌─") || !strings.Contains(rendered, "echo") || !strings.Contains(rendered, "└─") {
		t.Fatalf("/btw did not use the standard bordered tool renderer:\n%s", rendered)
	}
	toolRow := -1
	for idx, row := range rows {
		if strings.Contains(row, "┌─") {
			toolRow = idx
			break
		}
	}
	if toolRow < 1 || rows[toolRow-1] != "" {
		t.Fatalf("/btw tool box is not separated from the user bubble by a blank row:\n%s", rendered)
	}
	if cursorRow, _ := d.CursorPos(paneWidth); cursorRow != len(rows)-3 {
		t.Fatalf("cursor row = %d, want editor row %d after tool spacing change", cursorRow, len(rows)-3)
	}
}

func TestBtwActivityRendersFactDrivenLabel(t *testing.T) {
	d := &btwDialog{
		active:   true,
		loading:  true,
		theme:    tui.Dark,
		toolView: &tui.View{Theme: tui.Dark},
		spin:     newSpinner(tui.Dark),
		activity: newAgentActivity("anthropic", "claude-test"),
	}
	d.spin.Start()
	d.activity.apply(core.EvRequestStarted{Provider: "anthropic", Model: "claude-test", Scope: core.RetryScopeProvider, Attempt: 1, MaxAttempts: 3})

	rendered := strings.Join(d.Render(tui.Dark, 80), "\n")
	if !strings.Contains(rendered, "Sending request to anthropic") {
		t.Fatalf("/btw activity = %q, want factual request label", rendered)
	}
	if strings.Contains(rendered, "reticulating splines") || strings.Contains(rendered, "thinking") {
		t.Fatalf("/btw activity retained random spinner wording: %q", rendered)
	}
}

func TestBtwDialogIgnoresCallbacksFromPreviousGeneration(t *testing.T) {
	d := &btwDialog{
		active:     true,
		loading:    true,
		generation: 2,
		turns:      []btwTurn{{User: "new question"}},
		activity:   newAgentActivity("provider", "model"),
	}

	d.handleAgentEvent(0, 1, core.EvTextDelta{Delta: "stale response"})
	d.completeTurn(0, 1, "stale error")

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.turns[0].Assistant != "" || d.turns[0].Err != "" {
		t.Fatalf("stale callback changed reopened turn: %#v", d.turns[0])
	}
	if !d.loading {
		t.Fatal("stale completion cleared the reopened dialog loading state")
	}
}

func TestBtwToolConfirmationRoutesThroughSideAgent(t *testing.T) {
	client := &btwToolTestClient{done: make(chan struct{})}
	tool := &btwEchoTool{}
	main := core.NewAgent(client, "main-model", "main-system", core.Registry{"echo": tool})
	d := newBtwDialog()
	i := &Interactive{
		btwDialog:     d,
		confirmDialog: newConfirmDialog(),
		dirty:         make(chan struct{}, 1),
	}
	gate := core.NewConfirmGate(i)
	mainResp := make(chan core.ConfirmDecision, 1)
	i.confirmDialog.Enqueue(&confirmRequest{toolName: "main", resp: mainResp})
	i.confirmDialog.Blur()
	main.BeforeToolExecute = func(call provider.ToolCallBlock) (bool, string, json.RawMessage) {
		return gate.CheckToolCall(core.ToolCallConfirmation{
			ID:      call.ID,
			Name:    call.Name,
			Summary: "hello",
			Origin:  call.Origin,
		})
	}

	d.Open(tui.Dark, main, main.System, main.Model, t.TempDir(), "use the echo tool", false, false, true, nil)
	sideConfirmationFirst := func() bool {
		i.confirmDialog.mu.Lock()
		defer i.confirmDialog.mu.Unlock()
		return len(i.confirmDialog.pending) > 0 && i.confirmDialog.pending[0].toolName == "echo"
	}
	deadline := time.Now().Add(2 * time.Second)
	for !sideConfirmationFirst() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !sideConfirmationFirst() {
		t.Fatal("side-agent tool confirmation did not reach the front of the interactive confirmation queue")
	}
	if !i.confirmDialog.Focused() {
		t.Fatal("side-agent confirmation did not focus the prioritized dialog")
	}

	d.mu.Lock()
	if len(d.turns) != 1 || len(d.turns[0].Tools) != 1 {
		d.mu.Unlock()
		t.Fatalf("side-agent tool state = %#v, want one live tool", d.turns)
	}
	if got, want := d.turns[0].Tools[0].Args, "hello"; got != want {
		d.mu.Unlock()
		t.Fatalf("side-agent confirmation summary = %q, want %q", got, want)
	}
	if got, want := d.activity.label(), "Waiting for approval: echo"; got != want {
		d.mu.Unlock()
		t.Fatalf("side-agent activity = %q, want %q", got, want)
	}
	d.mu.Unlock()

	i.confirmDialog.HandleKey(tui.Key{Kind: tui.KeyEnter})
	select {
	case <-client.done:
	case <-time.After(2 * time.Second):
		t.Fatal("side-agent did not continue after confirmation")
	}
	if got := tool.runs.Load(); got != 1 {
		t.Fatalf("tool executions = %d, want 1 after confirmation", got)
	}
	if i.confirmDialog.Focused() {
		t.Fatal("main confirmation regained focus instead of returning input to /btw")
	}
	i.confirmDialog.CancelAll("test complete")
	select {
	case <-mainResp:
	default:
		t.Fatal("main confirmation was not retained after resolving the side-chat call")
	}
}
