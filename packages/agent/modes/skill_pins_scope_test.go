package modes

import (
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/skills"
	"github.com/bnema/zut/packages/tui"
)

func TestSkillPinStateReadOnlyWithoutLoader(t *testing.T) {
	called := false
	state := newSkillPinState(nil, func(string, string, bool) error { called = true; return nil }, reviewSnapshot)
	dialog := newSkillsDialog()
	dialog.Open(reviewSnapshot())
	state.refresh("/workspace", dialog)
	if handled, _, _, _ := state.handleKey(pinKey('p'), "/workspace", dialog); handled || called {
		t.Fatal("read-only picker wrote pin preferences")
	}
}

func TestSkillPinAliasesShowScopesAndPreloadOnce(t *testing.T) {
	skill := &skills.Skill{Name: "review", Aliases: []string{"group/review"}, Body: "Inspect carefully."}
	pins := skills.Pins{Project: []string{"group/review"}, Global: []string{"review"}}
	state := newSkillPinState(func(string) (skills.Pins, error) { return pins, nil }, func(string, string, bool) error { return nil }, func() []*skills.Skill { return []*skills.Skill{skill} })
	dialog := newSkillsDialog()
	dialog.Open([]*skills.Skill{skill})
	state.arm(true, "/workspace", dialog)
	if marker := dialog.pinMarker(skill); marker != "[pg] " {
		t.Fatalf("marker=%q, want [pg]", marker)
	}
	notice := stripANSIBytes(strings.Join(state.notice(tui.Theme{}, 80), ""))
	if !strings.Contains(notice, "review (project + global)") {
		t.Fatalf("alias scope missing from notice: %q", notice)
	}
	if prompt := state.consume("question"); strings.Count(prompt, "Inspect carefully.") != 1 {
		t.Fatalf("alias pin included body twice: %q", prompt)
	}
}
