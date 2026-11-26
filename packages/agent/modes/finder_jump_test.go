package modes

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

func jumpTextMsg(role provider.Role, text string) provider.Message {
	return provider.Message{Role: role, Content: []provider.Content{provider.TextBlock{Text: text}}}
}

func TestResolveHitInView(t *testing.T) {
	stored := []provider.Message{
		jumpTextMsg(provider.RoleUser, "hello"),   // 0
		jumpTextMsg(provider.RoleAssistant, "ok"), // 1
		jumpTextMsg(provider.RoleUser, "again"),   // 2
		jumpTextMsg(provider.RoleAssistant, "ok"), // 3
		jumpTextMsg(provider.RoleUser, "target"),  // 4
		jumpTextMsg(provider.RoleAssistant, "ok"), // 5
	}
	fp := core.MessageTextFingerprint
	tool := provider.Message{Role: provider.RoleTool, Content: []provider.Content{provider.TextBlock{Text: "synthetic result"}}}

	for _, tc := range []struct {
		name    string
		view    []provider.Message
		fp      uint64
		seq     int
		want    int
		wantOK  bool
		comment string
	}{
		{"plain", stored, fp(stored[4]), 4, 4, true, ""},
		{"duplicate picks nearest", stored, fp(stored[1]), 3, 3, true, "three \"ok\" rows"},
		{"duplicate last", stored, fp(stored[5]), 5, 5, true, ""},
		{"compaction removed head", append([]provider.Message{jumpTextMsg(provider.RoleUser, "summary")}, stored[3:]...), fp(stored[3]), 3, 1, true, ""},
		{"repair inserted tool row", append(append(append([]provider.Message(nil), stored[:2]...), tool), stored[2:]...), fp(stored[3]), 3, 4, true, ""},
		{"hidden row filtered", append(append([]provider.Message(nil), stored[:2]...), stored[3:]...), fp(stored[1]), 1, 1, true, "row 2 hidden"},
		{"missing", stored[3:], fp(stored[0]), 0, -1, false, "compacted away"},
		{"role matters", stored, fp(jumpTextMsg(provider.RoleUser, "ok")), 1, -1, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx, ok := resolveHitInView(tc.view, tc.fp, tc.seq, len(stored))
			require.Equal(t, tc.wantOK, ok, tc.comment)
			require.Equal(t, tc.want, idx, tc.comment)
		})
	}
}

func TestApplyJumpSelectionWidensResumeTail(t *testing.T) {
	i := NewInteractive(InteractiveConfig{Theme: tui.Dark, Terminal: &alertTestTerminal{}})
	var msgs []provider.Message
	for n := range 100 {
		msgs = append(msgs, jumpTextMsg(provider.RoleUser, fmt.Sprintf("question %d", n)), jumpTextMsg(provider.RoleAssistant, fmt.Sprintf("answer %d", n)))
	}
	i.view.Messages = msgs
	i.view.TailLimit = initialResumeTailLimit
	target := 20 // older than the 80-message tail
	require.Less(t, target, len(msgs)-initialResumeTailLimit)

	i.applyJumpSelection(target, 11)
	require.Empty(t, i.statusErr)
	require.Equal(t, "jumped to turn 11", i.statusOK)
	require.Equal(t, len(msgs)-target, i.view.TailLimit)

	chat, anchors := i.view.BuildWithAnchors(i.lastCols())
	row := -1
	for _, a := range anchors {
		if a.MessageIdx == target {
			row = a.Row
		}
	}
	require.GreaterOrEqual(t, row, 0)
	require.Equal(t, row, len(chat)-i.scrollOffset-i.chatPage(), "target at the top of the viewport")
}

func TestApplyJumpSelectionKeepsTailWhenTargetVisible(t *testing.T) {
	i := NewInteractive(InteractiveConfig{Theme: tui.Dark, Terminal: &alertTestTerminal{}})
	var msgs []provider.Message
	for n := range 100 {
		msgs = append(msgs, jumpTextMsg(provider.RoleUser, fmt.Sprintf("q %d", n)), jumpTextMsg(provider.RoleAssistant, "a"))
	}
	i.view.Messages = msgs
	i.view.TailLimit = initialResumeTailLimit
	i.applyJumpSelection(len(msgs)-initialResumeTailLimit, 61)
	require.Empty(t, i.statusErr)
	require.Equal(t, initialResumeTailLimit, i.view.TailLimit)
}
