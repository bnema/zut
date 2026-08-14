package modes

import (
	"strings"
	"testing"

	"github.com/bnema/zut/packages/tui"
)

func TestFloatingBackgroundFrameKeepsNewestOversizedBottomRows(t *testing.T) {
	got := floatingBackgroundFrame([]string{"chat"}, []string{"old", "newer", "newest"}, 3)
	want := []string{"newer", "newest", ""}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("floating background = %q, want %q", got, want)
	}
}

func TestSettingsDialogDimsBackdropAndMainCursor(t *testing.T) {
	term := &alertTestTerminal{}
	i := NewInteractive(InteractiveConfig{Terminal: term, Theme: tui.Dark})
	i.rend.Resize(80, 24)
	i.ed.SetValue("draft")
	if !i.settingsDialog.Open([]settingsItem{{key: "test", label: "test setting"}}) {
		t.Fatal("settings dialog did not open")
	}

	i.redraw()
	got := term.String()

	input := i.cfg.Theme.AccentBar(i.cfg.Theme.Accent) + "draft"
	if !strings.Contains(got, tui.Dim(input)) {
		t.Fatalf("settings dialog left the main input undimmed: %q", got)
	}
	pane := tui.FloatingPaneMaxRect(80, 24)
	header := frameHeader(i.cfg.Theme, "settings", pane.ContentWidth())
	if !strings.Contains(got, header) {
		t.Fatalf("settings dialog was not rendered inside the floating pane: %q", got)
	}
	if strings.Contains(got, tui.Dim(header)) {
		t.Fatalf("settings dialog itself was dimmed: %q", got)
	}
	// The pane shrinks to its content and remains horizontally centered.
	if !strings.Contains(got, tui.MoveTo(9, pane.X+1)) {
		t.Fatalf("floating pane was not positioned at its computed geometry: %q", got)
	}
	cursor := tui.CursorColor(i.cfg.Theme.DimColor(tui.Color256(15), modalBackdropDimPercent))
	if !strings.Contains(got, cursor) {
		t.Fatalf("settings dialog left the main cursor bright; missing %q in %q", cursor, got)
	}

	term.mu.Lock()
	term.data = nil
	term.mu.Unlock()
	i.settingsDialog.Close()
	i.redraw()
	if got := term.String(); !strings.Contains(got, tui.CursorColor(tui.Color256(15))) {
		t.Fatalf("closing settings did not restore the main cursor color: %q", got)
	}
}
