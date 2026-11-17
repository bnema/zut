package modes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestParseAdvisorArgs(t *testing.T) {
	on, off := true, false
	spec := "anthropic/claude-opus-4-7:high"
	for _, tc := range []struct {
		name  string
		args  []string
		want  *session.AdvisorChange
		usage bool
	}{
		{"status", nil, nil, false},
		{"on", []string{"on"}, &session.AdvisorChange{Enabled: &on}, false},
		{"OFF", []string{"OFF"}, &session.AdvisorChange{Enabled: &off}, false},
		{"spec", []string{spec}, &session.AdvisorChange{Model: &spec}, false},
		{"bare word", []string{"opus"}, nil, true},
		{"extra args", []string{"on", "now"}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, usage := parseAdvisorArgs(tc.args)
			require.Equal(t, tc.usage, usage != "")
			if tc.usage {
				require.Contains(t, usage, "/advisor provider/model:reasoning | on | off")
			}
			require.Equal(t, tc.want, got)
		})
	}
}

func TestAdvisorSlashStatus(t *testing.T) {
	i, _ := newPortProfileInteractive(t, profileInfo(0, nil))
	i.runSlash(context.Background(), "/advisor")
	require.Contains(t, profStatusErr(i), "advisor status unavailable (check config.json)")

	info := profileInfo(0, nil)
	info.Advisor = &session.AdvisorStatus{Enabled: true, Model: "anthropic/claude-opus-4-7:high", Mode: "auto"}
	profSetInfo(i, info)
	i.runSlash(context.Background(), "/advisor")
	i.mu.Lock()
	ok, errText := i.statusOK, i.statusErr
	i.mu.Unlock()
	require.Empty(t, errText)
	require.Equal(t, "advisor on · anthropic/claude-opus-4-7:high · mode auto", ok)

	info.Advisor.Effective = "native"
	profSetInfo(i, info)
	i.runSlash(context.Background(), "/advisor")
	i.mu.Lock()
	ok = i.statusOK
	i.mu.Unlock()
	require.Equal(t, "advisor on · anthropic/claude-opus-4-7:high · mode auto (native; reasoning set by the provider)", ok)
}

func TestAdvisorSlashUsageError(t *testing.T) {
	i, _ := newPortProfileInteractive(t, profileInfo(0, nil))
	i.runSlash(context.Background(), "/advisor opus")
	require.Equal(t, advisorUsage, profStatusErr(i))
}

func waitAdvisorStatus(t *testing.T, i *Interactive, done func(ok, errText string) bool) (string, string) {
	t.Helper()
	var ok, errText string
	require.Eventually(t, func() bool {
		i.mu.Lock()
		ok, errText = i.statusOK, i.statusErr
		i.mu.Unlock()
		return done(ok, errText)
	}, 5*time.Second, time.Millisecond)
	return ok, errText
}

func TestAdvisorSlashSetModelAndToggle(t *testing.T) {
	i, port := newPortProfileInteractive(t, profileInfo(0, nil))
	spec := "anthropic/claude-opus-4-7:high"
	port.EXPECT().SetAdvisor(mock.Anything, session.AdvisorChange{Model: &spec}).
		Return(session.AdvisorStatus{Enabled: true, Model: spec, Mode: "auto"}, nil).Once()
	i.runSlash(context.Background(), "/advisor "+spec)
	ok, _ := waitAdvisorStatus(t, i, func(ok, _ string) bool { return ok != "" })
	require.Equal(t, "advisor on · "+spec+" · mode auto", ok)

	on := true
	port.EXPECT().SetAdvisor(mock.Anything, session.AdvisorChange{Enabled: &on}).
		Return(session.AdvisorStatus{}, session.ErrBusy).Once()
	i.runSlash(context.Background(), "/advisor on")
	_, errText := waitAdvisorStatus(t, i, func(_, e string) bool { return e != "" })
	require.Contains(t, errText, "while a turn is running")

	off := false
	port.EXPECT().SetAdvisor(mock.Anything, session.AdvisorChange{Enabled: &off}).
		Return(session.AdvisorStatus{}, context.DeadlineExceeded).Once()
	i.mu.Lock()
	i.statusOK, i.statusErr = "", ""
	i.mu.Unlock()
	i.runSlash(context.Background(), "/advisor off")
	_, errText = waitAdvisorStatus(t, i, func(_, e string) bool { return e != "" })
	require.True(t, strings.HasPrefix(errText, "advisor: "), errText)
}

func TestAdvisorSlashDoesNotCancelTurn(t *testing.T) {
	require.False(t, slashCommandCancelsTurn("/advisor on"))
}

func TestNewBtwAgentDropsAdvisor(t *testing.T) {
	main := core.NewAgent(nil, "model", "system", core.Registry{tools.AdvisorToolName: &tools.AdvisorTool{}, "plan": &tools.PlanTool{}})
	side := newBtwAgent(main, "system", "model", "btw")
	reg := side.ToolsSnapshot()
	require.NotContains(t, reg, tools.AdvisorToolName)
	require.Contains(t, reg, "plan")
	require.Contains(t, main.ToolsSnapshot(), tools.AdvisorToolName)
}

func TestAdvisorSlashOldDaemon(t *testing.T) {
	i, port := newPortProfileInteractive(t, profileInfo(0, nil))
	on := true
	port.EXPECT().SetAdvisor(mock.Anything, session.AdvisorChange{Enabled: &on}).
		Return(session.AdvisorStatus{}, errors.New(`unsupported port method "set_advisor"; run "zut daemon restart"`)).Once()
	i.runSlash(context.Background(), "/advisor on")
	_, errText := waitAdvisorStatus(t, i, func(_, e string) bool { return e != "" })
	require.Equal(t, "advisor: restart the daemon (zut daemon restart) to use /advisor", errText)
}
