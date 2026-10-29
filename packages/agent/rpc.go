package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/bnema/zut/packages/agent/modes"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func resolveRPCModel(providerName, modelID string) (provider.Model, error) {
	model, err := provider.FindModel(providerName, modelID)
	if err == nil {
		return model, nil
	}
	if providerName == provider.ProviderOpenCodeGo && provider.AcceptsUnlistedModels(providerName) {
		return provider.DynamicOpenCodeGoModel(providerName, modelID, ""), nil
	}
	return provider.Model{}, err
}

func rpcReasoningTokens(usage provider.Usage) any {
	if !usage.ReasoningTokensKnown {
		return nil
	}
	return usage.ReasoningTokens
}

// messagesToJSON serialises a transcript using the same schema as the
// --json event mode for cross-format consistency.
func messagesToJSON(msgs []provider.Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, map[string]any{
			"role":    string(m.Role),
			"content": modes.ContentToJSON(m.Content),
			"time":    m.Time,
		})
	}
	return out
}

// runRPCClient serves the docs/rpc.md protocol on stdin/stdout as a bridge
// to a daemon session: the docs/rpc.md commands, responses and events,
// with the agent owned by the daemon. clear maps to the daemon clear (D9)
// and the bridge follows the successor session transparently. stdin EOF
// or Ctrl+C aborts any running turn and closes the session unless
// --keep-session is set.
func runRPCClient(ctx context.Context, args Args, rawArgs []string, version string) error {
	return runRPCBridge(ctx, processHeadlessIO(version), args, rawArgs, version, os.Stdin)
}

func runRPCBridge(parent context.Context, hio headlessIO, args Args, rawArgs []string, version string, in io.Reader) (runErr error) {
	ctx, stop := headlessSignalContext(parent)
	defer stop()
	if args.NoYolo {
		fmt.Fprintln(hio.stderr, "warning: --no-yolo has no effect in rpc mode (no interactive prompt available); tools will run without confirmation")
	}
	h, err := openHeadlessSession(ctx, hio, args, rawArgs)
	if err != nil {
		return err
	}
	b := &rpcBridge{h: h, out: hio.stdout, version: version, cwd: args.CWD, token: os.Getenv("ZUTCORE_RPC_TOKEN")}
	defer func() { runErr = errors.Join(runErr, b.end(args.KeepSession)) }()
	if err := b.subscribe(); err != nil {
		return err
	}
	lines := make(chan string)
	readErr := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			lines <- sc.Text()
		}
		readErr <- sc.Err()
		close(lines)
	}()
	for {
		select {
		case <-ctx.Done():
			b.abortActive()
			return ctx.Err()
		case line, ok := <-lines:
			if !ok {
				// stdin closed: let in-flight work finish before returning.
				b.waitIdle(ctx)
				return <-readErr
			}
			if err := b.handleLine(ctx, line); err != nil {
				return err
			}
		}
	}
}

// rpcBridge translates RPC commands into Port calls and session events into
// RPC frames. One prompt or compact runs at a time; a second one waits.
type rpcBridge struct {
	h       *headlessSession
	out     io.Writer
	version string
	cwd     string
	token   string
	authed  bool

	writeMu sync.Mutex
	mu      sync.Mutex
	// active is closed when the running prompt/compact reaches idle.
	active  chan struct{}
	compact bool
	// sawRunning marks that the active prompt's session left idle, so the
	// next idle Info ends it (stale idle events never do).
	sawRunning bool
	turnErr    error
	info       session.Info
	cancel     context.CancelFunc
	events     <-chan session.Event
	pumped     chan struct{}
}

func (b *rpcBridge) end(keep bool) error {
	b.mu.Lock()
	cancel, pumped := b.cancel, b.pumped
	b.mu.Unlock()
	err := b.h.end(keep)
	if cancel != nil {
		cancel()
	}
	if pumped != nil {
		<-pumped
	}
	return err
}

