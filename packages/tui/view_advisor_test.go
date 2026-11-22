package tui

import (
	"strings"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestViewRendersAdvisorNoteApartFromUserTurns(t *testing.T) {
	note := provider.Message{
		Role:    provider.RoleUser,
		Meta:    map[string]string{core.WatcherNoteMetaKey: "true"},
		Content: []provider.Content{provider.TextBlock{Text: "[advisor note]\nThe bug is in lexer.go, not parser.go."}},
	}
	view := &View{Theme: Dark, Messages: []provider.Message{note}}
	raw := strings.Join(view.Build(60), "\n")
	plain := stripANSI(raw)
	if !strings.Contains(plain, "◆ advisor note") || !strings.Contains(plain, "The bug is in lexer.go") {
		t.Fatalf("advisor note not rendered:\n%s", plain)
	}
	if strings.Contains(plain, "[advisor note]") || strings.Contains(plain, "▌") {
		t.Fatalf("advisor note rendered as a user bubble:\n%s", plain)
	}
}
