package daemonproto

import (
	"time"

	"github.com/bnema/zut/packages/agent/session"
)

// BotSpec describes a daemon-hosted bot service. It never carries
// credentials: the daemon reads the bot token from $ZUT_HOME (for Telegram,
// bot.json, mode 0600). CWD, Env and Args are captured by the client and
// become the bot session's session.CreateSpec (Headless is forced).
type BotSpec struct {
	// Kind selects the protocol; "telegram" is the only kind.
	Kind string   `json:"kind"`
	CWD  string   `json:"cwd"`
	Env  []string `json:"env"`
	Args []string `json:"args"`
}

// BotStartRequest is the bot_start frame. Type must be "bot_start".
type BotStartRequest struct {
	ID   string  `json:"id"`
	Type string  `json:"type"`
	Spec BotSpec `json:"spec"`
}

// BotStopRequest is the bot_stop frame. Type must be "bot_stop".
type BotStopRequest struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Kind string `json:"kind"`
}

// BotStatusRequest is the bot_status frame. Type must be "bot_status".
type BotStatusRequest struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Kind string `json:"kind"`
}

// TelegramBridgeRequest is the telegram_bridge frame. Type must be
// "telegram_bridge". On mirrors the paired Telegram DM into Session; off
// stops the mirror.
type TelegramBridgeRequest struct {
	ID      string     `json:"id"`
	Type    string     `json:"type"`
	Session session.ID `json:"session"`
	On      bool       `json:"on"`
}

// BotStatusData is the result of bot_start, bot_stop and bot_status.
type BotStatusData struct {
	Kind    string     `json:"kind"`
	Running bool       `json:"running"`
	Session session.ID `json:"session,omitempty"`
	// StartedAt is zero when the service is not running.
	StartedAt    time.Time `json:"started_at,omitempty"`
	Username     string    `json:"username,omitempty"`
	PairedUserID int64     `json:"paired_user_id,omitempty"`
	// LastError is why the last service run ended on its own, if it did.
	LastError string `json:"last_error,omitempty"`
	// Bridge is the interactive bridge state for the same kind.
	Bridge *TelegramBridgeData `json:"bridge,omitempty"`
}

// TelegramBridgeData is the result of telegram_bridge and the bridge part of
// bot_status.
type TelegramBridgeData struct {
	Active       bool       `json:"active"`
	Session      session.ID `json:"session,omitempty"`
	Username     string     `json:"username,omitempty"`
	PairedUserID int64      `json:"paired_user_id,omitempty"`
}
