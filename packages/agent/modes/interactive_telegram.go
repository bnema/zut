package modes

import (
	"context"
	"errors"
	"fmt"

	"github.com/bnema/zut/packages/agent/modes/telegram"
)

func (i *Interactive) openTelegramDialog() {
	items := i.telegramMenuItems()
	if len(items) == 0 {
		i.mu.Lock()
		i.statusErr = "telegram not configured. run `zut telegram-bot setup` first."
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.telegramDialog.Open(items)
	i.invalidate()
}
func (i *Interactive) telegramMenuItems() []telegramItem {
	cfg, err := telegram.LoadConfig(i.cfg.ZutHome)
	if err != nil || cfg.BotToken == "" {
		return nil
	}
	var items []telegramItem
	if i.telegramBridge != nil && i.telegramBridge.Active() {
		items = append(items, telegramItem{label: "disconnect", action: "disconnect", hint: "stop mirroring"})
		st := i.telegramBridge.State()
		hint := "active"
		if st.Username != "" {
			hint += " as @" + st.Username
		}
		items = append(items, telegramItem{label: "status", action: "status", hint: hint})
	} else {
		label := "connect"
		hint := "start mirroring dms into this session"
		if cfg.AllowedUserID == 0 {
			hint = "awaiting pairing (send /start to the bot once connected)"
		}
		items = append(items, telegramItem{label: label, action: "connect", hint: hint})
		items = append(items, telegramItem{label: "status", action: "status", hint: "disconnected"})
	}
	return items
}
func (i *Interactive) doTelegram(action string) {
	switch action {
	case "connect":
		i.telegramConnect()
	case "disconnect":
		i.telegramDisconnect()
	case "status":
		i.telegramStatus()
	default:
		i.mu.Lock()
		i.statusErr = "unknown telegram action: " + action + " (use connect, disconnect, or status)"
		i.mu.Unlock()
		i.invalidate()
	}
}
func (i *Interactive) telegramConnect() {
	if i.telegramBridge != nil && i.telegramBridge.Active() {
		i.mu.Lock()
		i.statusOK = "telegram already connected"
		i.statusErr = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	cfg, err := telegram.LoadConfig(i.cfg.ZutHome)
	if err != nil {
		i.mu.Lock()
		i.statusErr = "telegram: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if cfg.BotToken == "" {
		i.mu.Lock()
		i.statusErr = "telegram: no bot token configured. run `zut telegram-bot setup` first."
		i.mu.Unlock()
		i.invalidate()
		return
	}
	// Refuse to start when a background daemon is already polling
	// the same bot. Two concurrent long-poll consumers race each
	// update and one always loses, so DMs get half-delivered. The
	// user can `zut telegram-bot stop` first, then /telegram connect.
	if pid, alive, _ := telegram.IsRunning(i.cfg.ZutHome); alive && pid > 0 {
		i.mu.Lock()
		i.statusErr = fmt.Sprintf("telegram: bot daemon already running (pid %d). stop it with `zut telegram-bot stop` first.", pid)
		i.mu.Unlock()
		i.invalidate()
		return
	}
	bridge := &telegram.Bridge{
		Client: telegram.NewClient(cfg.BotToken),
		Config: cfg,
		Save: func(next telegram.Config) error {
			return telegram.SaveConfig(i.cfg.ZutHome, next)
		},
		Host: &telegramHost{iv: i},
	}
	i.mu.Lock()
	i.telegramBridge = bridge
	i.mu.Unlock()
	// Strip web_search before the bridge's startup handshake can accept a
	// Telegram update. ApplyAgentPromptConfig also keeps it stripped from
	// concurrent refreshes while this bridge pointer is attached.
	i.applyTelegramTools(true)
	if err := bridge.Start(i.runCtx); err != nil {
		i.mu.Lock()
		i.telegramBridge = nil
		i.mu.Unlock()
		refreshErr := i.refreshToolsAfterTelegram()
		i.mu.Lock()
		i.statusErr = "telegram connect failed: " + err.Error()
		if refreshErr != nil {
			i.statusErr += "; tool refresh: " + refreshErr.Error()
		}
		i.mu.Unlock()
		i.invalidate()
		return
	}
	state := bridge.State()
	label := "telegram connected"
	if state.Username != "" {
		label += " as @" + state.Username
	}
	if state.PairedID == 0 {
		label += " — send /start to the bot from your phone to claim it"
	}
	i.mu.Lock()
	i.statusOK = label
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
}
func (i *Interactive) telegramDisconnect() {
	if i.telegramBridge == nil || !i.telegramBridge.Active() {
		i.mu.Lock()
		i.statusOK = "telegram already disconnected"
		i.statusErr = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	bridge := i.telegramBridge
	bridge.Stop()
	i.mu.Lock()
	// Clear the pointer before rebuilding the normal registry. This both
	// allows web_search back into the refresh and marks the bridge inactive
	// for any concurrent prompt-config commit.
	i.telegramBridge = nil
	i.mu.Unlock()
	refreshErr := i.refreshToolsAfterTelegram()
	i.mu.Lock()
	if refreshErr != nil {
		i.statusOK = ""
		i.statusErr = "telegram disconnect tool refresh: " + refreshErr.Error()
	} else {
		i.statusOK = "telegram disconnected"
		i.statusErr = ""
	}
	i.mu.Unlock()
	i.invalidate()
}
func (i *Interactive) refreshToolsAfterTelegram() error {
	if i.cfg.RefreshTools == nil {
		i.stripWebSearchTool()
		i.applyTelegramTools(false)
		return errors.New("live tool refresh is unavailable")
	}
	if err := i.cfg.RefreshTools(); err != nil {
		i.stripWebSearchTool()
		i.applyTelegramTools(false)
		return err
	}
	i.applyTelegramTools(false)
	return nil
}
func (a telegramSenderAdapter) SendImage(ctx context.Context, path, caption string) error {
	if a.bridge == nil {
		return fmt.Errorf("telegram bridge is not connected")
	}
	return a.bridge.SendImage(ctx, path, caption)
}
func (a telegramSenderAdapter) SendDocument(ctx context.Context, path, caption string) error {
	if a.bridge == nil {
		return fmt.Errorf("telegram bridge is not connected")
	}
	return a.bridge.SendDocument(ctx, path, caption)
}
func (a telegramSenderAdapter) Active() bool {
	return a.bridge != nil && a.bridge.Active()
}
