package modes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

// btwTurn is one user/assistant pair within a side chat. Kept
// separate from the main transcript so closing the dialog leaves
// the main session untouched.
type btwTurn struct {
	User      string
	Assistant string
	Tools     []tui.ToolCallView
	Err       string
}

// btwDialog is the side-chat overlay opened by /btw. It shows the
// user's question, runs an isolated agent turn against the live
// snapshot of the main session plus any prior side-chat turns,
// renders the assistant reply through the markdown pipeline, and
// keeps the main transcript completely untouched.
//
// Cancellation: esc cancels an in-flight call when one is running,
// otherwise closes the dialog.
type btwDialog struct {
	mu          sync.Mutex
	active      bool
	turns       []btwTurn
	editor      *tui.Editor
	loading     bool
	compactMode bool
	lineInput   bool
	cancel      context.CancelFunc
	generation  uint64
	closeHook   func()
	// pendingHook shows a forked session's confirmation or question; the
	// answer callbacks route through the fork's port. resolvedHook removes
	// a request resolved elsewhere. Both receive dialog-local request IDs.
	pendingHook  func(localID string, p session.PendingRequest, confirm func(core.ConfirmDecision), answer func([]tools.QuestionAnswer))
	resolvedHook func(localID string)

	// spin drives the same braille animation as the main status bar. Its
	// activity label is owned by this dialog under d.mu.
	spin     *spinner
	activity agentActivity

	// sideAgent owns an isolated transcript seeded from the main
	// conversation. It uses the same tools and runtime hooks as the
	// main agent, but has no persistence callbacks.
	sideAgent *core.Agent

	// Theme and toolView are cached so render() uses the same tool-call
	// presentation and collapse behavior as the main transcript.
	theme    tui.Theme
	toolView *tui.View

	// cwd is the working directory used to resolve relative paths
	// when the user presses Tab on a path-like token in the side-
	// chat editor. Set by Open() from the host's cwd so the same
	// path-completion that works in the main editor also works
	// here.
	cwd string

	// remote is set when the side chat runs on a forked daemon session
	// (port mode) instead of a local side agent.
	remote *btwRemote
	// note is a dialog-level message such as a failed fork creation.
	note string
}

// btwRemote is the daemon-backed side chat state. The fork is created,
// attached and subscribed off the render loop; port is nil until ready.
type btwRemote struct {
	client  SessionClient
	timeout time.Duration
	id      session.ID
	port    session.Port
	stop    context.CancelFunc
	turn    int
	// pending holds the dialog-local IDs of the fork's open requests.
	pending    map[string]bool
	invalidate func()
}

// errBtwStale rejects an answer for a side session that was closed or
// replaced since the request was shown.
var errBtwStale = errors.New("side session closed")

// btwRemoteCloseReason is sent to the daemon when the side chat releases
// its forked session.
const btwRemoteCloseReason = "btw_closed"

func newBtwDialog() *btwDialog {
	return &btwDialog{}
}

func btwOrigin(generation uint64) string {
	return fmt.Sprintf("btw:%d", generation)
}

func isBtwOrigin(origin string) bool {
	return strings.HasPrefix(origin, "btw:")
}

func (d *btwDialog) setCloseHook(hook func()) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.closeHook = hook
	d.mu.Unlock()
}

// setPendingHooks installs the host callbacks that display and retract the
// forked session's pending requests.
func (d *btwDialog) setPendingHooks(pending func(string, session.PendingRequest, func(core.ConfirmDecision), func([]tools.QuestionAnswer)), resolved func(string)) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.pendingHook, d.resolvedHook = pending, resolved
	d.mu.Unlock()
}

func btwPendingID(generation uint64, id string) string {
	return btwOrigin(generation) + ":" + id
}

// liveRemoteLocked reports whether rem is still the side session of
// generation. Callers hold d.mu.
func (d *btwDialog) liveRemoteLocked(rem *btwRemote, generation uint64) bool {
	return d.active && d.generation == generation && d.remote == rem && rem.port != nil
}

