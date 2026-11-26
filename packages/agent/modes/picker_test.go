package modes

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/require"

	"github.com/bnema/zut/packages/tui"
)

func TestPickerRowNoteIsDroppedBeforeLabel(t *testing.T) {
	row := pickerRow{Label: "Claude Opus 4.8", Note: "speculative", Detail: "claude-opus-4-8", Mark: "●"}
	cols := pickerListColumns([]pickerRow{row})

	wide := stripANSIBytes(renderPickerRow(tui.Dark, row, false, cols, 60))
	require.Equal(t, 60, runewidth.StringWidth(wide))
	require.Contains(t, wide, "Claude Opus 4.8 speculative")
	require.True(t, strings.HasSuffix(wide, "claude-opus-4-8 ● "), wide)

	narrow := stripANSIBytes(renderPickerRow(tui.Dark, row, true, cols, 36))
	require.Equal(t, 36, runewidth.StringWidth(narrow))
	require.NotContains(t, narrow, "spec")
	require.Contains(t, narrow, "Claude Opus")
}

func TestPickerFuzzyScoresMarksMatches(t *testing.T) {
	scores, matched := pickerFuzzyScores("opus46", []string{"claude-opus-4-6", "gpt-5.2", "claude-opus-4-5"})
	require.Equal(t, []bool{true, false, false}, matched)
	require.Positive(t, scores[0])

	_, matched = pickerFuzzyScores("", []string{"a"})
	require.Equal(t, []bool{false}, matched)
}

func TestPickerQueryEditAndCaret(t *testing.T) {
	var q pickerQuery
	for _, r := range "opus" {
		require.True(t, q.Edit(tui.Key{Kind: tui.KeyRune, Rune: r}))
	}
	require.False(t, q.Edit(tui.Key{Kind: tui.KeyLeft}))
	require.True(t, q.Edit(tui.Key{Kind: tui.KeyPaste, Paste: "4\n6"}))
	require.Equal(t, "opu4 6s", q.String())
	q.Render(tui.Dark, 40)
	require.Equal(t, 3+runewidth.StringWidth("opu4 6"), q.Caret())
	q.Reset()
	require.Empty(t, q.String())
}
