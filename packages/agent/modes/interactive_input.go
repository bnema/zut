package modes

import (
	"strings"
	"time"

	"github.com/bnema/zut/packages/tui"
)

// prepareInputKey applies main-editor key state that must happen before
// routing the key to a dialog, suggestion, or editor. It returns false when
// clipboard resolution failed and the key must not be delivered further.
func (i *Interactive) prepareInputKey(k tui.Key) (tui.Key, bool) {
	// A bare Escape is eligible for the session-tree gesture only while the
	// main input owns the key. Any other key, modified Escape, or visible
	// child interaction resets the pending first tap before normal routing.
	if !isUnmodifiedEscape(k) || i.sessionTreeEscapeBlocked() {
		i.mu.Lock()
		i.doubleEscape.Reset()
		i.mu.Unlock()
	}

	// Dialogs route keys before the main clipboard handler below. Resolve text
	// here when a child interaction owns input so every editor and filter sees
	// the same KeyPaste event as terminal-native bracketed paste. Main chat is
	// left image-first to preserve macOS clipboard image attachments.
	if k.Kind == tui.KeyPasteClipboard && i.confirmChildActive() {
		resolved, pastedText, err := resolveClipboardText(k, tui.ReadClipboardText)
		if err != nil {
			i.mu.Lock()
			i.statusErr = "clipboard paste failed: " + err.Error()
			i.statusOK = ""
			i.mu.Unlock()
			return tui.Key{}, false
		}
		if pastedText {
			k = resolved
			i.mu.Lock()
			i.statusErr = ""
			i.statusOK = ""
			i.mu.Unlock()
		}
	}

	// Any key that isn't ctrl+c invalidates an armed ctrl+c-exit, so pressing
	// ctrl+c then typing then ctrl+c much later doesn't quit unexpectedly. The
	// hint message also goes stale; clear it.
	if k.Kind != tui.KeyCtrlC {
		i.mu.Lock()
		if !i.lastCtrlC.IsZero() {
			i.lastCtrlC = time.Time{}
			if strings.HasPrefix(i.statusOK, "input cleared") || strings.HasPrefix(i.statusOK, "press ctrl+c") {
				i.statusOK = ""
			}
		}
		i.mu.Unlock()
	}
	return k, true
}