// answerRemote calls fn on the fork's port only while rem is still the live
// side session of generation; a closed or replaced fork rejects the answer.
func (d *btwDialog) answerRemote(rem *btwRemote, generation uint64, id string, fn func(context.Context, session.Port) error) error {
	d.mu.Lock()
	if !d.liveRemoteLocked(rem, generation) {
		d.mu.Unlock()
		return errBtwStale
	}
	port, timeout := rem.port, rem.timeout
	delete(rem.pending, btwPendingID(generation, id))
	d.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := fn(ctx, port)
	if err != nil && !errors.Is(err, errBtwStale) {
		d.mu.Lock()
		if d.liveRemoteLocked(rem, generation) {
			if rem.pending == nil {
				rem.pending = map[string]bool{}
			}
			rem.pending[btwPendingID(generation, id)] = true
			d.note = "side session: " + err.Error()
		}
		d.mu.Unlock()
		if rem.invalidate != nil {
			rem.invalidate()
		}
	}
	return err
}

// handleRemotePending shows or retracts one of the fork's pending requests.
func (d *btwDialog) handleRemotePending(rem *btwRemote, generation uint64, ev session.Event) {
	d.mu.Lock()
	if !d.liveRemoteLocked(rem, generation) {
		d.mu.Unlock()
		return
	}
	pendingHook, resolvedHook := d.pendingHook, d.resolvedHook
	switch data := ev.Data.(type) {
	case session.PendingEvent:
		p := data.Pending
		local := btwPendingID(generation, p.ID)
		if p.ID == "" || rem.pending[local] || pendingHook == nil {
			d.mu.Unlock()
			return
		}
		if rem.pending == nil {
			rem.pending = map[string]bool{}
		}
		rem.pending[local] = true
		if p.Kind == "confirm" && p.Confirm != nil {
			if turnIdx := rem.turn; turnIdx >= 0 && turnIdx < len(d.turns) {
				if tool := findBtwTool(&d.turns[turnIdx], p.Confirm.ID); tool != nil {
					tool.Args = p.Confirm.Summary
					tool.Preview = p.Confirm.Content
				}
			}
			d.activity.activity = activity{kind: activityAwaitingConfirmation, tool: p.Confirm.Name}
		}
		d.mu.Unlock()
		id := p.ID
		pendingHook(local, p, func(dec core.ConfirmDecision) {
			_ = d.answerRemote(rem, generation, id, func(ctx context.Context, port session.Port) error {
				return port.AnswerConfirmation(ctx, id, dec)
			})
		}, func(answers []tools.QuestionAnswer) {
			_ = d.answerRemote(rem, generation, id, func(ctx context.Context, port session.Port) error {
				return port.AnswerQuestion(ctx, id, answers)
			})
		})
	case session.PendingResolvedEvent:
		local := btwPendingID(generation, data.ID)
		known := rem.pending[local]
		delete(rem.pending, local)
		d.mu.Unlock()
		if known && resolvedHook != nil {
			resolvedHook(local)
		}
	default:
		d.mu.Unlock()
	}
}

// Active reports whether the dialog is visible and consuming keys.
func (d *btwDialog) Active() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.active
}

// Loading reports whether the dialog is currently awaiting a
// model response (and therefore rendering an animated spinner).
// Used by the host to decide whether a periodic redraw is worth
// triggering; when false and the user is just typing, we can
// skip the tick and let the terminal drive the cursor blink.
func (d *btwDialog) Loading() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.active && d.loading
}

// enqueueToolConfirmation attaches the side-effect-free confirmation preview
// to the matching side-chat tool box and queues its prompt while d.mu is
// held. Keeping both actions together prevents Close from advancing the
// generation between the ownership check and the confirmation enqueue.
func (d *btwDialog) enqueueToolConfirmation(origin, id, summary, content string, enqueue func()) bool {
	if d == nil || id == "" || enqueue == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.active || origin != btwOrigin(d.generation) {
		return false
	}
	for turnIdx := range d.turns {
		if tool := findBtwTool(&d.turns[turnIdx], id); tool != nil {
			tool.Args = summary
			tool.Preview = content
			d.activity.activity = activity{kind: activityAwaitingConfirmation, tool: tool.Name}
			enqueue()
			return true
		}
	}
	return false
}

