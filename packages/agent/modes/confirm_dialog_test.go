package modes

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/tui"
)

func TestConfirmToolCallAttachesDiffBeforeDecision(t *testing.T) {
	dialog := newConfirmDialog()
	btw := &btwDialog{
		active:   true,
		toolView: &tui.View{Theme: tui.Dark},
		turns: []btwTurn{{Tools: []tui.ToolCallView{{
			ID: "call-1", Name: "edit",
		}}}},
		editor: tui.NewEditor(""),
	}
	i := &Interactive{
		toolCalls: map[string]*tui.ToolCallView{
			"call-1": {ID: "call-1", Name: "edit"},
		},
		btwDialog:     btw,
		confirmDialog: dialog,
		dirty:         make(chan struct{}, 1),
	}
	decision := make(chan core.ConfirmDecision, 1)
	go func() {
		decision <- i.ConfirmToolCall(core.ToolCallConfirmation{
			ID:      "call-1",
			Name:    "edit",
			Summary: "sample.go",
			Content: "-old\n+new\n",
			Origin:  btwOrigin(0),
		})
	}()

	deadline := time.Now().Add(time.Second)
	for !dialog.Active() && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if !dialog.Active() {
		t.Fatal("confirmation dialog did not open")
	}
	i.mu.Lock()
	mainPreview := i.toolCalls["call-1"].Preview
	i.mu.Unlock()
	if mainPreview != "" {
		t.Fatalf("main tool preview = %q, want side-chat origin to leave it untouched", mainPreview)
	}
	btw.mu.Lock()
	btwPreview := btw.turns[0].Tools[0].Preview
	btw.mu.Unlock()
	if btwPreview != "-old\n+new\n" {
		t.Fatalf("/btw tool preview = %q", btwPreview)
	}
	combined := strings.Join(renderBtwConfirmation(tui.Dark, 80, btw, dialog), "\n")
	for _, want := range []string{"btw - side chat", "old", "new", "confirm tool call"} {
		if !strings.Contains(combined, want) {
			t.Fatalf("combined /btw confirmation missing %q:\n%s", want, combined)
		}
	}

	dialog.HandleKey(tui.Key{Kind: tui.KeyEnter})
	select {
	case got := <-decision:
		if !got.Allow {
			t.Fatalf("confirmation decision = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("confirmation did not return")
	}
}

func TestConfirmToolCallRejectsClosedSideChatGeneration(t *testing.T) {
	i := newConfirmationInputInteractive()
	i.btwDialog.Open(tui.Dark, i.agent, i.agent.System, i.agent.Model, t.TempDir(), "", true, nil)
	i.btwDialog.mu.Lock()
	origin := btwOrigin(i.btwDialog.generation)
	i.btwDialog.mu.Unlock()
	i.btwDialog.Close()

	decision := i.ConfirmToolCall(core.ToolCallConfirmation{
		ID:     "call-1",
		Name:   "edit",
		Origin: origin,
	})
	if decision.Allow || decision.Reason != "side-chat tool call canceled" {
		t.Fatalf("stale side-chat decision = %+v", decision)
	}
	if i.confirmDialog.Active() {
		t.Fatal("stale side-chat confirmation activated the main dialog")
	}
}

func TestBtwCloseCancelsSideChatConfirmation(t *testing.T) {
	i := newConfirmationInputInteractive()
	d := i.btwDialog
	d.mu.Lock()
	d.active = true
	d.generation = 1
	d.turns = []btwTurn{{Tools: []tui.ToolCallView{{ID: "call-1", Name: "edit"}}}}
	d.mu.Unlock()

	decision := make(chan core.ConfirmDecision, 1)
	go func() {
		decision <- i.ConfirmToolCall(core.ToolCallConfirmation{
			ID:     "call-1",
			Name:   "edit",
			Origin: btwOrigin(1),
		})
	}()

	deadline := time.Now().Add(time.Second)
	for !i.confirmDialog.Active() && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if !i.confirmDialog.Active() {
		t.Fatal("side-chat confirmation did not enter the queue")
	}
	d.Close()

	select {
	case got := <-decision:
		if got.Allow || got.Reason != "side chat closed" {
			t.Fatalf("close decision = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("closing /btw did not unblock its confirmation")
	}
	if i.confirmDialog.Active() {
		t.Fatal("side-chat confirmation remained active after /btw closed")
	}
}

func newConfirmationInputInteractive() *Interactive {
	agent := core.NewAgent(nil, "test-model", "test system", nil)
	return NewInteractive(InteractiveConfig{
		Theme:    tui.Dark,
		Terminal: tui.NewProcTerm(),
		Agent:    agent,
		Sandbox:  tools.NewSandbox("."),
	})
}

func enqueueConfirmation(i *Interactive) <-chan core.ConfirmDecision {
	resp := make(chan core.ConfirmDecision, 1)
	i.confirmDialog.Enqueue(&confirmRequest{toolName: "bash", preview: "pwd", resp: resp})
	return resp
}

func TestConfirmDialogSlashOpensBtwBeforeDecision(t *testing.T) {
	i := newConfirmationInputInteractive()
	resp := enqueueConfirmation(i)

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: '/'})
	if got := i.ed.Value(); got != "/" {
		t.Fatalf("editor = %q, want slash command input", got)
	}
	if i.confirmDialog.Focused() {
		t.Fatal("confirmation retained focus after slash")
	}

	i.ed.SetValue("/btw")
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
	if !i.btwDialog.Active() {
		t.Fatal("/btw did not open while confirmation was pending")
	}
	if i.confirmDialog.Focused() {
		t.Fatal("confirmation stole focus from /btw")
	}
	select {
	case decision := <-resp:
		t.Fatalf("/btw unexpectedly answered confirmation: %+v", decision)
	default:
	}

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: 'x'})
	i.btwDialog.mu.Lock()
	btwInput := i.btwDialog.editor.Value()
	i.btwDialog.mu.Unlock()
	if btwInput != "x" {
		t.Fatalf("/btw editor = %q, want %q", btwInput, "x")
	}

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc})
	if i.btwDialog.Active() {
		t.Fatal("esc did not close /btw")
	}
	if !i.confirmDialog.Focused() {
		t.Fatal("closing /btw did not restore confirmation focus")
	}
}

