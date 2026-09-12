package modes

import (
	"errors"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/extensions"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/tui"
)

func TestFormatReloadStatusIncludesErrorDetails(t *testing.T) {
	msg, failed := formatReloadStatus(extensions.ReloadStats{
		Stopped: 2,
		Loaded:  1,
		Ready:   1,
		Errors: []error{
			errors.New("broken manifest"),
			errors.New("spawn worker: executable not found"),
		},
	})
	if !failed {
		t.Fatal("reload with errors was not marked failed")
	}
	for _, want := range []string{
		"2 stopped, 1 loaded (1 ready)",
		"2 error(s)",
		"broken manifest",
		"spawn worker: executable not found",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("reload status missing %q: %s", want, msg)
		}
	}
}

func TestRenderReloadErrorsPreservesMultilineAlignment(t *testing.T) {
	lines := renderReloadErrors(tui.Theme{Error: tui.Color256(1)}, []string{"failed to start\nextension directory: /tmp/ext\nstderr log: /tmp/ext.log"}, 80)
	if len(lines) != 3 {
		t.Fatalf("rendered %d lines, want 3: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "✖ failed to start") {
		t.Fatalf("first line = %q, want error marker", lines[0])
	}
	for _, line := range lines[1:] {
		if !strings.Contains(line, "  ") {
			t.Fatalf("continuation line = %q, want indentation", line)
		}
	}
}

func TestFormatReloadStatusSuccess(t *testing.T) {
	msg, failed := formatReloadStatus(extensions.ReloadStats{Stopped: 2, Loaded: 3, Ready: 3})
	if failed {
		t.Fatal("successful reload was marked failed")
	}
	if want := "reloaded: 2 stopped, 3 loaded (3 ready)"; msg != want {
		t.Fatalf("reload status = %q, want %q", msg, want)
	}
}

func TestSetReloadStatusUsesErrorChannelForFailures(t *testing.T) {
	i := &Interactive{}
	i.setReloadStatus("broken extension", true)
	if i.statusErr != "broken extension" || i.statusOK != "" {
		t.Fatalf("failed reload status used wrong channel: err=%q ok=%q", i.statusErr, i.statusOK)
	}
	if len(i.reloadErrors) != 1 || i.reloadErrors[0] != "broken extension" {
		t.Fatalf("reload errors = %q, want persistent failure", i.reloadErrors)
	}
	i.setReloadStatus("reload complete", false)
	if i.statusOK != "reload complete" || i.statusErr != "" {
		t.Fatalf("successful reload status used wrong channel: err=%q ok=%q", i.statusErr, i.statusOK)
	}
	if len(i.reloadErrors) != 0 {
		t.Fatalf("reload errors = %q after successful reload, want none", i.reloadErrors)
	}
}

func TestStartupExtensionFailureAppearsInChatWithoutEnteringContext(t *testing.T) {
	agent := core.NewAgent(nil, "", "", nil)
	i := NewInteractive(InteractiveConfig{
		Agent:                  agent,
		Theme:                  tui.Theme{Error: tui.Color256(1)},
		StartupExtensionErrors: []string{"startup failed: stderr log: /tmp/startup.log"},
	})

	chat := strings.Join(i.buildChatLocked(30), "\n")
	if !strings.Contains(chat, "startup failed") || !strings.Contains(chat, "/tmp/startup.log") {
		t.Fatalf("chat does not contain startup extension failure: %q", chat)
	}
	if got := len(agent.Messages()); got != 0 {
		t.Fatalf("agent context contains %d startup error message(s), want none", got)
	}
}

func TestReloadFailureRemainsInChatWithoutEnteringContext(t *testing.T) {
	agent := core.NewAgent(nil, "", "", nil)
	i := NewInteractive(InteractiveConfig{
		Agent: agent,
		Theme: tui.Theme{Error: tui.Color256(1)},
	})
	seq := i.setReloadStatus("broken extension: stderr log: /tmp/ext.log", true)
	if !i.clearReloadStatus(seq, i.statusErr, true) {
		t.Fatal("temporary reload status was not cleared")
	}

	chat := strings.Join(i.buildChatLocked(30), "\n")
	if !strings.Contains(chat, "broken extension") || !strings.Contains(chat, "/tmp/ext.log") {
		t.Fatalf("chat does not contain persistent reload failure: %q", chat)
	}
	if got := len(agent.Messages()); got != 0 {
		t.Fatalf("agent context contains %d reload error message(s), want none", got)
	}
}

func TestClearReloadStatusDoesNotClearNewerStatus(t *testing.T) {
	i := &Interactive{statusErr: "newer error", statusOK: "newer success"}
	if i.clearReloadStatus(0, "old error", true) {
		t.Fatal("stale error dismissal reported a clear")
	}
	if i.clearReloadStatus(0, "old success", false) {
		t.Fatal("stale success dismissal reported a clear")
	}
	if i.statusErr != "newer error" || i.statusOK != "newer success" {
		t.Fatalf("stale dismissal changed status: err=%q ok=%q", i.statusErr, i.statusOK)
	}

	if !i.clearReloadStatus(0, "newer error", true) || i.statusErr != "" {
		t.Fatalf("matching error status was not cleared: %q", i.statusErr)
	}
	if !i.clearReloadStatus(0, "newer success", false) || i.statusOK != "" {
		t.Fatalf("matching success status was not cleared: %q", i.statusOK)
	}

	first := i.setReloadStatus("same result", false)
	second := i.setReloadStatus("same result", false)
	if first == second {
		t.Fatal("reload status generation did not advance")
	}
	if i.clearReloadStatus(first, "same result", false) {
		t.Fatal("older identical reload status cleared the newer result")
	}
}
