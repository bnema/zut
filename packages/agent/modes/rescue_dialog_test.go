package modes

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bnema/zut/packages/agent/session"
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

func TestRescueDialogUsesSharedPicker(t *testing.T) {
	d := newRescueDialog()
	d.OpenPending("r1", session.RescueOptions{
		Error:      "rate limited",
		Candidates: []string{"openai/gpt-5.2", "anthropic/claude-opus-4-5", "openrouter/anthropic/claude-opus-4.5"},
	}, nil)
	for _, r := range "opus45" {
		d.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: r})
	}
	require.Len(t, d.view, 2)
	const width = 70
	rows := d.Render(tui.Dark, width, 20)
	assertRowsFitWidth(t, rows, width)
	text := stripANSIBytes(strings.Join(rows, "\n"))
	require.Contains(t, text, "anthropic · Claude Opus 4.5")
	require.Contains(t, text, "enter retry")
	row, col := d.CursorPos()
	require.Contains(t, stripANSIBytes(rows[row]), " > opus45")
	require.Equal(t, 3+len("opus45"), col)

	d.HandleKey(tui.Key{Kind: tui.KeyTab})
	row, _ = d.CursorPos()
	require.Equal(t, -1, row, "the error view has no query caret")
}

func TestRescueDialogPendingUsesCatalogAndShowsHelpWhenEmpty(t *testing.T) {
	d := newRescueDialog()
	d.OpenPending("r1", session.RescueOptions{Error: "rate limited", Candidates: []string{"anthropic/claude-opus-4-5"}}, nil)
	require.Len(t, d.all, 1)
	require.NotEmpty(t, d.all[0].DisplayName, "catalog metadata fills pending candidates")
	require.True(t, d.all[0].Reasoning)

	d.OpenPending("r2", session.RescueOptions{Error: "rate limited"}, nil)
	text := stripANSIBytes(strings.Join(d.Render(tui.Dark, 70, 20), "\n"))
	require.Contains(t, text, "no other models available")
	require.Contains(t, text, "tab full error")
}

// TestRescueDialogFitsPaneThroughFrame renders through the floating pane
// path: the body fits the pane, the caret lands on the query line, and the
// highlighted row stays visible.
func TestRescueDialogFitsPaneThroughFrame(t *testing.T) {
	var candidates []string
	for n := 0; n < 20; n++ {
		candidates = append(candidates, fmt.Sprintf("openai/gpt-test-%02d", n))
	}
	d := newRescueDialog()
	d.OpenPending("r1", session.RescueOptions{Error: strings.Repeat("rate limited ", 40), Candidates: candidates}, nil)
	for n := 0; n < 15; n++ {
		d.HandleKey(tui.Key{Kind: tui.KeyDown})
	}
	for _, h := range []int{8, 12, 17, 40} {
		_, body, removed := floatingDialogBody(padDialogFrame(d.Render(tui.Dark, 60, h)))
		require.LessOrEqual(t, len(body), h, "height %d", h)
		r, _ := d.CursorPos()
		require.Contains(t, stripANSIBytes(body[r-removed]), " >", "height %d", h)
		require.Contains(t, stripANSIBytes(strings.Join(body, "\n")), "gpt-test-15", "height %d", h)
	}
}
