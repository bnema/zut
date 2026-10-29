package telegram

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/modes/bot"
	"github.com/bnema/zut/packages/provider"
)

// Adapter implements bot.BotAdapter for Telegram.
type Adapter struct {
	Client *Client
	Cfg    *Config // pointer so Run can mutate and persist
	Save   func(Config) error
	// Log receives diagnostics; nil uses stderr. Never the token.
	Log io.Writer
}

func (a *Adapter) log() io.Writer {
	if a.Log != nil {
		return a.Log
	}
	return stderr()
}

// NewAdapter creates a Telegram adapter.
func NewAdapter(client *Client, cfg *Config, save func(Config) error) *Adapter {
	return &Adapter{Client: client, Cfg: cfg, Save: save}
}

// Run drives the Telegram long-polling loop.  It performs initial
// GetMe, handles pairing, and dispatches inbound messages to the
// generic handler / commandHandler callbacks.
func (a *Adapter) Run(ctx context.Context,
	handler func(bot.InboundMessage),
	commandHandler func(bot.Command, bot.InboundMessage),
) error {
	if a.Cfg.BotToken == "" {
		return fmt.Errorf("no bot token configured; run `zut bot setup` first")
	}
	me, err := a.Client.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("getMe: %w", err)
	}
	if err := a.Client.DeleteWebhook(ctx, false); err != nil {
		return fmt.Errorf("remove telegram webhook before polling: %w", err)
	}
	// Keep the stored username/id in sync with the actual bot.
	if a.Cfg.BotID != me.ID || a.Cfg.BotUsername != me.Username {
		a.Cfg.BotID = me.ID
		a.Cfg.BotUsername = me.Username
		_ = a.Save(*a.Cfg)
	}

	fmt.Fprintf(a.log(), "telegram bot online as @%s (id=%d)\n", me.Username, me.ID)
	if a.Cfg.AllowedUserID == 0 {
		fmt.Fprintln(a.log(), "no user paired yet — send /start to the bot from Telegram to claim it")
	} else {
		fmt.Fprintf(a.log(), "paired with telegram user id %d\n", a.Cfg.AllowedUserID)
	}

	return a.pollLoop(ctx, handler, commandHandler)
}

// pollLoop long-polls Telegram for updates and dispatches them.
func (a *Adapter) pollLoop(ctx context.Context,
	handler func(bot.InboundMessage),
	commandHandler func(bot.Command, bot.InboundMessage),
) error {
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		updates, err := a.Client.GetUpdates(ctx, a.Cfg.LastUpdateID+1, 30)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintln(a.log(), "telegram: getUpdates error:", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		for _, u := range updates {
			a.handleUpdate(ctx, u, handler, commandHandler)
			a.Cfg.LastUpdateID = u.UpdateID
			_ = a.Save(*a.Cfg)
		}
	}
}