// ToggleToolExpansion expands or collapses long tool results in the side chat.
func (d *btwDialog) ToggleToolExpansion() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.toolView != nil {
		d.toolView.ExpandAll = !d.toolView.ExpandAll
	}
}

func (d *btwDialog) setTheme(theme tui.Theme) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.theme = theme
	if d.toolView != nil {
		d.toolView.Theme = theme
		d.toolView.InvalidateRenderCache()
	}
	if d.editor != nil {
		d.editor.Prompt = theme.AccentBar(theme.Accent)
	}
	if d.spin != nil {
		d.spin.Configure(theme)
	}
}

// Open enters the side chat. agent supplies the live transcript and
// system prompt, plus the underlying provider client to use for the
// one-off completion. seed is an optional first question that gets
// auto-submitted (so /btw <text> starts a conversation right away).
// invalidate, if non-nil, is called after each state change so the
// host redraw loop can pick up the update without polling.
func (d *btwDialog) Open(th tui.Theme, agent *core.Agent, system, model, cwd, seed string, compactMode, flatTools bool, lineInput bool, invalidate func()) {
	d.mu.Lock()
	d.generation++
	d.active = true
	d.theme = th
	d.compactMode = compactMode
	d.lineInput = lineInput
	d.toolView = &tui.View{Theme: th, CompactMode: compactMode, FlatTools: flatTools}
	d.turns = nil
	d.loading = false
	d.cancel = nil
	d.activity = agentActivity{}
	prompt := th.AccentBar(th.Accent)
	if lineInput {
		prompt = ""
	}
	d.editor = tui.NewEditor(prompt)
	d.sideAgent = newBtwAgent(agent, system, model, btwOrigin(d.generation))
	d.cwd = cwd
	d.remote = nil
	d.note = ""
	d.mu.Unlock()

	if seed = strings.TrimSpace(seed); seed != "" {
		d.editor.SetValue(seed)
		d.submit(invalidate)
	}
}

// OpenPort enters the side chat backed by a forked daemon session. The fork
// (spec) is created, attached and subscribed in a goroutine so the render
// loop never waits on the daemon. A seed is submitted once the fork is ready.
func (d *btwDialog) OpenPort(th tui.Theme, client SessionClient, spec session.CreateSpec, timeout time.Duration, cwd, seed string, compactMode, flatTools, lineInput bool, invalidate func()) {
	d.mu.Lock()
	d.generation++
	generation := d.generation
	d.active = true
	d.theme = th
	d.compactMode = compactMode
	d.lineInput = lineInput
	d.toolView = &tui.View{Theme: th, CompactMode: compactMode, FlatTools: flatTools}
	d.turns = nil
	d.loading = false
	d.cancel = nil
	d.activity = agentActivity{}
	prompt := th.AccentBar(th.Accent)
	if lineInput {
		prompt = ""
	}
	d.editor = tui.NewEditor(prompt)
	d.sideAgent = nil
	d.cwd = cwd
	d.note = "starting side session…"
	rem := &btwRemote{client: client, timeout: timeout, turn: -1, invalidate: invalidate}
	d.remote = rem
	seed = strings.TrimSpace(seed)
	if seed != "" {
		d.editor.SetValue(seed)
	}
	d.mu.Unlock()

	go d.startRemote(rem, generation, spec, seed != "", invalidate)
}

