package modes

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/require"

	"github.com/bnema/zut/packages/tui"
)

func TestDecideResume(t *testing.T) {
	for _, tc := range []struct {
		name   string
		plan   resumePlan
		direct bool
	}{
		{"same dir", resumePlan{TargetCWD: "/a/zut/", CurrentCWD: "/a/zut", CWDExists: true}, true},
		{"same dir live", resumePlan{TargetCWD: "/a/zut", CurrentCWD: "/a/zut", Live: true}, true},
		{"unknown origin", resumePlan{CurrentCWD: "/a/zut"}, true},
		{"different dir", resumePlan{TargetCWD: "/a/gordon", CurrentCWD: "/a/zut", CWDExists: true}, false},
		{"missing dir", resumePlan{TargetCWD: "/a/gone", CurrentCWD: "/a/zut"}, false},
		{"live", resumePlan{TargetCWD: "/a/gordon", CurrentCWD: "/a/zut", CWDExists: true, Live: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.direct, decideResume(tc.plan) == nil)
		})
	}
}

func TestResumeConfirmKeys(t *testing.T) {
	y := tui.Key{Kind: tui.KeyRune, Rune: 'y'}
	n := tui.Key{Kind: tui.KeyRune, Rune: 'N'}
	esc := tui.Key{Kind: tui.KeyEsc}
	other := tui.Key{Kind: tui.KeyRune, Rune: 'x'}
	for _, tc := range []struct {
		name           string
		plan           resumePlan
		y, n, esc, oth resumeChoice
	}{
		{"different dir", resumePlan{TargetCWD: "/a/g", CurrentCWD: "/a/z", CWDExists: true}, resumeChoiceSwitch, resumeChoiceFork, resumeChoiceCancel, resumeChoiceNone},
		{"missing dir", resumePlan{TargetCWD: "/a/g", CurrentCWD: "/a/z"}, resumeChoiceNone, resumeChoiceFork, resumeChoiceCancel, resumeChoiceNone},
		{"live", resumePlan{TargetCWD: "/a/g", CurrentCWD: "/a/z", CWDExists: true, Live: true}, resumeChoiceAttach, resumeChoiceNone, resumeChoiceCancel, resumeChoiceNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := decideResume(tc.plan)
			require.Equal(t, tc.y, c.HandleKey(y))
			require.Equal(t, tc.n, c.HandleKey(n))
			require.Equal(t, tc.esc, c.HandleKey(esc))
			require.Equal(t, tc.oth, c.HandleKey(other))
			require.Equal(t, resumeChoiceCancel, c.HandleKey(tui.Key{Kind: tui.KeyCtrlC}))
		})
	}
}

func TestResumeConfirmRender(t *testing.T) {
	long := "/a/" + strings.Repeat("very-long-directory-name/", 6) + "日本語プロジェクト"
	plans := map[string]resumePlan{
		"different": {TargetCWD: "/a/gordon", CurrentCWD: "/a/zut", CWDExists: true},
		"missing":   {TargetCWD: "/a/gone", CurrentCWD: "/a/zut"},
		"live":      {TargetCWD: "/a/gordon", CurrentCWD: "/a/zut", CWDExists: true, Live: true},
		"long":      {TargetCWD: long, CurrentCWD: "/a/z\x1b[31m", CWDExists: true},
	}
	for name, plan := range plans {
		for _, width := range []int{20, 40, 60, 100} {
			lines := decideResume(plan).Render(tui.Dark, width)
			for idx, line := range lines {
				require.Equal(t, width, runewidth.StringWidth(stripANSIBytes(line)), "%s width %d line %d: %q", name, width, idx, stripANSIBytes(line))
			}
			require.NotContains(t, strings.Join(lines, ""), "\x1b[31m")
		}
	}

	text := func(plan resumePlan) string {
		return stripANSIBytes(strings.Join(decideResume(plan).Render(tui.Dark, 100), "\n"))
	}
	got := text(plans["different"])
	require.Contains(t, got, "Resume session")
	require.Contains(t, got, "This session ran in /a/gordon")
	require.Contains(t, got, "You are in          /a/zut")
	require.Contains(t, got, "[y] switch to /a/gordon and resume")
	require.Contains(t, got, "fork into /a/zut")
	require.Contains(t, got, "new session")
	require.Contains(t, got, "original stays untouched")
	require.Contains(t, got, "esc cancel")

	got = text(plans["missing"])
	require.Contains(t, got, "original directory no longer exists")
	require.NotContains(t, got, "[y]")
	require.Contains(t, got, "[n] fork into")

	got = text(plans["live"])
	require.Contains(t, got, "[y] attach (runs in /a/gordon)")
	require.NotContains(t, got, "[n]")
	require.NotContains(t, got, "fork")

	got = text(plans["long"])
	require.Contains(t, got, "日本語")
}

func TestFinderResumeWiring(t *testing.T) {
	i := NewInteractive(InteractiveConfig{Theme: tui.Dark, Terminal: &alertTestTerminal{}})
	i.openFinderResume(resumePlan{TargetCWD: "/a", CurrentCWD: "/a"})
	require.False(t, i.finderResume.Active())
	require.Equal(t, "session loading is not wired in this build", i.statusErr)

	i.statusErr = ""
	i.openFinderResume(resumePlan{TargetCWD: "/a/g", CurrentCWD: "/a/z", Live: true})
	require.True(t, i.finderResume.Active())
	require.True(t, i.confirmChildActive())
	require.True(t, i.sessionTreeEscapeBlocked())

	i.handleKey(t.Context(), tui.Key{Kind: tui.KeyRune, Rune: 'n'})
	require.True(t, i.finderResume.Active(), "fork is not offered for live sessions")
	i.handleKey(t.Context(), tui.Key{Kind: tui.KeyEsc})
	require.False(t, i.finderResume.Active())
	require.Empty(t, i.statusErr)

	i.openFinderResume(resumePlan{TargetCWD: "/a/g", CurrentCWD: "/a/z", Live: true})
	i.handleKey(t.Context(), tui.Key{Kind: tui.KeyRune, Rune: 'y'})
	require.False(t, i.finderResume.Active())
	require.Equal(t, "finder: attaching a live session needs the daemon", i.statusErr)
}
