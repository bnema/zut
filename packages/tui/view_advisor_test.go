package tui

import (
	"strings"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestViewRendersAdvisorNotesApartFromUserTurns(t *testing.T) {
	for _, tc := range []struct {
		note, header, body string
	}{
		{"[advisor note]\nThe bug is in lexer.go.\n\nYou decide.", "◆ advisor note", "The bug is in lexer.go."},
		{"[advisor] Stop and reconsider:\nwrong file", "◆ advisor · Stop and reconsider", "wrong file"},
		{"[advisor note] tests not run", "◆ advisor note", "tests not run"},
	} {
		note := provider.Message{
			Role:    provider.RoleUser,
			Meta:    map[string]string{core.WatcherNoteMetaKey: "true"},
			Content: []provider.Content{provider.TextBlock{Text: tc.note}},
		}
		view := &View{Theme: Dark, Messages: []provider.Message{note}}
		plain := stripANSI(strings.Join(view.Build(80), "\n"))
		if !strings.Contains(plain, tc.header+"\n") || !strings.Contains(plain, tc.body) {
			t.Fatalf("note %q rendered:\n%s", tc.note, plain)
		}
		if strings.Contains(plain, "]") || strings.Contains(plain, "▌") {
			t.Fatalf("note %q leaked its tag or a user bubble:\n%s", tc.note, plain)
		}
	}
}
