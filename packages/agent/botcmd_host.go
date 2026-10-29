package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/bnema/zut/packages/agent/daemon"
	"github.com/bnema/zut/packages/agent/modes/telegram"
	"github.com/bnema/zut/packages/agent/session"
)

// The session factory hosts bots inside the daemon (bot_start,
// telegram_bridge). Credentials are read from $ZUT_HOME by the service.
var _ daemon.BotFactory = (*sessionFactory)(nil)

// telegramServices holds one service per factory so bot.json writes from the
// bot service and the bridge are serialized.
var telegramServices sync.Map // *sessionFactory → *telegram.Service

func (f *sessionFactory) telegramService() *telegram.Service {
	v, _ := telegramServices.LoadOrStore(f, &telegram.Service{Home: f.home, Log: slogWriter{log: f.log, kind: "telegram"}})
	return v.(*telegram.Service)
}

func (f *sessionFactory) botService(kind string) (*telegram.Service, error) {
	if kind != "telegram" {
		return nil, fmt.Errorf("unknown bot kind %q", kind)
	}
	return f.telegramService(), nil
}

// CheckBot implements daemon.BotHost.
func (f *sessionFactory) CheckBot(kind string) error {
	s, err := f.botService(kind)
	if err != nil {
		return err
	}
	return s.Check()
}

// RunBot implements daemon.BotHost.
func (f *sessionFactory) RunBot(ctx context.Context, kind string, port session.Port) error {
	s, err := f.botService(kind)
	if err != nil {
		return err
	}
	return s.RunBot(ctx, port)
}

// RunBridge implements daemon.BotHost.
func (f *sessionFactory) RunBridge(ctx context.Context, kind string, port session.Port, ready func(error)) error {
	s, err := f.botService(kind)
	if err != nil {
		ready(err)
		return err
	}
	return s.RunBridge(ctx, port, ready)
}

// BotIdentity implements daemon.BotHost.
func (f *sessionFactory) BotIdentity(kind string) (string, int64) {
	s, err := f.botService(kind)
	if err != nil {
		return "", 0
	}
	return s.Identity()
}

// slogWriter forwards bot diagnostics (one line per write) to the daemon log.
type slogWriter struct {
	log  *slog.Logger
	kind string
}

var _ io.Writer = slogWriter{}

func (w slogWriter) Write(p []byte) (int, error) {
	msg := string(p)
	for len(msg) > 0 && (msg[len(msg)-1] == '\n' || msg[len(msg)-1] == '\r') {
		msg = msg[:len(msg)-1]
	}
	if msg != "" {
		w.log.Info(msg, "bot", w.kind)
	}
	return len(p), nil
}
