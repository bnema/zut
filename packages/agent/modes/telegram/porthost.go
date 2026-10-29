package telegram

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/bnema/zut/packages/agent/modes/bot"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/provider"
)

// portHostTimeout bounds one Port call made for a Telegram update.
const portHostTimeout = 10 * time.Second

// PortHost adapts a session Port to the Bridge Host. It lets the bridge run
// next to the session (in the daemon), independent of any attached client.
type PortHost struct {
	Port session.Port
	// Subscription marks OAuth/subscription billing in /status.
	Subscription bool
	// Log receives bridge notices and errors; nil discards them.
	Log io.Writer
}

var _ Host = (*PortHost)(nil)

// SubmitOrQueue submits the DM to the session with origin telegram.
func (h *PortHost) SubmitOrQueue(prompt string, images []provider.ImageBlock) {
	ctx, cancel := context.WithTimeout(context.Background(), portHostTimeout)
	defer cancel()
	if err := h.Port.Submit(ctx, session.Input{Text: prompt, Images: images, Mode: session.SubmitModeAuto, Origin: session.InputOriginTelegram}); err != nil {
		h.Notify("error", fmt.Sprintf("telegram: submit: %v", err))
	}
}

// CancelTurn aborts the session's active turn.
func (h *PortHost) CancelTurn() {
	ctx, cancel := context.WithTimeout(context.Background(), portHostTimeout)
	defer cancel()
	if err := h.Port.Abort(ctx); err != nil {
		h.Notify("error", fmt.Sprintf("telegram: abort: %v", err))
	}
}

// Status renders the session Info for /status.
func (h *PortHost) Status() string {
	return bot.FormatStatus(bot.StatusFromInfo(h.Port.Info(), h.Subscription, 0))
}

// Notify writes a bridge notice to Log.
func (h *PortHost) Notify(level, message string) {
	if h.Log != nil {
		fmt.Fprintf(h.Log, "%s: %s\n", level, message)
	}
}

// Follow mirrors finished session runs into the paired chat until ctx ends
// or the session closes: prompts typed by the user are sent as "you: ...",
// then the final assistant text. Runs started from Telegram only send the
// reply (the user's own message is already in the chat).
func (b *Bridge) Follow(ctx context.Context, port session.Port) error {
	return bot.WatchTurns(ctx, port, func(t bot.TurnResult) {
		switch t.Origin {
		case session.InputOriginTelegram:
			b.mu.Lock()
			b.nextReplyFromTelegram = true
			b.mu.Unlock()
		case session.InputOriginUser:
			for _, p := range t.Prompts {
				b.OnUserTyped(p)
			}
		}
		if t.Text == "" {
			b.stopWorking()
			return
		}
		b.OnAssistantText(t.Text)
	})
}
