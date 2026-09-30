package skills

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPinsResolveUnionAndMissing(t *testing.T) {
	a := &Skill{Name: "a", Body: "first"}
	b := &Skill{Name: "ext:b", Body: "second", DisableModelInvocation: true}
	pins := Pins{Global: []string{"ext:b", "a", "missing"}, Project: []string{"a", "ext:b"}}
	selected, warnings := pins.Resolve([]*Skill{b, a})
	if len(selected) != 2 || selected[0] != a || selected[1] != b {
		t.Fatalf("selected = %#v", selected)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "missing") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestPreloadPromptPreservesSkillLocationAndRequest(t *testing.T) {
	s := &Skill{Name: "review", Body: "Inspect carefully.", Path: filepath.Join("skills", "review", "SKILL.md")}
	got := PreloadPrompt([]*Skill{s}, "my request")
	for _, want := range []string{"# Skill: review", "Inspect carefully.", "Skill directory: " + filepath.Dir(s.Path), "User request:\nmy request"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if got := PreloadPrompt(nil, "unchanged"); got != "unchanged" {
		t.Fatal(got)
	}
}

func TestPinsResolveDeduplicatesAliasAndCanonical(t *testing.T) {
	inner := &Skill{Name: "golang-patterns", Aliases: []string{"systems/subskills/golang-patterns"}, Path: "/r/systems/subskills/golang-patterns/SKILL.md"}
	other := &Skill{Name: "alpha", Path: "/r/alpha/SKILL.md"}
	pins := Pins{
		Global:  []string{"systems/subskills/golang-patterns", "alpha"},
		Project: []string{"golang-patterns"},
	}
	available := []*Skill{inner, other}
	for i := 0; i < 20; i++ { // map iteration must not affect the result
		selected, warnings := pins.Resolve(available)
		if len(warnings) != 0 || len(selected) != 2 || selected[0] != other || selected[1] != inner {
			t.Fatalf("selected=%v warnings=%v", selected, warnings)
		}
	}
	project, global := pins.Scopes(available, inner)
	if !project || !global {
		t.Fatalf("scopes = project %v global %v", project, global)
	}
	project, global = pins.Scopes(available, other)
	if project || !global {
		t.Fatalf("scopes = project %v global %v", project, global)
	}
}