func (d *btwDialog) startRemote(rem *btwRemote, generation uint64, spec session.CreateSpec, submitSeed bool, invalidate func()) {
	fail := func(err error) {
		d.mu.Lock()
		if d.active && d.generation == generation && d.remote == rem {
			d.note = "side session: " + err.Error()
		}
		d.mu.Unlock()
		if invalidate != nil {
			invalidate()
		}
	}
	callCtx, cancel := context.WithTimeout(context.Background(), rem.timeout)
	info, err := rem.client.Create(callCtx, spec)
	cancel()
	if err != nil {
		fail(err)
		return
	}
	id := session.ID(info.ID)
	release := func() {
		ctx, cancel := context.WithTimeout(context.Background(), rem.timeout)
		defer cancel()
		_ = rem.client.Close(ctx, id, btwRemoteCloseReason)
	}
	callCtx, cancel = context.WithTimeout(context.Background(), rem.timeout)
	port, err := rem.client.Attach(callCtx, id)
	cancel()
	if err != nil {
		release()
		fail(err)
		return
	}
	subCtx, stop := context.WithCancel(context.Background())
	events, err := port.Subscribe(subCtx, 0)
	if err != nil {
		stop()
		release()
		fail(err)
		return
	}
	d.mu.Lock()
	if !d.active || d.generation != generation || d.remote != rem {
		d.mu.Unlock()
		stop()
		release()
		return
	}
	rem.id = id
	rem.port = port
	rem.stop = stop
	d.note = ""
	d.mu.Unlock()
	go d.pumpRemote(rem, generation, events, invalidate)
	if invalidate != nil {
		invalidate()
	}
	if submitSeed {
		d.submit(invalidate)
	}
}

// pumpRemote routes the forked session's agent events to the current turn.
func (d *btwDialog) pumpRemote(rem *btwRemote, generation uint64, events <-chan session.Event, invalidate func()) {
	for ev := range events {
		if ev.Kind == session.EvPending || ev.Kind == session.EvPendingResolved {
			d.handleRemotePending(rem, generation, ev)
			if invalidate != nil {
				invalidate()
			}
			continue
		}
		if ev.Kind != session.EvAgent || ev.Agent == nil {
			continue
		}
		d.mu.Lock()
		turnIdx := rem.turn
		live := d.active && d.generation == generation && d.remote == rem
		d.mu.Unlock()
		if !live {
			return
		}
		if turnIdx < 0 {
			continue
		}
		switch e := ev.Agent.(type) {
		case core.EvDone:
			d.completeTurn(turnIdx, generation, "")
		case core.EvError:
			msg := "turn failed"
			if e.Err != nil {
				msg = e.Err.Error()
			}
			d.completeTurn(turnIdx, generation, msg)
		default:
			d.handleAgentEvent(turnIdx, generation, ev.Agent)
		}
		if invalidate != nil {
			invalidate()
		}
	}
}

// Close hides the dialog. Cancels any in-flight request.
func (d *btwDialog) Close() {
	d.mu.Lock()
	d.generation++
	d.active = false
	d.turns = nil
	d.editor = nil
	d.loading = false
	d.activity = agentActivity{}
	cancel := d.cancel
	d.cancel = nil
	d.sideAgent = nil
	d.toolView = nil
	rem := d.remote
	d.remote = nil
	d.note = ""
	closeHook, resolvedHook := d.closeHook, d.resolvedHook
	var retract []string
	if rem != nil {
		for id := range rem.pending {
			retract = append(retract, id)
		}
		rem.pending = nil
	}
	d.mu.Unlock()
	if resolvedHook != nil {
		for _, id := range retract {
			resolvedHook(id)
		}
	}
	if cancel != nil {
		cancel()
	}
	if rem != nil && rem.port != nil {
		if rem.stop != nil {
			rem.stop()
		}
		// Release the forked daemon session off the caller's goroutine.
		go func(client SessionClient, id session.ID, timeout time.Duration) {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			_ = client.Close(ctx, id, btwRemoteCloseReason)
		}(rem.client, rem.id, rem.timeout)
	}
	if closeHook != nil {
		closeHook()
	}
}

