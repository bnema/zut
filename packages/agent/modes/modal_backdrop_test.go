package modes

import (
	"strings"
	"testing"

	"github.com/bnema/zut/packages/tui"
)

func TestFloatingDialogBodyMovesFrameTitleIntoPaneBorder(t *testing.T) {
	lines := padDialogFrame([]string{frameHeader(tui.Dark, "settings", 40), "body", frameRule(tui.Dark, 40)})
	title, body, removed := floatingDialogBody(lines)
	if title != "settings" || removed != 1 {
		t.Fatalf("chrome extraction = title:%q removed:%d, want settings/1", title, removed)
	}
	if strings.Join(body, "\n") != "\nbody\n" {
		t.Fatalf("dialog body retained frame chrome: %q", body)
	}
}

func TestFloatingBackgroundFramePreservesTopAnchoredViewport(t *testing.T) {
	tests := []struct {
		name   string
		chat   []string
		bottom []string
		rows   int
		want   []string
	}{
		{
			name:   "session stays at top",
			chat:   []string{"chat"},
			bottom: []string{"status"},
			rows:   5,
			want:   []string{"chat", "status", "", "", ""},
		},
		{
			name:   "oversized bottom retains newest rows",
			chat:   []string{"chat"},
			bottom: []string{"old", "newer", "newest"},
			rows:   3,
			want:   []string{"newer", "newest", ""},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := floatingBackgroundFrame(tt.chat, tt.bottom, tt.rows)
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
				t.Fatalf("floating background = %q, want %q", got, tt.want)
			}
		})
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
	innerHeader := frameHeader(i.cfg.Theme, "settings", pane.ContentWidth())
	if strings.Contains(got, innerHeader) {
		t.Fatalf("settings retained its inner frame header: %q", got)
	}
	if !strings.Contains(got, " settings ") {
		t.Fatalf("settings title was not rendered in the floating border: %q", got)
	}
	if strings.Contains(got, tui.Dim(" settings ")) {
		t.Fatalf("floating pane title was dimmed: %q", got)
	}
	// The pane shrinks to its chrome-free content and remains centered.
	if !strings.Contains(got, tui.MoveTo(10, pane.X+1)) {
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
