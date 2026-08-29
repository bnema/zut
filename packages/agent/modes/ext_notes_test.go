package modes

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/tui"
)

func newNotesTestInteractive() *Interactive {
	i := &Interactive{dirty: make(chan struct{}, 1)}
	i.cfg.Theme = tui.Theme{
		Muted: tui.Color256(8), Warning: tui.Color256(3), Error: tui.Color256(1),
		Tool: tui.Color256(2), Accent: tui.Color256(4),
	}
	return i
}

func TestClearNotesRemovesOnlyOwnerNotes(t *testing.T) {
	i := newNotesTestInteractive()

	i.Notify("kagi", "info", "pending")
	i.Notify("kagi", "success", "approved")
	i.Notify("other", "info", "keep me")

	if len(i.extNotes) != 3 {
		t.Fatalf("expected 3 notes, got %d", len(i.extNotes))
	}

	i.ClearNotes("kagi")

	if len(i.extNotes) != 1 {
		t.Fatalf("expected 1 note after clear, got %d: %v", len(i.extNotes), i.extNotes)
	}
	if !strings.Contains(i.extNotes[0], "[other] ") {
		t.Fatalf("expected the surviving note to belong to other, got %q", i.extNotes[0])
	}
}

func TestClearNotesNoMatchKeepsNotes(t *testing.T) {
	i := newNotesTestInteractive()
	i.Notify("kagi", "info", "pending")

	i.ClearNotes("nope")

	if len(i.extNotes) != 1 {
		t.Fatalf("expected note to survive, got %d", len(i.extNotes))
	}
}

func TestPersistentExtensionWidgetRowsAreBounded(t *testing.T) {
	i := newNotesTestInteractive()
	lines := make([]string, 20)
	for idx := range lines {
		lines[idx] = "line"
	}
	i.SetWidget("extension", "widget", "above_input", "Title", lines)

	i.mu.Lock()
	got := i.extensionChromeLinesLocked(80)
	i.mu.Unlock()
	if len(got) != maxExtensionWidgetRows {
		t.Fatalf("widget rows = %d, want %d: %v", len(got), maxExtensionWidgetRows, got)
	}
	if !strings.Contains(strings.Join(got, "\n"), "extension widgets truncated") {
		t.Fatalf("bounded widget rows omitted truncation marker: %v", got)
	}
}

func TestPersistentExtensionStatusRowsAreBounded(t *testing.T) {
	i := newNotesTestInteractive()
	for n := 0; n < maxExtensionStatusRows+4; n++ {
		i.SetStatus("extension", fmt.Sprintf("status-%d", n), "info", "line")
	}

	i.mu.Lock()
	got := i.extensionChromeLinesLocked(80)
	i.mu.Unlock()
	if len(got) != maxExtensionStatusRows {
		t.Fatalf("status rows = %d, want %d: %v", len(got), maxExtensionStatusRows, got)
	}
	if !strings.Contains(strings.Join(got, "\n"), "extension statuses truncated") {
		t.Fatalf("bounded status rows omitted truncation marker: %v", got)
	}
}

func TestPersistentExtensionStatusAndWidget(t *testing.T) {
	i := newNotesTestInteractive()
	i.SetStatus("sample", "progress", "success", "2/4 tasks checked")
	i.SetWidget("sample", "plan", "above_input", "Plan", []string{"Current phase: parse", "[ ] read files"})

	i.mu.Lock()
	lines := i.extensionChromeLinesLocked(80)
	statusKey := i.extensionStatusesKeyLocked()
	widgetKey := i.extensionWidgetsKeyLocked()
	i.mu.Unlock()
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "2/4 tasks checked") || !strings.Contains(joined, "Plan") || !strings.Contains(joined, "[ ] read files") {
		t.Fatalf("persistent extension chrome = %q", joined)
	}
	if !strings.Contains(statusKey, "sample/progress/success/2/4 tasks checked") {
		t.Fatalf("status cache key = %q", statusKey)
	}
	if !strings.Contains(widgetKey, "sample/plan/above_input/Plan") {
		t.Fatalf("widget cache key = %q", widgetKey)
	}

	i.SetStatus("sample", "progress", "", "")
	i.ClearWidget("sample", "plan")
	if len(i.extStatuses) != 0 || len(i.extWidgets) != 0 {
		t.Fatalf("persistent extension chrome was not cleared: statuses=%v widgets=%v", i.extStatuses, i.extWidgets)
	}
}

func TestClearExtensionChromeRemovesOnlyTheExitedExtension(t *testing.T) {
	i := newNotesTestInteractive()
	i.SetStatus("gone", "progress", "success", "done")
	i.SetWidget("gone", "plan", "above_input", "Gone", []string{"old"})
	i.SetStatus("keep", "progress", "info", "still here")
	i.SetWidget("keep", "plan", "above_input", "Keep", []string{"new"})
	i.extNotes = []string{"[gone] old note", "[keep] keep note"}

	i.ClearExtensionChrome("gone")

	if _, ok := i.extStatuses["gone"]; ok {
		t.Fatal("exited extension status survived")
	}
	if _, ok := i.extWidgets["gone"]; ok {
		t.Fatal("exited extension widget survived")
	}
	if _, ok := i.extStatuses["keep"]; !ok {
		t.Fatal("unrelated extension status was removed")
	}
	if _, ok := i.extWidgets["keep"]; !ok {
		t.Fatal("unrelated extension widget was removed")
	}
	if got := strings.Join(i.extNotes, "\n"); got != "[keep] keep note" {
		t.Fatalf("extension notes after cleanup = %q", got)
	}
}

func TestInteractiveRedrawPlacesWidgetAboveInput(t *testing.T) {
	term := &alertTestTerminal{}
	i := NewInteractive(InteractiveConfig{Terminal: term, Theme: tui.Dark})
	i.rend.Resize(80, 24)
	i.SetWidget("sample", "plan", "above_input", "Plan", []string{"[ ] read files"})
	i.redraw()

	out := stripANSIBytes(term.String())
	if !strings.Contains(out, "Plan") || !strings.Contains(out, "[ ] read files") {
		t.Fatalf("redraw omitted above-input widget: %q", out)
	}
}