// HandleKey routes a keypress to the dialog. Returns true if the
// dialog wants the event consumed (always true while active, except
// for the special closing case where the caller may want to signal
// the parent).
func (d *btwDialog) HandleKey(k tui.Key, invalidate func()) (closed bool) {
	if !d.Active() {
		return false
	}
	switch k.Kind {
	case tui.KeyEsc:
		// First esc: cancel an in-flight call. Subsequent esc closes.
		d.mu.Lock()
		busy := d.loading
		cancel := d.cancel
		d.mu.Unlock()
		if busy && cancel != nil {
			cancel()
			return false
		}
		d.Close()
		invalidate()
		return true
	}

	d.mu.Lock()
	editor := d.editor
	loading := d.loading
	cwd := d.cwd
	d.mu.Unlock()
	if editor == nil {
		return false
	}
	// Tab-complete a path-like token before the editor sees the key,
	// matching the main editor's behaviour.
	if k.Kind == tui.KeyTab {
		if tryPathTabCompleteEditor(editor, cwd) {
			invalidate()
			return false
		}
	}
	// Don't accept new submissions while a call is in flight; arrow
	// keys / scrolling still flow through to the editor for caret
	// movement and history.
	submitted := editor.HandleKey(k)
	invalidate()
	if submitted && !loading {
		d.submit(invalidate)
	}
	return false
}

// submit fires the LLM call for the current input and, on success,
// appends a new turn to d.turns. invalidate is called every time
// the turn's visible state changes (text delta, error, complete)
// so the host redraw loop picks up the update without relying on
// a periodic tick.
func (d *btwDialog) submit(invalidate func()) {
	d.mu.Lock()
	if d.editor == nil || d.loading {
		d.mu.Unlock()
		return
	}
	question := strings.TrimSpace(d.editor.Value())
	if question == "" {
		d.mu.Unlock()
		return
	}
	if rem := d.remote; rem != nil {
		d.submitRemoteLocked(rem, question, invalidate)
		return
	}
	d.editor.Clear()
	d.loading = true
	if d.spin == nil {
		d.spin = newSpinner(d.theme)
	} else {
		d.spin.Configure(d.theme)
	}
	d.spin.Start()
	d.turns = append(d.turns, btwTurn{User: question})
	turnIdx := len(d.turns) - 1
	generation := d.generation

	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	agent := d.sideAgent
	providerName := ""
	model := ""
	if agent != nil {
		model = agent.Model
		if agent.Client != nil {
			providerName = agent.Client.Name()
		}
	}
	d.activity = newAgentActivity(providerName, model)
	d.mu.Unlock()

	go func() {
		err := agent.Prompt(ctx, question, nil, func(ev core.AgentEvent) {
			d.handleAgentEvent(turnIdx, generation, ev)
			if invalidate != nil {
				invalidate()
			}
		})
		errMsg := ""
		if err != nil && ctx.Err() == nil {
			errMsg = err.Error()
		}
		d.completeTurn(turnIdx, generation, errMsg)
		if invalidate != nil {
			invalidate()
		}
	}()
}

// submitRemoteLocked sends question to the forked daemon session. Called
// with d.mu held; it unlocks d.mu. Replies arrive through pumpRemote.
func (d *btwDialog) submitRemoteLocked(rem *btwRemote, question string, invalidate func()) {
	port := rem.port
	if port == nil {
		// Keep the text in the editor until the fork is ready.
		d.mu.Unlock()
		return
	}
	d.editor.Clear()
	d.loading = true
	if d.spin == nil {
		d.spin = newSpinner(d.theme)
	} else {
		d.spin.Configure(d.theme)
	}
	d.spin.Start()
	d.turns = append(d.turns, btwTurn{User: question})
	turnIdx := len(d.turns) - 1
	rem.turn = turnIdx
	generation := d.generation
	timeout := rem.timeout
	d.cancel = func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			_ = port.Abort(ctx)
		}()
	}
	d.activity = newAgentActivity("", "")
	d.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		err := port.Submit(ctx, session.Input{Text: question, Mode: session.SubmitModeAuto, Origin: session.InputOriginUser})
		if err != nil {
			d.completeTurn(turnIdx, generation, err.Error())
		}
		if invalidate != nil {
			invalidate()
		}
	}()
}

