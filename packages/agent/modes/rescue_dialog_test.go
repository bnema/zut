package modes

import (
	"errors"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/tui"
)

func TestRescueErrorPreservesFullDiagnostic(t *testing.T) {
	msg := `opencode-go: http 403: {"type":"error","error":{"type":"DataPolicyError","message":"` + strings.Repeat("policy details ", 30) + `review consent at https://example.com/workspace/go"}}`
	ok, reason := classifyRescueError(errors.New(msg))
	if !ok || !strings.Contains(reason, msg) {
		t.Fatalf("diagnostic was truncated: %q", reason)
	}
}

func TestRescueDialogFullErrorView(t *testing.T) {
	d := newRescueDialog()
	reason := strings.Repeat("diagnostic 界 details ", 100) + "FINAL-DIAGNOSTIC"
	d.Open("", nil, "opencode-go", "example", reason, "prompt")
	d.HandleKey(tui.Key{Kind: tui.KeyTab})
	const width, height = 40, 10
	rows := d.Render(tui.Theme{}, width, height)
	assertRowsFitWidth(t, rows, width)
	if len(rows) > height+2 {
		t.Fatalf("error view exceeds viewport: %d", len(rows))
	}
	if !strings.Contains(strings.Join(rows, "\n"), "diagnostic") {
		t.Fatal("missing start of error")
	}
	for n := 0; n < 100; n++ {
		d.HandleKey(tui.Key{Kind: tui.KeyPageDown})
		rows = d.Render(tui.Theme{}, width, height)
	}
	if !strings.Contains(strings.Join(rows, "\n"), "FINAL-DIAGNOSTIC") {
		t.Fatal("cannot scroll to end of error")
	}
	d.HandleKey(tui.Key{Kind: tui.KeyHome})
	rows = d.Render(tui.Theme{}, width, height)
	if !strings.Contains(strings.Join(rows, "\n"), "diagnostic") {
		t.Fatal("cannot return to start")
	}
	if act := d.HandleKey(tui.Key{Kind: tui.KeyEnter}); act.Select || act.Close {
		t.Fatal("error view must not retry a model")
	}
	d.HandleKey(tui.Key{Kind: tui.KeyEsc})
	if !d.Active() || d.details {
		t.Fatal("escape should return to picker")
	}
	d.HandleKey(tui.Key{Kind: tui.KeyEsc})
	if d.Active() {
		t.Fatal("escape should dismiss picker")
	}
}
