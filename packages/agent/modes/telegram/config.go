// Package telegram implements zut's Telegram bot bridge.
//
// It runs in-process, polling Telegram for DMs and forwarding them to
// a core.Agent. Responses stream back as Telegram messages. Images
// sent by the user are downloaded and attached to the next prompt as
// provider.ImageBlock, so vision-capable models see them the same way
// they would via drag-and-drop in the TUI.
//
// State (bot token, allowed user id) lives in $ZUT_HOME/bot.json.
package telegram

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Config is the on-disk state for the telegram bridge.
type Config struct {
	BotToken      string `json:"bot_token,omitempty"`
	BotUsername   string `json:"bot_username,omitempty"`
	BotID         int64  `json:"bot_id,omitempty"`
	AllowedUserID int64  `json:"allowed_user_id,omitempty"`
	LastUpdateID  int64  `json:"last_update_id,omitempty"`
	// APIBase overrides the Bot API server (self-hosted telegram-bot-api).
	// Empty means DefaultAPIBase.
	APIBase string `json:"api_base,omitempty"`
}

// NewClient returns a Bot API client for c's token and server.
func (c Config) NewClient() *Client { return NewClientWithBase(c.BotToken, c.APIBase) }

// ConfigPath returns the path to bot.json.
func ConfigPath(zutHome string) string {
	return filepath.Join(zutHome, "bot.json")
}

// LoadConfig reads bot.json, returning a zero Config if it doesn't exist.
func LoadConfig(zutHome string) (Config, error) {
	var c Config
	b, err := os.ReadFile(ConfigPath(zutHome))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	return c, nil
}

// SaveConfig writes bot.json atomically with mode 0600.
func SaveConfig(zutHome string, c Config) error {
	if err := os.MkdirAll(zutHome, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	path := ConfigPath(zutHome)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	// WriteFile keeps the mode of an existing temp file; force 0600.
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
