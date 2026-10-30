package modes

import (
	"context"
	"errors"
	"fmt"

	"github.com/bnema/zut/packages/agent/session"
)

// errProfilesUnsupported reports a daemon whose Info carries no profile slots.
var errProfilesUnsupported = errors.New("model profiles need a newer zut daemon; run \"zut daemon restart\"")

// portProfiles returns the daemon's slots, reporting an old daemon on the
// status line. Call on the Run loop.
func (i *Interactive) portProfiles() ([]session.QuickModelShortcut, bool) {
	info := i.SessionInfo()
	if len(info.Profiles) == 0 {
		i.ReportError(errProfilesUnsupported)
		return nil, false
	}
	return info.Profiles, true
}

// portBusy guards model switches with the daemon's run state.
func (i *Interactive) portBusy() bool {
	run := i.SessionInfo().Run
	if run != session.RunStateRunning && run != session.RunStateCompacting {
		return false
	}
	i.ReportError(errors.New("cannot switch model while a turn is running"))
	return true
}

// portApplyProfile activates slot through the daemon. An empty slot (in Info,
// or cleared elsewhere meanwhile) opens the picker to assign and activate it.
func (i *Interactive) portApplyProfile(slot int) {
	profiles, ok := i.portProfiles()
	if !ok || i.portBusy() {
		return
	}
	if slot > len(profiles) || profiles[slot-1].Provider == "" && profiles[slot-1].Model == "" {
		i.openActivatingProfilePicker(slot)
		return
	}
	i.portSettingsCall(fmt.Sprintf("profile %d", slot), func(ctx context.Context, port session.Port) error {
		err := port.ApplyProfile(ctx, slot)
		if errors.Is(err, session.ErrProfileEmpty) {
			i.onLoop(ctx, func() { i.openActivatingProfilePicker(slot) })
			return nil
		}
		return err
	})
}

// openActivatingProfilePicker opens the model picker in assign+activate mode.
func (i *Interactive) openActivatingProfilePicker(slot int) {
	i.openQuickModelPicker(slot)
	i.quickModelActivate = true
	i.mu.Lock()
	i.statusErr = ""
	i.statusOK = fmt.Sprintf("choose a model for profile %d; Enter saves and activates", slot)
	i.mu.Unlock()
	i.invalidate()
}

// portAssignProfile stores a picker selection in slot; the activating picker
// also switches to it.
func (i *Interactive) portAssignProfile(slot int, providerName, model string) {
	activate := i.quickModelActivate
	i.quickModelActivate = false
	if _, ok := i.portProfiles(); !ok {
		return
	}
	if activate && i.portBusy() {
		return
	}
	fastMode := i.cfg.FastMode != nil && *i.cfg.FastMode
	if slot <= len(i.cfg.QuickModelShortcuts) && i.cfg.QuickModelShortcuts[slot-1].Model != "" {
		fastMode = i.cfg.QuickModelShortcuts[slot-1].FastMode
	}
	p := session.QuickModelShortcut{Provider: providerName, Model: model, Reasoning: i.modelDialog.reasoning, FastMode: fastMode}
	i.portSettingsCall(fmt.Sprintf("profile %d", slot), func(ctx context.Context, port session.Port) error {
		return port.AssignProfile(ctx, slot, p, activate)
	})
}

// portClearProfile empties slot through the daemon.
func (i *Interactive) portClearProfile(slot int) {
	if _, ok := i.portProfiles(); !ok {
		return
	}
	i.portSettingsCall(fmt.Sprintf("profile %d", slot), func(ctx context.Context, port session.Port) error {
		return port.ClearProfile(ctx, slot)
	})
}

// onLoop runs fn on the Run loop, which owns the dialogs.
func (i *Interactive) onLoop(ctx context.Context, fn func()) {
	select {
	case i.loopCalls <- fn:
	case <-ctx.Done():
	}
}