func TestConfirmDialogPrioritizesSideChatToolAndReturnsToSideChat(t *testing.T) {
	dialog := newConfirmDialog()
	mainResp := make(chan core.ConfirmDecision, 1)
	sideResp := make(chan core.ConfirmDecision, 1)
	dialog.Enqueue(&confirmRequest{toolName: "main", resp: mainResp})
	dialog.Blur()
	dialog.Enqueue(&confirmRequest{toolName: "side", resp: sideResp, returnToChild: true})

	dialog.mu.Lock()
	first := dialog.pending[0].toolName
	dialog.mu.Unlock()
	if first != "side" {
		t.Fatalf("first confirmation = %q, want side-chat tool", first)
	}
	if !dialog.Focused() {
		t.Fatal("side-chat tool confirmation did not take focus")
	}

	dialog.HandleKey(tui.Key{Kind: tui.KeyEnter})
	select {
	case decision := <-sideResp:
		if !decision.Allow {
			t.Fatalf("side-chat decision = %+v", decision)
		}
	default:
		t.Fatal("side-chat confirmation was not resolved")
	}
	if !dialog.Active() {
		t.Fatal("main confirmation was lost")
	}
	if dialog.Focused() {
		t.Fatal("main confirmation stole focus after side-chat tool decision")
	}
	select {
	case decision := <-mainResp:
		t.Fatalf("main confirmation was unexpectedly resolved: %+v", decision)
	default:
	}
}

func TestConfirmDialogEscCancelsSlashInputOnly(t *testing.T) {
	i := newConfirmationInputInteractive()
	resp := enqueueConfirmation(i)

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: '/'})
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: 'h'})
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc})

	if !i.ed.IsEmpty() {
		t.Fatalf("editor was not cleared: %q", i.ed.Value())
	}
	if !i.confirmDialog.Focused() {
		t.Fatal("esc did not return focus to confirmation")
	}
	select {
	case decision := <-resp:
		t.Fatalf("esc from slash input answered confirmation: %+v", decision)
	default:
	}
}

func TestConfirmDialogHelpEscDismissesHelpOnly(t *testing.T) {
	i := newConfirmationInputInteractive()
	resp := enqueueConfirmation(i)

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: '/'})
	i.ed.SetValue("/help")
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})

	if len(i.helpBlock) == 0 {
		t.Fatal("/help did not open while confirmation was pending")
	}
	if i.confirmDialog.Focused() {
		t.Fatal("confirmation stole focus from /help")
	}

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc})

	if len(i.helpBlock) != 0 {
		t.Fatal("esc did not dismiss /help")
	}
	if !i.confirmDialog.Focused() {
		t.Fatal("dismissing /help did not restore confirmation focus")
	}
	select {
	case decision := <-resp:
		t.Fatalf("esc from /help answered confirmation: %+v", decision)
	default:
	}
}

