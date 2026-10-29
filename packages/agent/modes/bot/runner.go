package bot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/bnema/zut/packages/agent/session"
)

// Config holds runner-level settings that are protocol-independent.
type Config struct {
	// Subscription marks OAuth/subscription billing in /status.
	Subscription bool
	// Log receives diagnostics (never message text or credentials).
	// Nil discards them.
	Log io.Writer
}

// Runner is the protocol-agnostic bot engine. It forwards inbound messages
// to a session Port (origin telegram), and sends each finished bot-originated
// run's final assistant text back through the BotAdapter. Queueing and
// turn execution belong to the session.
type Runner struct {
	port    session.Port
	adapter BotAdapter
	cfg     Config

	mu      sync.Mutex
	pending []string // channel IDs waiting for a reply, oldest first
	working func()   // stops the typing indicator
	runCtx  context.Context
	// outbox holds replies for the sender goroutine, so the session event
	// loop never waits on the network. Bounded by replyQueueSize; on
	// overflow the oldest reply is dropped.
	outbox []reply
	wake   chan struct{}
}

// replyQueueSize bounds queued outgoing replies.
const replyQueueSize = 16

type reply struct{ channel, text string }

// NewRunner creates a Runner wired to the given adapter and session port.
func NewRunner(adapter BotAdapter, port session.Port, cfg Config) *Runner {
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	return &Runner{port: port, adapter: adapter, cfg: cfg, wake: make(chan struct{}, 1)}
}

// enqueueReply queues a reply without blocking.
func (r *Runner) enqueueReply(channel, text string) {
	r.mu.Lock()
	if len(r.outbox) >= replyQueueSize {
		r.outbox = r.outbox[1:]
		fmt.Fprintln(r.cfg.Log, "bot: reply queue full; dropped the oldest reply")
	}
	r.outbox = append(r.outbox, reply{channel, text})
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// sendLoop delivers queued replies in order until ctx ends.
func (r *Runner) sendLoop(ctx context.Context) {
	for {
		r.mu.Lock()
		var next *reply
		if len(r.outbox) > 0 {
			next = &r.outbox[0]
			r.outbox = r.outbox[1:]
		}
		r.mu.Unlock()
		if next == nil {
			select {
			case <-ctx.Done():
				return
			case <-r.wake:
			}
			continue
		}
		// Adapter.Send is responsible for chunking to protocol limits.
		if err := r.adapter.Send(ctx, next.channel, next.text, SendOptions{}); err != nil {
			fmt.Fprintln(r.cfg.Log, "bot: send reply:", err)
		}
	}
}

// Run drives the adapter and the session watcher until ctx ends, the adapter
// fails, or the session closes.
func (r *Runner) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.mu.Lock()
	r.runCtx = ctx
	r.mu.Unlock()
	defer r.stopWorking()

	sendDone := make(chan struct{})
	go func() { defer close(sendDone); r.sendLoop(ctx) }()
	defer func() { <-sendDone }()
	watchErr := make(chan error, 1)
	go func() {
		err := WatchTurns(ctx, r.port, r.onTurn)
		cancel() // a closed session ends the service
		watchErr <- err
	}()
	adErr := r.adapter.Run(ctx, r.handleMessage, r.handleCommand)
	cancel()
	wErr := <-watchErr
	if errors.Is(wErr, session.ErrClosed) {
		return wErr
	}
	if errors.Is(adErr, context.Canceled) {
		return nil
	}
	return adErr
}

func (r *Runner) ctx() context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runCtx == nil {
		return context.Background()
	}
	return r.runCtx
}

