package modes

import (
	"errors"
	"testing"

	"github.com/bnema/zut/packages/tui"
	"github.com/stretchr/testify/require"
)

func newTestFinderSource(t *testing.T, id string) *mockFinderSource {
	src := newMockFinderSource(t)
	src.EXPECT().ID().Return(id).Maybe()
	src.EXPECT().Title().Return(id).Maybe()
	return src
}

func finderRunes(m *finderModel, s string) finderAction {
	var act finderAction
	for _, r := range s {
		act = m.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: r})
	}
	return act
}

func openFinderWithItems(t *testing.T, items ...finderItem) *finderModel {
	t.Helper()
	m := newFinderModel("/work", newTestFinderSource(t, "sessions"))
	act := m.Open(0)
	require.True(t, m.ApplyResults(act.Gen, act.TabID, finderResults{Items: items}, nil))
	return m
}

func TestFinderOpenRequestsSearch(t *testing.T) {
	m := newFinderModel("/work", newTestFinderSource(t, "sessions"))
	act := m.Open(0)
	require.True(t, m.Active())
	require.Equal(t, finderActionSearch, act.Kind)
	require.Equal(t, "sessions", act.TabID)
	require.Equal(t, finderQuery{CWD: "/work"}, act.Query)
	require.Equal(t, m.Generation(), act.Gen)
}

func TestFinderQueryEditing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		keys   []tui.Key
		want   string
		search bool
	}{
		{"insert", []tui.Key{{Kind: tui.KeyRune, Rune: '!'}}, "foo bar!", true},
		{"backspace", []tui.Key{{Kind: tui.KeyBackspace}}, "foo ba", true},
		{"ctrl+w", []tui.Key{{Kind: tui.KeyCtrlW}}, "foo ", true},
		{"ctrl+u", []tui.Key{{Kind: tui.KeyCtrlU}}, "", true},
		{"left insert", []tui.Key{{Kind: tui.KeyLeft}, {Kind: tui.KeyRune, Rune: 'X'}}, "foo baXr", true},
		{"home insert", []tui.Key{{Kind: tui.KeyHome}, {Kind: tui.KeyRune, Rune: 'X'}}, "Xfoo bar", true},
		{"home end insert", []tui.Key{{Kind: tui.KeyHome}, {Kind: tui.KeyEnd}, {Kind: tui.KeyRune, Rune: 'X'}}, "foo barX", true},
		{"home backspace noop", []tui.Key{{Kind: tui.KeyHome}, {Kind: tui.KeyBackspace}}, "foo bar", false},
		{"paste newline", []tui.Key{{Kind: tui.KeyPaste, Paste: "a\nb"}}, "foo bara b", true},
		{"right only", []tui.Key{{Kind: tui.KeyRight}}, "foo bar", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newFinderModel("/work", newTestFinderSource(t, "sessions"))
			m.Open(0)
			finderRunes(m, "foo bar")
			var act finderAction
			for _, k := range tc.keys {
				act = m.HandleKey(k)
			}
			require.Equal(t, tc.want, m.Query())
			if tc.search {
				require.Equal(t, finderActionSearch, act.Kind)
				require.Equal(t, tc.want, act.Query.Text)
				require.Equal(t, m.Generation(), act.Gen)
			} else {
				require.Equal(t, finderActionNone, act.Kind)
			}
		})
	}
}

func TestFinderQueryHereToken(t *testing.T) {
	m := newFinderModel("/work", newTestFinderSource(t, "sessions"))
	m.Open(0)
	act := finderRunes(m, "bug @here")
	require.Equal(t, finderQuery{Text: "bug @here", Here: true, CWD: "/work"}, act.Query)
}

func TestFinderApplyResultsDropsStale(t *testing.T) {
	m := newFinderModel("/work", newTestFinderSource(t, "sessions"), newTestFinderSource(t, "messages"))
	first := m.Open(0)
	second := finderRunes(m, "x")
	require.False(t, m.ApplyResults(first.Gen, first.TabID, finderResults{Items: []finderItem{{Key: "old"}}}, nil))
	require.Empty(t, m.items)
	require.True(t, m.loading)
	require.False(t, m.ApplyResults(second.Gen, "messages", finderResults{Items: []finderItem{{Key: "x"}}}, nil))
	require.True(t, m.ApplyResults(second.Gen, second.TabID, finderResults{Items: []finderItem{{Key: "new"}}, Partial: true}, nil))
	require.False(t, m.loading)
	require.True(t, m.partial)
	require.Equal(t, "new", m.items[0].Key)

	m.Close()
	require.False(t, m.ApplyResults(second.Gen, second.TabID, finderResults{}, nil))
}

func TestFinderApplyResultsError(t *testing.T) {
	m := newFinderModel("/work", newTestFinderSource(t, "sessions"))
	act := m.Open(0)
	require.True(t, m.ApplyResults(act.Gen, act.TabID, finderResults{}, errors.New("boom")))
	require.Equal(t, "boom", m.err)
	require.False(t, m.loading)
	finderRunes(m, "a")
	require.Empty(t, m.err)
	require.True(t, m.loading)
}

func TestFinderApplyResultsKeepsCursorKey(t *testing.T) {
	m := openFinderWithItems(t, finderItem{Key: "a"}, finderItem{Key: "b"}, finderItem{Key: "c"})
	m.HandleKey(tui.Key{Kind: tui.KeyDown})
	m.HandleKey(tui.Key{Kind: tui.KeyDown})
	act := finderRunes(m, "q")
	require.True(t, m.ApplyResults(act.Gen, act.TabID, finderResults{Items: []finderItem{{Key: "c"}, {Key: "a"}}}, nil))
	require.Equal(t, 0, m.cursor)
	m.HandleKey(tui.Key{Kind: tui.KeyDown})
	act = finderRunes(m, "r")
	require.True(t, m.ApplyResults(act.Gen, act.TabID, finderResults{Items: []finderItem{{Key: "z"}, {Key: "y"}}}, nil))
	require.Equal(t, 0, m.cursor)
}