func (b *rpcBridge) subscribe() error {
	subCtx, cancel := context.WithCancel(context.Background())
	events, err := b.h.port.Subscribe(subCtx, 0)
	if err != nil {
		cancel()
		return fmt.Errorf("attach daemon session: %w", err)
	}
	pumped := make(chan struct{})
	b.mu.Lock()
	b.cancel, b.events, b.pumped = cancel, events, pumped
	b.info = b.h.port.Info()
	b.mu.Unlock()
	go b.pump(events, pumped)
	return nil
}

// pump forwards events of the current session until its stream ends.
func (b *rpcBridge) pump(events <-chan session.Event, done chan struct{}) {
	defer close(done)
	for ev := range events {
		switch ev.Kind {
		case session.EvAgent:
			if ev.Agent == nil {
				continue
			}
			if _, ok := ev.Agent.(core.EvDone); ok {
				// The bridge emits one done per command when the session is idle.
				continue
			}
			b.mu.Lock()
			if e, ok := ev.Agent.(core.EvTurnEnd); ok && e.Err != nil {
				b.turnErr = e.Err
			}
			b.mu.Unlock()
			b.write(modes.EventToJSON(ev.Agent))
		case session.EvInfo:
			if i, ok := ev.Data.(session.InfoEvent); ok {
				b.mu.Lock()
				b.info = i.Info
				if b.active != nil && i.Info.Run != session.RunStateIdle {
					b.sawRunning = true
				}
				b.mu.Unlock()
				if i.Info.Run == session.RunStateIdle {
					b.finishActive()
				}
			}
		case session.EvCompaction:
			if c, ok := ev.Data.(session.CompactionEvent); ok {
				switch c.Phase {
				case "done":
					b.write(map[string]any{"type": "compact_done", "summary": c.Summary})
					b.finishActive()
				case "failed":
					b.finishActive()
				}
			}
		case session.EvNote:
			if n, ok := ev.Data.(session.NoteEvent); ok && n.Note.Level == "error" {
				b.mu.Lock()
				compacting := b.active != nil && b.compact
				b.mu.Unlock()
				if compacting {
					b.write(map[string]any{"type": "error", "message": n.Note.Text})
				}
			}
		case session.EvExtDisplay:
			if d, ok := ev.Data.(session.ExtDisplayEvent); ok {
				b.write(map[string]any{"type": "ext_display", "extension": d.Ext, "text": d.Text})
			}
		case session.EvExtAlert:
			if a, ok := ev.Data.(session.ExtAlertEvent); ok {
				b.write(map[string]any{"type": "ext_alert", "kind": a.Alert.Kind, "reason": a.Alert.Reason})
			}
		case session.EvPending:
			if p, ok := ev.Data.(session.PendingEvent); ok {
				go declineHeadlessPending(b.h)(p.Pending)
			}
		}
	}
	// A cleared or closed session ends its stream; release any waiter.
	b.finishActive()
}

// finishActive ends the running prompt/compact once it has produced events.
func (b *rpcBridge) finishActive() {
	b.mu.Lock()
	ch, compact, turnErr := b.active, b.compact, b.turnErr
	if ch == nil {
		b.mu.Unlock()
		return
	}
	if !compact && (b.info.Run != session.RunStateIdle || !b.sawRunning) {
		b.mu.Unlock()
		return
	}
	b.active, b.turnErr = nil, nil
	b.mu.Unlock()
	if !compact {
		if turnErr != nil && !errors.Is(turnErr, context.Canceled) && !strings.Contains(turnErr.Error(), context.Canceled.Error()) {
			b.write(map[string]any{"type": "error", "message": turnErr.Error()})
		}
		b.write(map[string]any{"type": "done"})
	}
	close(ch)
}