// handleMessage forwards a normal inbound message to the session.
func (r *Runner) handleMessage(msg InboundMessage) {
	if msg.Text == "" && len(msg.Images) == 0 {
		return
	}
	ctx := r.ctx()
	r.mu.Lock()
	r.pending = append(r.pending, msg.ChannelID)
	if r.working == nil {
		r.working = r.adapter.IndicateWorking(ctx, msg.ChannelID)
	}
	r.mu.Unlock()
	err := r.port.Submit(ctx, session.Input{Text: msg.Text, Images: msg.Images, Mode: session.SubmitModeAuto, Origin: session.InputOriginTelegram})
	if err != nil {
		r.mu.Lock()
		if n := len(r.pending); n > 0 {
			r.pending = r.pending[:n-1]
		}
		r.mu.Unlock()
		r.stopWorkingIfIdle()
		fmt.Fprintln(r.cfg.Log, "bot: submit:", err)
		_ = r.adapter.Send(ctx, msg.ChannelID, "error: "+err.Error(), SendOptions{ReplyToMessageID: msg.MessageID})
	}
}

// onTurn replies to the oldest waiting channel when a bot-originated run ends.
func (r *Runner) onTurn(t TurnResult) {
	if t.Origin != session.InputOriginTelegram {
		return
	}
	r.mu.Lock()
	if len(r.pending) == 0 {
		r.mu.Unlock()
		return
	}
	ch := r.pending[0]
	r.pending = r.pending[1:]
	r.mu.Unlock()
	r.stopWorkingIfIdle()

	text := strings.TrimSpace(t.Text)
	if t.Err != nil && !errors.Is(t.Err, context.Canceled) {
		text = "error: " + t.Err.Error()
	}
	if text == "" {
		text = "(no reply)"
	}
	r.enqueueReply(ch, text)
}

func (r *Runner) stopWorkingIfIdle() {
	r.mu.Lock()
	if len(r.pending) > 0 {
		r.mu.Unlock()
		return
	}
	stop := r.working
	r.working = nil
	r.mu.Unlock()
	if stop != nil {
		stop()
	}
}

func (r *Runner) stopWorking() {
	r.mu.Lock()
	stop := r.working
	r.working = nil
	r.pending = nil
	r.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// handleCommand is called by the adapter for built-in commands.
func (r *Runner) handleCommand(cmd Command, msg InboundMessage) {
	ctx := r.ctx()
	opts := SendOptions{ReplyToMessageID: msg.MessageID}
	switch cmd {
	case CmdStart, CmdHelp:
		_ = r.adapter.Send(ctx, msg.ChannelID,
			"send me any message and i'll forward it to zut. attach an image and i'll pass it to the model. commands: /status, /stop, or plain stop.",
			opts)
	case CmdStatus:
		_ = r.adapter.Send(ctx, msg.ChannelID, r.Status(), opts)
	case CmdStop:
		info := r.port.Info()
		if info.Run == session.RunStateIdle {
			_ = r.adapter.Send(ctx, msg.ChannelID, "nothing running.", opts)
			return
		}
		if err := r.port.Abort(ctx); err != nil {
			_ = r.adapter.Send(ctx, msg.ChannelID, "error: "+err.Error(), opts)
			return
		}
		_ = r.adapter.Send(ctx, msg.ChannelID, "cancelled the current turn.", opts)
	}
}

// Status renders the /status reply from the session Info.
func (r *Runner) Status() string {
	info := r.port.Info()
	r.mu.Lock()
	waiting := len(r.pending)
	r.mu.Unlock()
	queued := waiting - 1
	if info.Run == session.RunStateIdle {
		queued = waiting
	}
	if queued < 0 {
		queued = 0
	}
	status := FormatStatus(StatusFromInfo(info, r.cfg.Subscription, queued))
	if extra := r.adapter.StatusText(); extra != "" {
		status += "\n" + extra
	}
	return status
}

// StatusFromInfo maps a session Info to the /status snapshot.
func StatusFromInfo(info session.Info, subscription bool, queued int) StatusSnapshot {
	return StatusSnapshot{
		Provider:     info.Provider,
		Model:        info.Model,
		CWD:          info.CWD,
		Usage:        info.Usage,
		Subscription: subscription,
		ContextUsed:  info.ContextUsed,
		ContextMax:   info.ContextMax,
		Busy:         info.Run != session.RunStateIdle,
		Queued:       queued,
	}
}
