package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeThemeFile(t *testing.T, name, body string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "themes"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "themes", name+".json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestLoadThemeAllowsPartialColorOverrides(t *testing.T) {
	home := writeThemeFile(t, "partial", `{"colors":{"dark":{"accent":204}}}`)

	th, name, err := LoadThemeFromHome(home, "partial", Dark)
	if err != nil {
		t.Fatal(err)
	}
	if name != "partial" {
		t.Fatalf("name = %q, want partial", name)
	}
	if th.Accent != Color256(204) {
		t.Fatalf("accent = %#v, want 204", th.Accent)
	}
	if th.FG != TerminalDefault() {
		t.Fatalf("fg = %#v, want terminal default", th.FG)
	}
	if len(th.SpinnerFrames) == 0 {
		t.Fatal("spinner frames should be inherited")
	}
}

func TestLoadThemeAllowsThinkingMaxOverride(t *testing.T) {
	home := writeThemeFile(t, "thinking", `{"colors":{"dark":{"thinkingMax":201}}}`)

	th, _, err := LoadThemeFromHome(home, "thinking", Dark)
	if err != nil {
		t.Fatal(err)
	}
	if th.ThinkingMax != Color256(201) {
		t.Fatalf("thinking max = %#v, want 201", th.ThinkingMax)
	}
}

func TestLoadThemeAllowsSpinnerAppearanceOverrides(t *testing.T) {
	home := writeThemeFile(t, "spinner", `{"spinner_frames":[".","o"],"spinner_messages":["working"],"spinner_interval_ms":200}`)

	th, _, err := LoadThemeFromHome(home, "spinner", Dark)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(th.SpinnerFrames); got != 2 {
		t.Fatalf("spinner frame count = %d, want 2", got)
	}
	if th.SpinnerFrames[1] != "o" || th.SpinnerIntervalMS != 200 {
		t.Fatalf("spinner appearance overrides not applied: %#v %d", th.SpinnerFrames, th.SpinnerIntervalMS)
	}
	if th.Accent != TerminalPaletteSlot(12) {
		t.Fatalf("accent = %#v, want terminal palette slot 12", th.Accent)
	}
}

func TestLoadThemeFallsBackToDarkWhenLightModeMissing(t *testing.T) {
	home := writeThemeFile(t, "darkonly", `{"colors":{"dark":{"spinner_frames":["◢","◣","◤","◥"],"spinner_messages":["working"],"spinner_interval_ms":120}}}`)
	detected := Light
	detected.Terminal = TerminalProfile{SchemeKnown: true, Light: true}

	th, _, err := LoadThemeFromHome(home, "darkonly", detected)
	if err != nil {
		t.Fatal(err)
	}
	if len(th.SpinnerFrames) != 4 || th.SpinnerFrames[0] != "◢" {
		t.Fatalf("spinner frames = %#v, want dark fallback frames", th.SpinnerFrames)
	}
	if th.SpinnerIntervalMS != 120 {
		t.Fatalf("spinner interval = %d, want 120", th.SpinnerIntervalMS)
	}
	if th.FG != TerminalDefault() {
		t.Fatalf("fg = %#v, want terminal default", th.FG)
	}
}

func TestLoadThemeIgnoresLegacySpinnerMessages(t *testing.T) {
	home := writeThemeFile(t, "shared", `{"colors":{"accent":204,"spinner_messages":["ship"]}}`)

	th, _, err := LoadThemeFromHome(home, "shared", Light)
	if err != nil {
		t.Fatal(err)
	}
	if th.Accent != Color256(204) {
		t.Fatalf("accent = %#v, want 204", th.Accent)
	}
	if len(th.SpinnerFrames) != len(Light.SpinnerFrames) || th.SpinnerIntervalMS != Light.SpinnerIntervalMS {
		t.Fatalf("legacy spinner_messages changed spinner appearance: %#v %d", th.SpinnerFrames, th.SpinnerIntervalMS)
	}
	if th.FG != TerminalDefault() {
		t.Fatalf("fg = %#v, want terminal default", th.FG)
	}
}

func TestLoadThemeAcceptsRGBSemanticColors(t *testing.T) {
	home := writeThemeFile(t, "rgb", `{"colors":{"dark":{"fg":"#123456","muted":{"mode":"rgb","r":18,"g":52,"b":86},"accent":{"mode":"rgb","r":200,"g":100,"b":50},"user": {"mode":"ansi","index":31},"selection_bg":{"mode":"256","index":237},"selection_fg":250}}}`)

	detected := Dark
	detected.Terminal.TrueColor = true
	th, _, err := LoadThemeFromHome(home, "rgb", detected)
	if err != nil {
		t.Fatal(err)
	}
	if !th.Terminal.TrueColor {
		t.Fatal("terminal color mode = 256, want truecolor")
	}
	if th.FG != ColorRGB(0x12, 0x34, 0x56) {
		t.Fatalf("foreground = %#v, want RGB", th.FG)
	}
	if th.Accent != ColorRGB(200, 100, 50) {
		t.Fatalf("accent = %#v, want RGB", th.Accent)
	}
	if th.SelectionBG != Color256(237) || th.SelectionFG != Color256(250) {
		t.Fatalf("indexed semantic colors were not preserved: bg=%#v fg=%#v", th.SelectionBG, th.SelectionFG)
	}
	if got := th.FGColor(th.Accent, "x"); !strings.Contains(got, "\x1b[38;2;200;100;50m") {
		t.Fatalf("RGB accent was not rendered as truecolor: %q", got)
	}
}

func TestLoadThemeQuantizesRGBSemanticColorsWithoutTrueColor(t *testing.T) {
	home := writeThemeFile(t, "rgb-fallback", `{"colors":{"dark":{"accent":"#c86432"}}}`)

	th, _, err := LoadThemeFromHome(home, "rgb-fallback", Dark)
	if err != nil {
		t.Fatal(err)
	}
	want := nearestXtermColor(200, 100, 50)
	if got := th.FGColor(th.Accent, "x"); got != sgrFG(want)+"x\x1b[0m" {
		t.Fatalf("RGB accent fallback = %q, want xterm-256 index %d", got, want)
	}
}

func TestLoadThemeRejectsInvalidNestedValues(t *testing.T) {
	for _, body := range []string{
		`{"colors":{"dark":{"accent":999}}}`,
		`{"colors":{"light":{"syntax":{"keyword":"not a style"}}}}`,
		`{"spinner_interval_ms":1}`,
		`{"syntax_base_style":"not-a-style"}`,
	} {
		home := writeThemeFile(t, "invalid", body)
		if _, err := LoadThemeSource(home, "invalid"); err == nil {
			t.Fatalf("LoadThemeSource(%s) succeeded", body)
		}
	}
}

func TestResolveThemeCustomUsesReportedScheme(t *testing.T) {
	home := writeThemeFile(t, "split", `{"colors":{"dark":{"accent":20},"light":{"accent":21}}}`)
	source, err := LoadThemeSource(home, "split")
	if err != nil {
		t.Fatal(err)
	}
	profile := TerminalProfile{SchemeKnown: true, Light: true}
	if got := ResolveTheme("split", source, profile).Theme.Accent; got != Color256(21) {
		t.Fatalf("light branch accent = %#v, want 21", got)
	}
	dark := TerminalProfile{SchemeKnown: true, Light: false}
	if got := ResolveTheme("split", source, dark).Theme.Accent; got != Color256(20) {
		t.Fatalf("dark branch accent = %#v, want 20", got)
	}
}