// handleUpdate processes a single Telegram update.  Telegram-specific
// concerns (pairing, user filtering, image download) live here; the
// generic callbacks are called for normal messages and commands.
func (a *Adapter) handleUpdate(ctx context.Context, u Update,
	handler func(bot.InboundMessage),
	commandHandler func(bot.Command, bot.InboundMessage),
) {
	msg := u.Message
	if msg == nil {
		msg = u.Edited
	}
	if msg == nil || msg.From == nil || msg.From.IsBot {
		return
	}
	if msg.Chat.Type != "private" {
		return
	}

	chanID := fmt.Sprintf("%d", msg.Chat.ID)
	msgID := fmt.Sprintf("%d", msg.MessageID)

	// Pairing: first user who sends /start claims the bridge.
	text := strings.TrimSpace(msg.Text)
	command, isCommand := bot.ParseCommand(text)
	if a.Cfg.AllowedUserID == 0 {
		if isCommand && command == bot.CmdStart {
			a.Cfg.AllowedUserID = msg.From.ID
			_ = a.Save(*a.Cfg)
			_ = a.Client.SendMessage(ctx, msg.Chat.ID,
				fmt.Sprintf("paired with @%s. send any message and i'll forward it to zut.", msg.From.Username),
				msg.MessageID)
			return
		}
		_ = a.Client.SendMessage(ctx, msg.Chat.ID,
			"this bot isn't paired yet. send /start to claim it.",
			msg.MessageID)
		return
	}

	// Enforce allowed user.
	if msg.From.ID != a.Cfg.AllowedUserID {
		_ = a.Client.SendMessage(ctx, msg.Chat.ID,
			"this bot is paired with a different user.",
			msg.MessageID)
		return
	}

	inbound := bot.InboundMessage{
		ChannelID: chanID,
		MessageID: msgID,
	}

	// Built-in commands that bypass the agent.
	if isCommand {
		commandHandler(command, inbound)
		return
	}
	if bot.IsStopCommand(text) {
		commandHandler(bot.CmdStop, inbound)
		return
	}

	// Build the prompt: combine text + caption; download image attachments.
	prompt := strings.TrimSpace(msg.Text)
	if msg.Caption != "" {
		if prompt != "" {
			prompt += "\n"
		}
		prompt += msg.Caption
	}

	var images []provider.ImageBlock
	if len(msg.Photo) > 0 {
		largest := msg.Photo[len(msg.Photo)-1]
		if data, mime, err := a.download(ctx, largest.FileID, ""); err == nil {
			images = append(images, provider.ImageBlock{MimeType: mime, Data: data})
		} else {
			fmt.Fprintln(a.log(), "telegram: download photo:", err)
		}
	}
	if msg.Document != nil && bot.IsImageMIME(msg.Document.MimeType) {
		if data, mime, err := a.download(ctx, msg.Document.FileID, msg.Document.MimeType); err == nil {
			images = append(images, provider.ImageBlock{MimeType: mime, Data: data})
		}
	}

	inbound.Text = prompt
	inbound.Images = images
	handler(inbound)
}

// Send delivers a reply to a Telegram chat.  channelID is parsed back
// to int64.  Messages are chunked to 4000 runes (Telegram limit 4096).
func (a *Adapter) Send(ctx context.Context, channelID, text string, opts bot.SendOptions) error {
	chatID, err := strconv.ParseInt(channelID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid channelID %q: %w", channelID, err)
	}
	replyTo := 0
	if opts.ReplyToMessageID != "" {
		if n, err := strconv.Atoi(opts.ReplyToMessageID); err == nil {
			replyTo = n
		}
	}
	for _, chunk := range bot.ChunkMessage(text, 4000) {
		if err := a.Client.SendMessage(ctx, chatID, chunk, replyTo); err != nil {
			return err
		}
		// Only the first chunk should be threaded under the original message.
		replyTo = 0
	}
	return nil
}

// IndicateWorking keeps Telegram's "typing..." indicator alive until
// the returned stop function is called.
func (a *Adapter) IndicateWorking(ctx context.Context, channelID string) (stop func()) {
	chatID, err := strconv.ParseInt(channelID, 10, 64)
	if err != nil {
		return func() {}
	}
	tctx, cancel := context.WithCancel(ctx)
	go func() {
		for {
			_ = a.Client.SendChatAction(tctx, chatID, "typing")
			select {
			case <-tctx.Done():
				return
			case <-time.After(4 * time.Second):
			}
		}
	}()
	return cancel
}

// StatusText returns the bot's @username for inclusion in /status.
func (a *Adapter) StatusText() string {
	if a.Cfg.BotUsername != "" {
		return "@" + a.Cfg.BotUsername
	}
	return ""
}

// download fetches a file from Telegram and returns bytes + mime.
func (a *Adapter) download(ctx context.Context, fileID, mime string) ([]byte, string, error) {
	f, err := a.Client.GetFile(ctx, fileID)
	if err != nil {
		return nil, "", err
	}
	data, err := a.Client.DownloadFile(ctx, f.FilePath)
	if err != nil {
		return nil, "", err
	}
	if mime == "" {
		mime = bot.GuessImageMIME(f.FilePath)
	}
	return data, mime, nil
}
