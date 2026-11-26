package modes

import (
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/provider"
	"github.com/stretchr/testify/require"
)

func TestLiveRowColumns(t *testing.T) {
	info := session.Info{ID: "self", RepoRoot: "/work/zut", CWD: "/work/zut/sub", Run: session.RunStateRunning, Activity: session.Activity{Kind: "tool", Tool: "bash"}, LastUserInputAt: time.Now().Add(-150 * time.Second), UpdatedAt: time.Now(), Model: "anthropic/claude-opus-5-5", Reasoning: "high", Usage: provider.Usage{CostUSD: 1.21}, Title: "A title"}
	require.Equal(t, []string{"this terminal", "zut", "● running · bash", "2 min ago", "opus-5-5:high", "$1.21", "A title"}, liveRowColumns(info, "self"))
	info.LastUserInputAt = time.Time{}
	require.Equal(t, "just now", liveRowColumns(info, "self")[3])
	info.Title = "bad\ntext\x1b[31m"
	require.NotContains(t, liveRowColumns(info, "self")[6], "\n")
	require.NotContains(t, liveRowColumns(info, "self")[6], "\x1b")
}

func TestLiveRowFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name         string
		info         session.Info
		state, title string
	}{
		{"empty", session.Info{}, "○ idle", "(new session)"},
		{"running", session.Info{Run: session.RunStateRunning}, "● running", "(new session)"},
		{"goal", session.Info{Goal: &session.GoalSummary{Objective: "Fix bug"}, PlanStep: &session.PlanStepSummary{Text: "Next"}}, "○ idle", "goal: Fix bug"},
		{"plan", session.Info{PlanStep: &session.PlanStepSummary{Text: "Next"}}, "○ idle", "Next"},
		{"residents", session.Info{Run: session.RunStateRunning, Activity: session.Activity{Kind: "idle"}, Residents: []session.ResidentSummary{{State: "running"}}}, "◌ waiting agents", "(new session)"},
		{"queued", session.Info{Run: session.RunStateIdle, Residents: []session.ResidentSummary{{State: "queued"}}}, "◌ waiting agents", "(new session)"},
		{"foreground", session.Info{Run: session.RunStateRunning, Activity: session.Activity{Kind: "thinking"}, Residents: []session.ResidentSummary{{State: "running"}}}, "● running", "(new session)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cols := liveRowColumns(tc.info, "")
			require.Equal(t, tc.state, cols[2])
			require.Equal(t, tc.title, cols[6])
		})
	}
	require.Equal(t, "repo", sessionProject("/a/repo", "/a/other"))
	require.Equal(t, "other", sessionProject("", "/a/other"))
	require.Equal(t, "-", sessionProject("", ""))
}
