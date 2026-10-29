package modes

import (
	"context"
	"fmt"
	"sync"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/tui"
)

// Deferred action resolutions accepted by session.Port.ResolveDeferred.
const (
	deferredDismiss = "dismiss"
	deferredSend    = "send_to_agent"
)

// deferredDialog lists the actions recorded while nobody was attached. Each
// can be dismissed or sent back to the agent; Esc closes the dialog and
// leaves the remaining actions pending.
type deferredDialog struct {
	mu      sync.Mutex
	actions []session.DeferredAction
	cursor  int
	resolve func(id, action string)
}

// Open shows actions; resolve is called for each user decision.
func (d *deferredDialog) Open(actions []session.DeferredAction, resolve func(id, action string)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.actions = append([]session.DeferredAction(nil), actions...)
	d.cursor = 0
	d.resolve = resolve
}

func (d *deferredDialog) Active() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.actions) > 0
}

func (d *deferredDialog) HandleKey(k tui.Key) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.actions) == 0 {
		return
	}
	pick := ""
	switch {
	case k.Kind == tui.KeyEsc || k.Kind == tui.KeyCtrlC:
		d.actions = nil
		return
	case k.Kind == tui.KeyUp || (k.Kind == tui.KeyRune && k.Rune == 'k'):
		d.cursor = max(0, d.cursor-1)
	case k.Kind == tui.KeyDown || (k.Kind == tui.KeyRune && k.Rune == 'j'):
		d.cursor = min(len(d.actions)-1, d.cursor+1)
	case k.Kind == tui.KeyRune && k.Rune == 'd':
		pick = deferredDismiss
	case k.Kind == tui.KeyEnter || (k.Kind == tui.KeyRune && k.Rune == 's'):
		pick = deferredSend
	}
	if pick == "" {
		return
	}
	id := d.actions[d.cursor].ID
	d.actions = append(d.actions[:d.cursor:d.cursor], d.actions[d.cursor+1:]...)
	d.cursor = min(d.cursor, max(0, len(d.actions)-1))
	if d.resolve != nil {
		d.resolve(id, pick)
	}
}

func (d *deferredDialog) Render(th tui.Theme, width int) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.actions) == 0 {
		return nil
	}
	width = max(1, width)
	lines := []string{frameHeader(th, fmt.Sprintf("Deferred actions (%d)", len(d.actions)), width)}
	for n, a := range d.actions {
		label := a.Summary
		if a.Tool != "" {
			label = a.Tool + ": " + label
		}
		if label == "" {
			label = a.Kind
		}
		for _, row := range tui.WrapANSILine(stripANSIBytes(label), width) {
			if n == d.cursor {
				row = th.PadHighlight(row, width)
			}
			lines = append(lines, row)
		}
	}
	lines = append(lines, "", th.FGColor(th.Muted, "↑/↓ j/k move · Enter/s send to agent · d dismiss · Esc close (keep pending)"))
	return lines
}

// openDeferredDialog shows the snapshot's deferred actions after attach.
// dropDeferred removes a resolved action from the local projection.
func (i *Interactive) dropDeferred(id string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	kept := i.portDeferred[:0]
	for _, d := range i.portDeferred {
		if d.ID != id {
			kept = append(kept, d)
		}
	}
	i.portDeferred = kept
}

// No-op outside Port mode or when nothing is deferred.
func (i *Interactive) openDeferredDialog() {
	actions := i.DeferredActions()
	if len(actions) == 0 || i.SessionPort() == nil {
		return
	}
	i.deferredDialog.Open(actions, func(id, action string) {
		i.portSettingsCall("deferred "+action, func(ctx context.Context, port session.Port) error {
			if err := port.ResolveDeferred(ctx, id, action); err != nil {
				return err
			}
			i.dropDeferred(id)
			return nil
		})
	})
	i.invalidate()
}