func TestFinderCursorAndHitNavigation(t *testing.T) {
	m := openFinderWithItems(t,
		finderItem{Key: "a", Hits: []finderHit{{Ref: 1}, {Ref: 2}, {Ref: 3}}},
		finderItem{Key: "b"},
	)
	m.HandleKey(tui.Key{Kind: tui.KeyUp})
	require.Equal(t, 0, m.cursor)
	m.HandleKey(tui.Key{Kind: tui.KeyCtrlN})
	m.HandleKey(tui.Key{Kind: tui.KeyCtrlN})
	m.HandleKey(tui.Key{Kind: tui.KeyCtrlN})
	require.Equal(t, 2, m.hit)
	m.HandleKey(tui.Key{Kind: tui.KeyCtrlP})
	act := m.HandleKey(tui.Key{Kind: tui.KeyEnter})
	require.Equal(t, finderActionOpen, act.Kind)
	require.Equal(t, "a", act.Item.Key)
	require.Equal(t, 1, act.Hit)

	m.HandleKey(tui.Key{Kind: tui.KeyDown})
	require.Equal(t, 0, m.hit)
	m.HandleKey(tui.Key{Kind: tui.KeyDown})
	require.Equal(t, 1, m.cursor)
	m.HandleKey(tui.Key{Kind: tui.KeyCtrlN})
	act = m.HandleKey(tui.Key{Kind: tui.KeyEnter})
	require.Equal(t, "b", act.Item.Key)
	require.Equal(t, -1, act.Hit)

	require.Equal(t, finderActionNone, m.HandleKey(tui.Key{Kind: tui.KeyCtrlR}).Kind)
	require.True(t, m.renaming)
	act = m.HandleKey(tui.Key{Kind: tui.KeyEnter})
	require.Equal(t, finderActionRename, act.Kind)
	require.Equal(t, "b", act.Item.Key)
}

func TestFinderEmptyListKeysAreNoops(t *testing.T) {
	m := openFinderWithItems(t)
	for _, k := range []tui.KeyKind{tui.KeyEnter, tui.KeyCtrlR, tui.KeyCtrlD, tui.KeyUp, tui.KeyDown, tui.KeyCtrlN, tui.KeyCtrlP} {
		require.Equal(t, finderActionNone, m.HandleKey(tui.Key{Kind: k}).Kind)
	}
	require.False(t, m.confirmDelete)
	require.False(t, m.renaming)
}

func TestFinderDeleteConfirm(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tui.Key
		want finderActionKind
	}{
		{"yes", tui.Key{Kind: tui.KeyRune, Rune: 'y'}, finderActionDelete},
		{"other rune", tui.Key{Kind: tui.KeyRune, Rune: 'n'}, finderActionNone},
		{"esc cancels without closing", tui.Key{Kind: tui.KeyEsc}, finderActionNone},
		{"enter cancels", tui.Key{Kind: tui.KeyEnter}, finderActionNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := openFinderWithItems(t, finderItem{Key: "a"})
			require.Equal(t, finderActionNone, m.HandleKey(tui.Key{Kind: tui.KeyCtrlD}).Kind)
			require.True(t, m.confirmDelete)
			act := m.HandleKey(tc.key)
			require.Equal(t, tc.want, act.Kind)
			require.False(t, m.confirmDelete)
			require.True(t, m.Active())
			require.Empty(t, m.Query())
			if tc.want == finderActionDelete {
				require.Equal(t, "a", act.Item.Key)
			}
		})
	}
}

func TestFinderCloseKeys(t *testing.T) {
	for _, k := range []tui.KeyKind{tui.KeyEsc, tui.KeyCtrlC} {
		m := openFinderWithItems(t, finderItem{Key: "a"})
		finderRunes(m, "abc")
		gen := m.Generation()
		require.Equal(t, finderActionClose, m.HandleKey(tui.Key{Kind: k}).Kind)
		require.False(t, m.Active())
		require.Greater(t, m.Generation(), gen)
	}
}

func TestFinderTabSwitchSearchesSameQuery(t *testing.T) {
	m := newFinderModel("/work", newTestFinderSource(t, "sessions"), newTestFinderSource(t, "messages"))
	act := m.Open(0)
	m.ApplyResults(act.Gen, act.TabID, finderResults{Items: []finderItem{{Key: "a"}, {Key: "b"}}}, nil)
	m.HandleKey(tui.Key{Kind: tui.KeyDown})
	finderRunes(m, "bug")

	act = m.HandleKey(tui.Key{Kind: tui.KeyTab})
	require.Equal(t, finderActionSearch, act.Kind)
	require.Equal(t, "messages", act.TabID)
	require.Equal(t, "bug", act.Query.Text)
	require.Empty(t, m.items)
	require.Equal(t, 0, m.cursor)

	act = m.HandleKey(tui.Key{Kind: tui.KeyShiftTab})
	require.Equal(t, "sessions", act.TabID)
	act = m.HandleKey(tui.Key{Kind: tui.KeyShiftTab})
	require.Equal(t, "messages", act.TabID)
	require.Equal(t, "bug", act.Query.Text)
}

func TestFinderSingleTabIgnoresTab(t *testing.T) {
	m := newFinderModel("/work", newTestFinderSource(t, "sessions"))
	m.Open(0)
	gen := m.Generation()
	require.Equal(t, finderActionNone, m.HandleKey(tui.Key{Kind: tui.KeyTab}).Kind)
	require.Equal(t, gen, m.Generation())
}
