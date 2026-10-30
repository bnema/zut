package modes

import (
	"context"
	"fmt"

	"github.com/bnema/zut/packages/tui"
)

const daemonExitPrompt = "Q quit · D detach · Esc cancel"

// handleDaemonExitMenu consumes every key while the lifecycle menu is open.
// In particular, cancellation must not leak Esc through to turn abortion.
func (i *Interactive) handleDaemonExitMenu(ctx context.Context, k tui.Key) (bool, bool) {
	i.mu.Lock()
	if !i.daemonExitMenu {
		i.mu.Unlock()
		return false, false
	}
	i.daemonExitMenu = false
	i.statusOK = ""
	i.mu.Unlock()
	switch {
	case k.Kind == tui.KeyCtrlC:
		return true, i.forceDaemonExit(ctx)
	case k.Kind == tui.KeyRune && (k.Rune == 'q' || k.Rune == 'Q'):
		i.portExit(ctx)
	case k.Kind == tui.KeyRune && (k.Rune == 'd' || k.Rune == 'D'):
		return true, i.portDetach()
	}
	return true, false
}

// A forced exit waits only for the existing bounded session-call timeout.
// Keep the UI independent of a client that fails to honor context cancellation.
func (i *Interactive) forceDaemonExit(ctx context.Context) bool {
	i.mu.Lock()
	client, id := i.client, i.sessionID
	i.mu.Unlock()
	callCtx, cancel := context.WithTimeout(ctx, i.sessionCallTimeout())
	defer cancel()
	result := make(chan error, 1)
	if client == nil {
		result <- fmt.Errorf("exit: no session client")
	} else {
		go func() { result <- client.Close(callCtx, id, portCloseReasonUserExit) }()
	}
	var err error
	select {
	case err = <-result:
	case <-callCtx.Done():
		err = callCtx.Err()
	}
	i.mu.Lock()
	if err != nil {
		i.exitNotice = fmt.Sprintf("exit: close %s: %v\nCheck zut ls; if the session is still running, attach with: zut attach %s", id, err, id)
	} else {
		i.addResumeHintLocked()
	}
	i.mu.Unlock()
	i.markCtrlCExit()
	return true
}
