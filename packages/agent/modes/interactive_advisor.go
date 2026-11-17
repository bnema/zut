package modes

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/agent/session"
)

const advisorUsage = "usage: /advisor provider/model:reasoning | on | off"

// parseAdvisorArgs maps /advisor arguments to a change. A nil change with an
// empty usage means "show status"; a non-empty usage reports invalid input.
func parseAdvisorArgs(args []string) (change *session.AdvisorChange, usage string) {
	if len(args) == 0 {
		return nil, ""
	}
	if len(args) > 1 {
		return nil, advisorUsage
	}
	arg := args[0]
	switch strings.ToLower(arg) {
	case "on", "off":
		enabled := strings.EqualFold(arg, "on")
		return &session.AdvisorChange{Enabled: &enabled}, ""
	}
	if !strings.Contains(arg, "/") {
		return nil, advisorUsage
	}
	return &session.AdvisorChange{Model: &arg}, ""
}

// formatAdvisorStatus renders an advisor status line.
func formatAdvisorStatus(s *session.AdvisorStatus) string {
	if s == nil {
		return "advisor status unavailable (check config.json)"
	}
	state := "off"
	if s.Enabled {
		state = "on"
	}
	model := s.Model
	if model == "" {
		model = "no model"
	}
	line := fmt.Sprintf("advisor %s · %s · mode %s", state, model, s.Mode)
	switch s.Effective {
	case session.AdvisorEffectiveNative:
		line += " (native; reasoning set by the provider)"
	case session.AdvisorEffectiveClient:
		line += " (client)"
	}
	return line
}

// runAdvisorCommand handles /advisor [spec|on|off].
func (i *Interactive) runAdvisorCommand(args []string) {
	change, usage := parseAdvisorArgs(args)
	setStatus := func(ok, errText string) {
		i.mu.Lock()
		i.statusOK, i.statusErr = ok, errText
		i.mu.Unlock()
		i.invalidate()
	}
	switch {
	case usage != "":
		setStatus("", usage)
		return
	case !i.portMode.Load():
		setStatus("", "advisor: requires a daemon session")
		return
	case change == nil:
		if info := i.SessionInfo(); info.Advisor != nil {
			setStatus(formatAdvisorStatus(info.Advisor), "")
		} else {
			setStatus("", formatAdvisorStatus(nil))
		}
		return
	}
	i.portSettingsCall("advisor", func(ctx context.Context, port session.Port) error {
		status, err := port.SetAdvisor(ctx, *change)
		switch {
		case errors.Is(err, session.ErrBusy):
			return errors.New("cannot change settings while a turn is running; try again when idle")
		case err != nil && strings.Contains(err.Error(), daemonproto.UnsupportedPortMethod):
			return errors.New("restart the daemon (zut daemon restart) to use /advisor")
		case err != nil:
			return err
		}
		setStatus(formatAdvisorStatus(&status), "")
		return nil
	})
}
