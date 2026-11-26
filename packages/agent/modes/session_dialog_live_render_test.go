package modes

import (
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/tui"
	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/require"
)

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
			if width < 20 {
				require.Contains(t, plain, "…")
			}
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

func TestLiveTitleVisibleAtCommonWidths(t *testing.T) {
	d := &sessionDialog{live: []session.Info{{CWD: "/a/zut", Model: "anthropic/claude-opus-5-5", Reasoning: "high", Title: "Fix nil auth"}}}
	for _, width := range []int{60, 80, 94, 130} {
		plain := stripANSIBytes(d.renderLive(tui.Theme{}, width)[1])
		require.Contains(t, plain, "Fix nil auth", "width %d", width)
		require.Contains(t, plain, "zut", "width %d", width)
		require.LessOrEqual(t, runewidth.StringWidth(plain), width)
	}
}