func newBtwAgent(main *core.Agent, system, model, origin string) *core.Agent {
	agent := core.NewAgent(main.Client, model, system, main.ToolsSnapshot())
	agent.MaxSteps = main.MaxSteps
	agent.DisableRepetitionGuard = main.DisableRepetitionGuard
	agent.Reasoning = main.Reasoning
	agent.Temperature = main.Temperature
	agent.FastMode = main.FastModeEnabled()
	agent.MaxTokens = main.MaxTokens
	if beforeToolExecute := main.BeforeToolExecute; beforeToolExecute != nil {
		agent.BeforeToolExecute = func(call provider.ToolCallBlock) (bool, string, json.RawMessage) {
			call.Origin = origin
			return beforeToolExecute(call)
		}
	}
	agent.BeforeTurn = main.BeforeTurn
	agent.BeforeAssistantMessage = main.BeforeAssistantMessage
	agent.MaxRetries = main.MaxRetries
	agent.RetryBaseDelay = main.RetryBaseDelay
	agent.OnEvent = main.OnEvent
	agent.SetMessages(main.Messages())
	// The side agent shares the main registry (which contains plan) and the main
	// transcript, so it must also carry the main plan: otherwise `show` answers
	// empty and an indexed update fails on a checklist the transcript displays.
	agent.SetPlan(main.CurrentPlan())
	return agent
}

func (d *btwDialog) handleAgentEvent(turnIdx int, generation uint64, ev core.AgentEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.active || generation != d.generation || turnIdx < 0 || turnIdx >= len(d.turns) {
		return
	}
	turn := &d.turns[turnIdx]
	d.activity.apply(ev)
	switch e := ev.(type) {
	case core.EvTextDelta:
		turn.Assistant += e.Delta
	case core.EvToolUseStart:
		turn.Tools = append(turn.Tools, tui.ToolCallView{ID: e.ID, Name: e.Name, Streaming: true})
	case core.EvToolUseArgs:
		if tool := findBtwTool(turn, e.ID); tool != nil {
			tool.RawJSONBuf += e.Delta
			if path, ok, _ := tui.ExtractPartialStringField(tool.RawJSONBuf, "path"); ok {
				tool.LivePath = path
			} else if path, ok, _ := tui.ExtractPartialStringField(tool.RawJSONBuf, "file_path"); ok {
				tool.LivePath = path
			}
		}
	case core.EvToolUseEnd:
		if tool := findBtwTool(turn, e.ID); tool != nil {
			tool.Streaming = false
		}
	case core.EvToolCall:
		tool := findBtwTool(turn, e.ID)
		if tool == nil {
			turn.Tools = append(turn.Tools, tui.ToolCallView{ID: e.ID, Name: e.Name})
			tool = &turn.Tools[len(turn.Tools)-1]
		}
		tool.Name = e.Name
		tool.Args = tui.ShortArgs(e.Name, e.Args)
		if tool.RawJSONBuf == "" {
			tool.RawJSONBuf = string(e.Args)
		}
		tool.Streaming = false
	case core.EvToolResult:
		if tool := findBtwTool(turn, e.ID); tool != nil {
			tool.Done = true
			tool.Error = e.Result.IsError
			var text strings.Builder
			for _, content := range e.Result.Content {
				if block, ok := content.(provider.TextBlock); ok {
					if text.Len() > 0 {
						text.WriteString("\n")
					}
					text.WriteString(block.Text)
				}
			}
			tool.Result = text.String()
		}
	}
}

func findBtwTool(turn *btwTurn, id string) *tui.ToolCallView {
	for j := range turn.Tools {
		if turn.Tools[j].ID == id {
			return &turn.Tools[j]
		}
	}
	return nil
}

// completeTurn records an error, if any, and clears the loading state.
func (d *btwDialog) completeTurn(idx int, generation uint64, errMsg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.active || generation != d.generation || idx < 0 || idx >= len(d.turns) {
		return
	}
	d.turns[idx].Err = errMsg
	d.loading = false
	d.activity = agentActivity{}
	d.cancel = nil
}

