package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/bnema/zut/packages/agent/modes/bot"
	"github.com/bnema/zut/packages/agent/session"
)

// Service runs the Telegram bot service and the interactive bridge against a
// session Port. The daemon hosts it; it reads bot.json from Home itself, so
// the token never crosses the daemon protocol, and it never logs the token.
type Service struct {
	Home string
	// Log receives diagnostics; nil discards them.
	Log io.Writer
	// Subscription marks OAuth/subscription billing in /status.
	Subscription func(port session.Port) bool

	// mu serializes bot.json read-modify-write between runs.
	mu sync.Mutex
}

// ErrNotConfigured reports a missing bot token.
var ErrNotConfigured = errors.New("telegram is not configured — run `zut telegram-bot setup` first")

func (s *Service) log() io.Writer {
	if s.Log == nil {
		return io.Discard
	}
	return s.Log
}

func (s *Service) load() (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := LoadConfig(s.Home)
	if err != nil {
		return Config{}, fmt.Errorf("load telegram config: %w", err)
	}
	if cfg.BotToken == "" {
		return Config{}, ErrNotConfigured
	}
	return cfg, nil
}

func (s *Service) save(c Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SaveConfig(s.Home, c)
}

func (s *Service) subscription(port session.Port) bool {
	return s.Subscription != nil && s.Subscription(port)
}

// Check reports whether the bot is configured.
func (s *Service) Check() error {
	_, err := s.load()
	return err
}

// Identity reports the bot username and the paired user (no secrets).
func (s *Service) Identity() (string, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := LoadConfig(s.Home)
	if err != nil {
		return "", 0
	}
	return cfg.BotUsername, cfg.AllowedUserID
}

// RunBot serves DMs as the bot service until ctx ends or the session closes.
func (s *Service) RunBot(ctx context.Context, port session.Port) error {
	cfg, err := s.load()
	if err != nil {
		return err
	}
	if err := disableWebSearch(ctx, port); err != nil {
		return fmt.Errorf("telegram bot: disable web search: %w", err)
	}
	adapter := NewAdapter(cfg.NewClient(), &cfg, s.save)
	adapter.Log = s.log()
	return bot.NewRunner(adapter, port, bot.Config{Subscription: s.subscription(port), Log: s.log()}).Run(ctx)
}

// RunBridge mirrors the paired chat into port until ctx ends or the session
// closes. ready is called once with the startup result.
func (s *Service) RunBridge(ctx context.Context, port session.Port, ready func(error)) error {
	cfg, err := s.load()
	if err != nil {
		ready(err)
		return err
	}
	// Telegram messages have no per-request confirmation surface, so
	// bridged sessions never keep native web search (as before).
	if err := disableWebSearch(ctx, port); err != nil {
		err = fmt.Errorf("telegram bridge: disable web search: %w", err)
		ready(err)
		return err
	}
	b := &Bridge{
		Client: cfg.NewClient(),
		Config: cfg,
		Save:   s.save,
		Host:   &PortHost{Port: port, Subscription: s.subscription(port), Log: s.log()},
	}
	if err := b.Start(ctx); err != nil {
		ready(err)
		return err
	}
	defer b.Stop()
	ready(nil)
	err = b.Follow(ctx, port)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func disableWebSearch(ctx context.Context, port session.Port) error {
	off := false
	return port.ApplySettings(ctx, session.SettingsPatch{WebSearchEnabled: &off})
}