// begin waits for the previous prompt/compact and registers a new one.
func (b *rpcBridge) begin(ctx context.Context, compact bool) (chan struct{}, error) {
	for {
		b.mu.Lock()
		prev := b.active
		if prev == nil {
			ch := make(chan struct{})
			b.active, b.compact, b.sawRunning, b.turnErr = ch, compact, false, nil
			b.mu.Unlock()
			return ch, nil
		}
		b.mu.Unlock()
		select {
		case <-prev:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (b *rpcBridge) cancelActive(ch chan struct{}) {
	b.mu.Lock()
	if b.active == ch {
		b.active = nil
	}
	b.mu.Unlock()
	close(ch)
}

func (b *rpcBridge) waitIdle(ctx context.Context) {
	b.mu.Lock()
	ch := b.active
	b.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case <-ch:
	case <-ctx.Done():
		b.abortActive()
	}
}

func (b *rpcBridge) abortActive() { b.h.abort() }

func (b *rpcBridge) handleLine(ctx context.Context, line string) error {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	var head struct {
		Type string `json:"type"`
		ID   string `json:"id,omitempty"`
	}
	if err := json.Unmarshal([]byte(line), &head); err != nil {
		b.writeError("", "", fmt.Sprintf("malformed json: %v", err))
		return nil
	}
	if b.token != "" && !b.authed {
		if head.Type != "hello" {
			b.writeError(head.ID, head.Type, "auth required: send hello with token first")
			return nil
		}
		var hello struct {
			Token string `json:"token"`
		}
		_ = json.Unmarshal([]byte(line), &hello)
		if hello.Token != b.token {
			b.writeError(head.ID, head.Type, "invalid token")
			return fmt.Errorf("rpc: bad auth token")
		}
		b.authed = true
	}
	b.dispatch(ctx, head.Type, head.ID, []byte(line))
	return nil
}

func (b *rpcBridge) currentInfo() session.Info {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.info
}

func (b *rpcBridge) dispatch(ctx context.Context, cmd, id string, raw []byte) {
	port := b.h.port
	switch cmd {
	case "hello":
		info := b.currentInfo()
		b.writeResponse(id, cmd, map[string]any{"protocol_version": 1, "version": b.version, "provider": info.Provider, "model": info.Model})
	case "ping":
		b.writeResponse(id, cmd, map[string]any{"pong": true})
	case "prompt":
		var req struct {
			Message string `json:"message"`
			Images  []struct {
				MimeType string `json:"mime_type"`
				Data     []byte `json:"data"`
			} `json:"images"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			b.writeError(id, cmd, err.Error())
			return
		}
		ch, err := b.begin(ctx, false)
		if err != nil {
			b.writeError(id, cmd, err.Error())
			return
		}
		b.writeResponse(id, cmd, map[string]any{"started": true})
		in := session.Input{Text: req.Message, Origin: session.InputOriginUser}
		for _, im := range req.Images {
			in.Images = append(in.Images, provider.ImageBlock{MimeType: im.MimeType, Data: im.Data})
		}
		if err := port.Submit(ctx, in); err != nil {
			b.write(map[string]any{"type": "error", "message": err.Error()})
			b.write(map[string]any{"type": "done"})
			b.cancelActive(ch)
			return
		}
	case "abort":
		_ = port.Abort(ctx)
		b.writeResponse(id, cmd, nil)
	case "compact":
		ch, err := b.begin(ctx, true)
		if err != nil {
			b.writeError(id, cmd, err.Error())
			return
		}
		b.writeResponse(id, cmd, map[string]any{"started": true})
		if err := port.Compact(ctx); err != nil {
			b.write(map[string]any{"type": "error", "message": err.Error()})
			b.cancelActive(ch)
		}
	case "get_state":
		info := b.currentInfo()
		snap, err := port.Snapshot(ctx)
		if err != nil {
			b.writeError(id, cmd, err.Error())
			return
		}
		cum := info.Usage
		b.writeResponse(id, cmd, map[string]any{
			"provider":      info.Provider,
			"model":         info.Model,
			"reasoning":     info.Reasoning,
			"cwd":           b.cwd,
			"message_count": len(snap.Messages),
			"busy":          info.Run != session.RunStateIdle,
			"usage": map[string]any{
				"input":                 cum.InputTokens,
				"output":                cum.OutputTokens,
				"reasoning":             rpcReasoningTokens(cum),
				"cache_read":            cum.CacheReadTokens,
				"cache_write":           cum.CacheWriteTokens,
				"cache_measured_prompt": cum.CacheMeasuredPromptTokens,
				"cache_measured_read":   cum.CacheMeasuredReadTokens,
				"cost_usd":              cum.CostUSD,
			},
		})
	case "get_messages":
		snap, err := port.Snapshot(ctx)
		if err != nil {
			b.writeError(id, cmd, err.Error())
			return
		}
		b.writeResponse(id, cmd, map[string]any{"messages": messagesToJSON(snap.Messages)})
	case "clear":
		if err := b.clear(ctx); err != nil {
			b.writeError(id, cmd, err.Error())
			return
		}
		b.writeResponse(id, cmd, nil)
	case "set_model":
		var req struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			b.writeError(id, cmd, err.Error())
			return
		}
		model := strings.TrimSpace(req.Model)
		if model == "" {
			b.writeError(id, cmd, "model must not be empty")
			return
		}
		info := b.currentInfo()
		if info.Run != session.RunStateIdle {
			b.writeError(id, cmd, "runtime is busy")
			return
		}
		if _, err := resolveRPCModel(info.Provider, model); err != nil {
			b.writeError(id, cmd, err.Error())
			return
		}
		if err := port.SetModel(ctx, info.Provider, model); err != nil {
			if errors.Is(err, session.ErrBusy) {
				err = errors.New("runtime is busy")
			}
			b.writeError(id, cmd, err.Error())
			return
		}
		b.writeResponse(id, cmd, map[string]any{"model": model})
	case "set_reasoning":
		var req struct {
			Reasoning string `json:"reasoning"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			b.writeError(id, cmd, err.Error())
			return
		}
		reasoning := provider.NormalizeReasoning(req.Reasoning)
		switch reasoning {
		case "", "minimum", "low", "medium", "high", "xhigh", "max":
		default:
			b.writeError(id, cmd, "reasoning must be off|minimum|low|medium|high|xhigh|max")
			return
		}
		if err := port.SetReasoning(ctx, reasoning); err != nil {
			b.writeError(id, cmd, err.Error())
			return
		}
		b.writeResponse(id, cmd, map[string]any{"reasoning": reasoning})
	case "get_models":
		out := []map[string]any{}
		for _, m := range provider.ModelsForProvider(b.currentInfo().Provider) {
			out = append(out, map[string]any{"id": m.ID, "provider": m.Provider, "context_window": m.ContextWindow, "max_output": m.MaxOutput, "reasoning": m.Reasoning})
		}
		b.writeResponse(id, cmd, map[string]any{"models": out})
	default:
		b.writeError(id, cmd, "unknown command")
	}
}

// clear asks the daemon for a successor session (D9) and moves the bridge
// to it: the old stream ends, a new one starts on the successor.
func (b *rpcBridge) clear(ctx context.Context) error {
	next, err := b.h.client.Clear(ctx, b.h.id)
	if err != nil {
		return err
	}
	b.mu.Lock()
	cancel, pumped := b.cancel, b.pumped
	b.mu.Unlock()
	cancel()
	<-pumped
	b.h.id = session.ID(next.ID)
	b.h.info = next
	b.h.port = b.h.client.Port(b.h.id, true)
	return b.subscribe()
}

func (b *rpcBridge) writeResponse(id, cmd string, data any) {
	frame := map[string]any{"type": "response", "command": cmd, "success": true}
	if id != "" {
		frame["id"] = id
	}
	if data != nil {
		frame["data"] = data
	}
	b.write(frame)
}

func (b *rpcBridge) writeError(id, cmd, msg string) {
	frame := map[string]any{"type": "response", "command": cmd, "success": false, "error": msg}
	if id != "" {
		frame["id"] = id
	}
	b.write(frame)
}

func (b *rpcBridge) write(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	_, _ = b.out.Write(append(raw, '\n'))
}
