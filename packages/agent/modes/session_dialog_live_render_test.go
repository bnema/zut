package modes

import (
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
	"github.com/mattn/go-runewidth"
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

func TestLiveRowsAlignedAndTruncated(t *testing.T) {
	d := &sessionDialog{liveFocus: true, live: []session.Info{{ID: "private-id", CWD: "/a/界", Title: "First"}, {ID: "other-id", CWD: "/a/long-project", Title: "Second"}}}
	rows := d.renderLive(tui.Theme{}, 200)
	first, second := stripANSIBytes(rows[1]), stripANSIBytes(rows[2])
	require.Equal(t, runewidth.StringWidth(strings.Split(first, "○ idle")[0]), runewidth.StringWidth(strings.Split(second, "○ idle")[0]))
	require.Contains(t, first, "▸")
	require.NotContains(t, first, "private-id")
	require.Equal(t, "", rows[3])
	require.Contains(t, stripANSIBytes(rows[4]), "resumable sessions")
	for _, width := range []int{1, 10, 40} {
		for _, row := range d.renderLive(tui.Theme{}, width)[1:3] {
			plain := stripANSIBytes(row)
			require.LessOrEqual(t, runewidth.StringWidth(plain), width)
			require.Contains(t, plain, "…")
		}
	}
}

func TestResumableRowProject(t *testing.T) {
	require.Contains(t, formatSessionRowPlain(core.SessionSummary{CWD: "/work/my-project", Title: "Saved"}, 200), "my-project")
}

func TestLiveColumnsHaveFixedOffsets(t *testing.T) {
	d := &sessionDialog{live: []session.Info{
		{CWD: "/a/project", Run: session.RunStateIdle, Model: "short", Title: "TITLE"},
		{CWD: "/a/very-long-project-name", Run: session.RunStateRunning, Activity: session.Activity{Tool: "very-long-tool-name"}, Model: "very-long-model-name-that-overflows", Title: "TITLE"},
	}}
	rows := d.renderLive(tui.Theme{}, 180)
	for _, row := range rows[1:3] {
		plain := stripANSIBytes(row)
		prefix := strings.Split(plain, "TITLE")[0]
		require.Equal(t, 2+13+12+18+11+16+7+12, runewidth.StringWidth(prefix))
	}
	require.Contains(t, stripANSIBytes(rows[2]), "…")
}