func TestConfirmDialogExtensionNoteEscDismissesNoteOnly(t *testing.T) {
	i := newConfirmationInputInteractive()
	resp := enqueueConfirmation(i)

	i.Display("example", "command output")
	if len(i.extNotes) == 0 {
		t.Fatal("extension note was not displayed")
	}
	if i.confirmDialog.Focused() {
		t.Fatal("confirmation retained focus over extension note")
	}

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc})

	if len(i.extNotes) != 0 {
		t.Fatal("esc did not dismiss extension note")
	}
	if !i.confirmDialog.Focused() {
		t.Fatal("dismissing extension note did not restore confirmation focus")
	}
	select {
	case decision := <-resp:
		t.Fatalf("esc from extension note answered confirmation: %+v", decision)
	default:
	}
}

func TestConfirmDialogExtensionPanelTakesAndReturnsFocus(t *testing.T) {
	i := newConfirmationInputInteractive()
	resp := enqueueConfirmation(i)

	i.OpenPanel("todo", extproto.PanelSpec{ID: "todos", Title: "Todos", Lines: []string{"one"}})
	if !i.extPanel.Active() {
		t.Fatal("extension panel did not open")
	}
	if i.confirmDialog.Focused() {
		t.Fatal("confirmation retained focus over extension panel")
	}

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc})
	if i.extPanel.Active() {
		t.Fatal("esc did not close extension panel")
	}
	if !i.confirmDialog.Focused() {
		t.Fatal("closing extension panel did not restore confirmation focus")
	}
	select {
	case decision := <-resp:
		t.Fatalf("extension panel answered confirmation: %+v", decision)
	default:
	}
}

func TestCancelAndWaitDrainsPendingConfirmation(t *testing.T) {
	i := newConfirmationInputInteractive()
	resp := enqueueConfirmation(i)
	i.busy = true
	i.cancelTurn = func() {
		i.mu.Lock()
		i.busy = false
		i.mu.Unlock()
	}

	i.cancelAndWaitForIdle()

	if i.confirmDialog.Active() {
		t.Fatal("confirmation remained active after turn cancellation")
	}
	select {
	case decision := <-resp:
		if decision.Allow || decision.Reason != "turn cancelled" {
			t.Fatalf("cancellation decision = %+v", decision)
		}
	default:
		t.Fatal("pending confirmation was not resolved")
	}
}

func TestConfirmDialogAllowsToolExpansion(t *testing.T) {
	resp := make(chan core.ConfirmDecision, 1)
	dialog := newConfirmDialog()
	dialog.Enqueue(&confirmRequest{
		toolName: "edit",
		preview:  "large edit",
		resp:     resp,
	})
	// Live write/edit bodies use a fixed tail window; a long bash
	// command is the live body that still collapses behind ctrl+o.
	args, err := json.Marshal(map[string]any{
		"command": strings.Repeat("echo value\n", 20),
	})
	if err != nil {
		t.Fatal(err)
	}
	view := &tui.View{
		ToolCalls: []tui.ToolCallView{{
			ID:         "call-1",
			Name:       "bash",
			RawJSONBuf: string(args),
		}},
	}
	i := &Interactive{
		view:          view,
		confirmDialog: dialog,
		dirty:         make(chan struct{}, 1),
	}
	if rendered := strings.Join(view.Build(80), "\n"); !strings.Contains(rendered, "ctrl+o to expand") {
		t.Fatalf("test preview was not initially collapsed:\n%s", rendered)
	}

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlO})

	if !i.view.ExpandAll {
		t.Fatal("ctrl+o did not expand tool previews while confirmation was active")
	}
	if rendered := strings.Join(view.Build(80), "\n"); strings.Contains(rendered, "ctrl+o to expand") {
		t.Fatalf("live bash preview remained collapsed after ctrl+o:\n%s", rendered)
	}
	if !dialog.Active() {
		t.Fatal("ctrl+o closed the confirmation dialog")
	}
	select {
	case decision := <-resp:
		t.Fatalf("ctrl+o unexpectedly answered confirmation: %+v", decision)
	default:
	}

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlO})
	if i.view.ExpandAll {
		t.Fatal("second ctrl+o did not collapse tool previews during confirmation")
	}
	if rendered := strings.Join(view.Build(80), "\n"); !strings.Contains(rendered, "ctrl+o to expand") {
		t.Fatalf("live edit preview remained expanded after second ctrl+o:\n%s", rendered)
	}
	if !dialog.Active() {
		t.Fatal("second ctrl+o closed the confirmation dialog")
	}
}
