package modes

import (
	"context"

	"github.com/bnema/zut/packages/tui"
)

// copyLoginURL copies the manual OAuth URL without disturbing the code editor.
// OSC 52 is preferred so an SSH client can copy to the user's local terminal;
// a process-local system clipboard is the fallback when terminal output fails.
func (i *Interactive) copyLoginURL(ctx context.Context) {
	i.mu.Lock()
	url := i.dialog.url
	term := i.cfg.Terminal
	i.mu.Unlock()

	if term != nil && tui.WriteClipboardTextOSC52(term, url) == nil {
		i.mu.Lock()
		i.statusErr = ""
		i.statusOK = "sent login URL to terminal clipboard"
		i.mu.Unlock()
		i.invalidate()
		return
	}

	err := tui.WriteClipboardText(ctx, url)
	if err == nil {
		i.mu.Lock()
		i.statusErr = ""
		i.statusOK = "login URL copied"
		i.mu.Unlock()
		i.invalidate()
		return
	}

	i.mu.Lock()
	i.statusErr = "copy login URL: " + err.Error()
	i.statusOK = ""
	i.mu.Unlock()
	i.invalidate()
}