func renderBtwConfirmation(th tui.Theme, width int, btw *btwDialog, confirm *confirmDialog) []string {
	rows := padDialogFrame(btw.Render(th, width))
	rows = append(rows, "")
	return append(rows, padDialogFrame(confirm.Render(th, width))...)
}

// Render returns the side-chat panel lines. Called every frame
// while active.
func (d *btwDialog) Render(th tui.Theme, width int) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.active {
		return nil
	}

	var out []string
	out = append(out, frameHeaderColor(th, "btw - side chat (esc closes; nothing is added to the main thread)", width, th.Accent))

	if len(d.turns) == 0 && !d.loading {
		out = append(out, "  "+th.FGColor(th.Muted, "ask anything; replies stay private to this side chat."))
	}
	if d.note != "" {
		out = append(out, wrapDialogTextRows(th.FGColor(th.Muted, "  "+d.note), width)...)
	}

	for _, t := range d.turns {
		out = append(out, "")
		out = append(out, btwUserBubbleRows(th, t.User, width-2, d.compactMode)...)
		if len(t.Tools) > 0 {
			// Match the main transcript's inter-message separator between
			// the user bubble and the first tool box.
			out = append(out, "")
		}
		for _, tool := range t.Tools {
			out = append(out, d.toolView.RenderToolCall(tool, width)...)
		}
		if t.Assistant != "" {
			out = append(out, "")
			out = append(out, renderDialogMarkdownRows(t.Assistant, th, width)...)
		}
		if t.Err != "" {
			out = append(out, wrapDialogTextRows(th.FGColor(th.Error, "✖ "+t.Err), width)...)
		}
	}

	if d.loading && d.spin != nil {
		out = append(out, "")
		label := d.activity.label()
		if label == "" {
			label = "Preparing request"
		}
		prefix := fmt.Sprintf("%s %s - %s",
			th.FGColor(th.Assistant, d.spin.Frame()),
			th.FGColor(th.Assistant, label),
			th.FGColor(th.Muted, d.spin.Elapsed().String()),
		)
		out = append(out, "  "+prefix+"  "+th.FGColor(th.Muted, "(esc cancels)"))
	}

	out = append(out, "")
	if d.editor != nil {
		// Render at width-2 to match the two-cell left indent applied
		// below. CursorPos uses the same width so the reported cursor
		// column matches the wrapped layout shown on screen.
		d.editor.Prompt = th.AccentBar(th.Accent)
		if d.lineInput {
			d.editor.Prompt = ""
		}
		edLines, _, _ := d.editor.Render(width - 2)
		for _, l := range edLines {
			// Indent the editor body so it lines up with the side-chat
			// content column. Editor's prompt already includes a left
			// marker, so just two cells of pad.
			out = append(out, "  "+l)
		}
		out = append(out, "") // breathing room between editor and frame rule
	}
	out = append(out, frameRuleColor(th, width, th.Accent))
	return out
}

