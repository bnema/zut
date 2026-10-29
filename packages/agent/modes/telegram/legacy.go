package telegram

// IsRunning always reports no process: the bot service runs inside the zut
// daemon (bot_status), never as a PID-file process. Kept only so the
// in-process TUI compiles until L16c moves /telegram to telegram_bridge;
// delete it then.
//
// Deprecated: use the daemon bot_status request.
func IsRunning(string) (int, bool, error) { return 0, false, nil }
