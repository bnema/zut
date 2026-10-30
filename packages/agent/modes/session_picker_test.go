package modes

import (
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/tui"
	"github.com/stretchr/testify/require"
)

func pickerForTest() *sessionDialog {
	rows := []core.SessionSummary{{Path: "/disk/one", Title: "disk-one"}, {Path: "/disk/two", Title: "disk-two"}}
	d := &sessionDialog{active: true, sessions: rows, baseSessions: rows}
	d.SetLive([]session.Info{{ID: "a"}, {ID: "b"}}, "a")
	return d
}

func TestSessionPickerHighlightMatchesEnter(t *testing.T) {
	for _, live := range []bool{true, false} {
		t.Run(map[bool]string{true: "live", false: "disk"}[live], func(t *testing.T) {
			d := pickerForTest()
			d.liveFocus = live
			lines := d.Render(tui.Dark, 100)
			var selected []string
			for _, line := range lines {
				if strings.Contains(line, tui.Dark.SelectionStyle()) {
					selected = append(selected, line)
				}
			}
			require.Len(t, selected, 1)
			act := d.HandleKey(tui.Key{Kind: tui.KeyEnter})
			require.True(t, act.Select)
			if live {
				require.Equal(t, session.ID("a"), act.LiveID)
				require.Contains(t, selected[0], "a")
			} else {
				require.Equal(t, "/disk/one", act.Path)
				require.Contains(t, selected[0], "disk-one")
			}
		})
	}
}

func TestSessionPickerSearchSelectsDisk(t *testing.T) {
	for _, key := range []tui.Key{{Kind: tui.KeyRune, Rune: 't'}, {Kind: tui.KeyPaste, Paste: "two"}} {
		d := pickerForTest()
		d.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: '/'})
		require.False(t, d.liveFocus)
		// Typing must also restore disk focus if it was changed in the meantime.
		d.liveFocus = true
		d.HandleKey(key)
		require.False(t, d.liveFocus)
		d.query = "two"
		d.searchReady = true
		d.searchMatches = map[string]sessionSearchMatch{"/disk/two": {count: 1}}
		d.applySearchFilter()
		d.HandleKey(tui.Key{Kind: tui.KeyUp}) // Search navigation stays among results.
		act := d.HandleKey(tui.Key{Kind: tui.KeyEnter})
		require.Equal(t, "/disk/two", act.Path)
		require.Empty(t, act.LiveID)
	}
}

func TestSessionPickerLiveUpdatesPreserveSelection(t *testing.T) {
	d := pickerForTest()
	d.HandleKey(tui.Key{Kind: tui.KeyDown})
	d.SetLive([]session.Info{{ID: "b"}, {ID: "a"}}, "a")
	require.True(t, d.liveFocus)
	require.Equal(t, "b", d.live[d.liveCursor].ID)
	d.SetLive([]session.Info{{ID: "a"}}, "a")
	require.Equal(t, 0, d.liveCursor)
	d.HandleKey(tui.Key{Kind: tui.KeyDown})
	d.HandleKey(tui.Key{Kind: tui.KeyDown})
	require.False(t, d.liveFocus)
	require.Equal(t, 1, d.cursor)
	d.SetLive([]session.Info{{ID: "c"}}, "a")
	require.False(t, d.liveFocus)
	require.Equal(t, 1, d.cursor)
}

func TestSessionPickerLateInitialLiveDoesNotStealFocus(t *testing.T) {
	d := &sessionDialog{active: true, sessions: []core.SessionSummary{{Path: "one"}, {Path: "two"}}}
	d.HandleKey(tui.Key{Kind: tui.KeyDown})
	d.SetLive([]session.Info{{ID: "a"}}, "a")
	require.False(t, d.liveFocus)
	require.Equal(t, 1, d.cursor)
}
