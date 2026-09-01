package modes

import "github.com/bnema/zut/packages/tui"

// copyLoginURL copies the manual OAuth URL without disturbing the code editor.
// OSC 52 is preferred so an SSH client can copy to the user's local terminal;
// a process-local system clipboard is the fallback when terminal output fails.
func (i *Interactive) copyLoginURL() {
	url := i.dialog.url

	i.mu.Lock()
	if i.cfg.Terminal != nil {
		if err := tui.WriteClipboardTextOSC52(i.cfg.Terminal, url); err == nil {
			i.statusErr = ""
			i.statusOK = "sent login URL to terminal clipboard"
			i.mu.Unlock()
			i.invalidate()
			return
		}
	}
	i.mu.Unlock()

	err := tui.WriteClipboardText(url)
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
