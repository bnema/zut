package modes

import (
	"strings"

	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/bnema/zut/packages/tui"
)

// Notify is the manager's NotifyFromExt entry point.
func (i *Interactive) Notify(extName, level, message string) {
	i.appendExtensionNote(extName, message, level)
	i.invalidate()
}

// Alert is the manager's AlertFromExt entry point. Alerts are emitted
// through the same terminal writer as redraws while holding the TUI mutex,
// so a BEL cannot be interleaved with a frame update.
func (i *Interactive) Alert(_ string, alert extproto.AlertRequest) {
	i.mu.Lock()
	i.emitAlertLocked(alert)
	i.mu.Unlock()
}

func terminalAlertsEnabled(enabled *bool) bool {
	return enabled == nil || *enabled
}

// emitAlertLocked applies the shared terminal-alert policy. The caller must
// hold i.mu because terminal writes share the renderer's output boundary.
func (i *Interactive) emitAlertLocked(alert extproto.AlertRequest) {
	if alert.Kind != extproto.AlertKindBell || !terminalAlertsEnabled(i.cfg.TerminalAlertsEnabled) || i.cfg.Terminal == nil {
		return
	}
	_ = tui.WriteBell(i.cfg.Terminal)
}

// scheduleMainAlert defers a main-session alert until the next redraw
// commits the final frame. Keeping this deferred even when no paced text is
// pending avoids racing the pacer's final state transition.
func (i *Interactive) scheduleMainAlert(reason string) {
	if reason == "" {
		return
	}
	i.mu.Lock()
	i.pendingAlert = &extproto.AlertRequest{Kind: extproto.AlertKindBell, Reason: reason}
	i.mu.Unlock()
	i.invalidate()
}

// ClearNotes removes every note line owned by extName from the
// bottom-sticky ext-notes block. Extensions use this to retract a
// transient status line (e.g. an approval prompt) once it no longer
// applies, instead of leaving it stacked forever. Notes from other
// extensions and internal notes (auto-compact) are left untouched.
func (i *Interactive) ClearNotes(extName string) {
	marker := "[" + extName + "] "
	i.mu.Lock()
	if len(i.extNotes) == 0 {
		i.mu.Unlock()
		return
	}
	kept := i.extNotes[:0:0]
	changed := false
	for _, line := range i.extNotes {
		if strings.Contains(line, marker) {
			changed = true
			continue
		}
		kept = append(kept, line)
	}
	if changed {
		i.extNotes = kept
	}
	i.mu.Unlock()
	if changed {
		i.invalidate()
	}
}
