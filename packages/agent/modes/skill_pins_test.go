package modes

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/skills"
	"github.com/bnema/zut/packages/tui"
	"github.com/mattn/go-runewidth"
)

func reviewSnapshot() []*skills.Skill {
	return []*skills.Skill{{Name: "review", Body: "Inspect carefully."}}
}

func pinKey(r rune) tui.Key { return tui.Key{Kind: tui.KeyRune, Rune: r} }

func TestSkillPinStateConsumesOnceAndRearms(t *testing.T) {
	load := func(string) (skills.Pins, error) { return skills.Pins{Global: []string{"review"}}, nil }
	s := newSkillPinState(load, nil, reviewSnapshot)
	s.arm(true, "/w", nil)
	if got := stripANSIBytes(strings.Join(s.notice(tui.Theme{}, 80), "")); !strings.Contains(got, "review (global)") {
		t.Fatalf("notice = %q", got)
	}
	first := s.consume("first")
	if !strings.Contains(first, "Inspect carefully.") || !strings.HasSuffix(first, "first") {
		t.Fatal(first)
	}
	if got := s.consume("second"); got != "second" {
		t.Fatal(got)
	}
	if len(s.notice(tui.Theme{}, 80)) != 0 {
		t.Fatal("notice survived consumption")
	}
	// /clear and /cd re-arm.
	s.arm(true, "/w", nil)
	if !strings.Contains(s.consume("after clear"), "Inspect carefully.") {
		t.Fatal("re-arm did not restore pins")
	}
	// Resume/import/fork disarm even after arming.
	s.arm(true, "/w", nil)
	s.arm(false, "/w", nil)
	if got := s.consume("resumed"); got != "resumed" {
		t.Fatal("resume reinjected pins")
	}
	// Refresh (model swap, /skills) never re-arms a consumed state.
	s.refresh("/w", nil)
	if got := s.consume("later"); got != "later" {
		t.Fatal("refresh re-armed pins")
	}
	// No loader means nothing is ever pending.
	none := newSkillPinState(nil, nil, reviewSnapshot)
	none.arm(true, "/w", nil)
	if got := none.consume("x"); got != "x" {
		t.Fatal(got)
	}
}

func TestSkillPinStateTogglesAndSaveFailure(t *testing.T) {
	pins := skills.Pins{}
	fail := false
	flip := func(names []string, name string) []string {
		if slices.Contains(names, name) {
			return nil
		}
		return []string{name}
	}
	toggle := func(_, name string, global bool) error {
		if fail {
			return errors.New("read-only preferences")
		}
		if global {
			pins.Global = flip(pins.Global, name)
		} else {
			pins.Project = flip(pins.Project, name)
		}
		return nil
	}
	s := newSkillPinState(func(string) (skills.Pins, error) { return pins, nil }, toggle, reviewSnapshot)
	d := newSkillsDialog()
	d.Open(reviewSnapshot())
	s.arm(true, "/w", d)

	for _, r := range []rune{'p', 'g'} {
		handled, ok, errText, _ := s.handleKey(pinKey(r), "/w", d)
		if !handled || errText != "" || ok == "" {
			t.Fatalf("key %c: handled=%v ok=%q err=%q", r, handled, ok, errText)
		}
	}
	if rendered := strings.Join(d.Render(tui.Theme{}, 100), "\n"); !strings.Contains(rendered, "[pg]") {
		t.Fatal(rendered)
	}
	if len(s.selected) != 1 {
		t.Fatal("scope union duplicated skill")
	}
	s.handleKey(pinKey('p'), "/w", d)
	if len(s.selected) != 1 || len(d.pins.Project) != 0 {
		t.Fatal("project unpin removed global pin")
	}
	fail = true
	handled, _, errText, _ := s.handleKey(pinKey('g'), "/w", d)
	if !handled || !strings.Contains(errText, "read-only") || len(d.pins.Global) != 1 {
		t.Fatalf("save failure not preserved: %q", errText)
	}
	// Body view and other keys are not pin keys.
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if handled, _, _, _ := s.handleKey(pinKey('g'), "/w", d); handled {
		t.Fatal("body view toggled a pin")
	}
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if handled, _, _, _ := s.handleKey(pinKey('x'), "/w", d); handled {
		t.Fatal("unrelated key handled")
	}
}

func TestSkillPinStateReadOnlyHost(t *testing.T) {
	s := newSkillPinState(func(string) (skills.Pins, error) { return skills.Pins{Global: []string{"review"}}, nil }, nil, reviewSnapshot)
	d := newSkillsDialog()
	d.Open(reviewSnapshot())
	s.arm(true, "/w", d)
	if handled, _, _, _ := s.handleKey(pinKey('p'), "/w", d); handled {
		t.Fatal("pin key handled without ToggleSkillPin")
	}
	rendered := strings.Join(d.Render(tui.Theme{}, 100), "\n")
	if strings.Contains(rendered, "[-g]") || strings.Contains(rendered, "p: project pin") {
		t.Fatalf("read-only host shows pin controls: %s", rendered)
	}
	// The pending notice still informs the user.
	if len(s.notice(tui.Theme{}, 80)) == 0 {
		t.Fatal("pending notice missing")
	}
}

func TestSkillPinStateNoticeShowsScopesOnce(t *testing.T) {
	load := func(string) (skills.Pins, error) {
		return skills.Pins{Project: []string{"local", "shared"}, Global: []string{"everywhere", "shared"}}, nil
	}
	snap := func() []*skills.Skill {
		return []*skills.Skill{{Name: "local"}, {Name: "everywhere"}, {Name: "shared"}}
	}
	s := newSkillPinState(load, nil, snap)
	s.arm(true, "/w", nil)
	text := stripANSIBytes(strings.Join(s.notice(tui.Theme{}, 120), "\n"))
	for _, want := range []string{"local (project)", "everywhere (global)", "shared (project + global)"} {
		if !strings.Contains(text, want) {
			t.Errorf("notice missing %q: %q", want, text)
		}
	}
	if strings.Count(text, "shared") != 1 {
		t.Fatalf("both scopes duplicated the skill: %q", text)
	}
}

func TestSkillPinStateWarningsAndNoticeWidth(t *testing.T) {
	load := func(string) (skills.Pins, error) { return skills.Pins{Global: []string{"missing", "review"}}, nil }
	s := newSkillPinState(load, nil, reviewSnapshot)
	warnings := s.arm(true, "/w", nil)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "missing") {
		t.Fatalf("warnings = %v", warnings)
	}
	bad := newSkillPinState(func(string) (skills.Pins, error) { return skills.Pins{}, errors.New("invalid skill-pins.json") }, nil, reviewSnapshot)
	if w := bad.arm(true, "/w", nil); len(w) != 1 || !strings.Contains(w[0], "invalid") {
		t.Fatalf("malformed warning = %v", w)
	}
	s.selected = []*skills.Skill{{Name: strings.Repeat("界", 30)}}
	for _, width := range []int{1, 4, 12, 30, 80} {
		for _, row := range s.notice(tui.Theme{}, width) {
			if got := runewidth.StringWidth(stripANSIBytes(row)); got > width {
				t.Fatalf("width %d exceeds %d", got, width)
			}
		}
	}
}