// CursorRow / CursorCol report where the dialog wants the terminal
// cursor placed within its render output, so the parent can position
// the actual terminal cursor on the editor input. Returns (-1, -1)
// when the dialog isn't active or has no editor.
func (d *btwDialog) CursorPos(width int) (row, col int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.active || d.editor == nil {
		return -1, -1
	}
	// Reproduce render's structure to find where the editor sits.
	// Note: the parent (interactive.go) wraps every dialog with
	// padDialogFrame. It only injects a blank row after the frame
	// header when Render did not already put one there. With existing
	// turns or a loading spinner, Render's first body row is already
	// blank, so counting an extra pad row would place the cursor one
	// row too low.
	editorOffset := 1 // header
	if len(d.turns) == 0 && !d.loading {
		editorOffset++ // padDialogFrame's post-header blank
		editorOffset++ // muted "ask anything..." line
	}
	if d.note != "" {
		editorOffset += len(wrapDialogTextRows(d.theme.FGColor(d.theme.Muted, "  "+d.note), width))
	}
	for _, t := range d.turns {
		editorOffset++ // blank
		editorOffset += len(btwUserBubbleRows(d.theme, t.User, width-2, d.compactMode))
		if len(t.Tools) > 0 {
			editorOffset++ // main transcript's user-to-tool separator
		}
		for _, tool := range t.Tools {
			editorOffset += len(d.toolView.RenderToolCall(tool, width))
		}
		if t.Assistant != "" {
			editorOffset++ // blank
			editorOffset += len(renderDialogMarkdownRows(t.Assistant, d.theme, width))
		}
		if t.Err != "" {
			editorOffset += len(wrapDialogTextRows(d.theme.FGColor(d.theme.Error, "✖ "+t.Err), width))
		}
	}
	if d.loading {
		editorOffset++ // blank
		editorOffset++ // spinner line
	}
	editorOffset++ // pre-editor blank
	d.editor.Prompt = d.theme.AccentBar(d.theme.Accent)
	if d.lineInput {
		d.editor.Prompt = ""
	}
	_, eRow, eCol := d.editor.Render(width - 2)
	return editorOffset + eRow, eCol + 2 /* matches render indent */
}

// btwUserBubbleRows renders a user message inside the /btw dialog
// using the same bubble layout the main chat uses (full-width tinted
// panel, left-edge ▌ bar, padding rows above and below). The frame
// padding is the caller's job; bubbleWidth is the available row
// width inside the frame.
func btwUserBubbleRows(th tui.Theme, text string, bubbleWidth int, compactMode bool) []string {
	const leftGutter = 0
	const rightGutter = 2
	innerWidth := bubbleWidth - 2 - leftGutter - rightGutter // 2 = bar's two cells
	if innerWidth < 1 {
		innerWidth = 1
	}
	bar := th.BG(th.UserBubbleBG, th.FGColor(th.Accent, "▌ "))
	row := func(content string) string {
		inner := strings.Repeat(" ", leftGutter) + content
		return "  " + bar + th.UserBubble(inner, bubbleWidth-2)
	}
	if compactMode {
		innerWidth = bubbleWidth - 2
		if innerWidth < 1 {
			innerWidth = 1
		}
		row = func(content string) string {
			return "  " + th.FGColor(th.Accent, "▌ ") + th.FGColor(th.Muted, content)
		}
	}
	var bubble []string
	for _, l := range strings.Split(text, "\n") {
		for _, w := range tui.WrapANSILine(l, innerWidth) {
			bubble = append(bubble, row(w))
		}
	}
	if len(bubble) == 0 {
		return nil
	}
	if compactMode {
		return bubble
	}
	out := make([]string, 0, len(bubble)+2)
	out = append(out, row(""))
	out = append(out, bubble...)
	out = append(out, row(""))
	return out
}

// showBtwPending displays a forked /btw session's confirmation or question.
// Answers go to the fork's port through confirm/answer, never the main port.
func (i *Interactive) showBtwPending(localID string, p session.PendingRequest, confirm func(core.ConfirmDecision), answer func([]tools.QuestionAnswer)) {
	switch p.Kind {
	case "confirm":
		req := &confirmRequest{pendingID: localID, returnToChild: true, onAnswer: func(dec core.ConfirmDecision) { go confirm(dec) }}
		if p.Confirm != nil {
			req.toolName, req.preview = p.Confirm.Name, p.Confirm.Summary
		}
		i.confirmDialog.Enqueue(req)
		i.alertNeedsInput(alertAwaitingConfirmation)
	case "question":
		i.questionDialog.EnqueuePending(localID, p.Questions, func(a []tools.QuestionAnswer) { go answer(a) })
		i.alertNeedsInput(alertAwaitingAnswers)
	default:
		return
	}
	i.invalidate()
}

// retractBtwPending drops a fork request resolved elsewhere or orphaned by
// closing the side chat.
func (i *Interactive) retractBtwPending(localID string) {
	i.confirmDialog.RemovePending(localID)
	i.questionDialog.RemovePending(localID)
	i.invalidate()
}
