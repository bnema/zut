package tui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestReadClipboardTextCommandsUsesAvailableCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "clipboard-test")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf 'clipboard text'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	text, ok, err := readClipboardTextCommands(clipboardTextCommand{name: "clipboard-test"})
	if err != nil {
		t.Fatal(err)
	}
	if !ok || text != "clipboard text" {
		t.Fatalf("text = %q, ok = %v", text, ok)
	}
}

func TestReadClipboardTextCommandsReportsUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, ok, err := readClipboardTextCommands(clipboardTextCommand{name: "missing-clipboard-command"})
	if ok || err != errClipboardCommandUnavailable {
		t.Fatalf("ok = %v, err = %v", ok, err)
	}
}

func TestWriteClipboardTextCommandsPassesTextOnStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a shell script")
	}
	dir := t.TempDir()
	payload := filepath.Join(dir, "clipboard-payload")
	path := filepath.Join(dir, "clipboard-test")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n/bin/cat > \"$CLIPBOARD_TEST_PAYLOAD\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("CLIPBOARD_TEST_PAYLOAD", payload)

	if err := writeClipboardTextCommands(context.Background(), "https://example.com/oauth?state=xyz", clipboardTextCommand{name: "clipboard-test"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "https://example.com/oauth?state=xyz" {
		t.Fatalf("clipboard payload = %q", got)
	}
}

func TestWriteClipboardTextCommandsHonorsCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "clipboard-test")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := writeClipboardTextCommands(ctx, "clipboard text", clipboardTextCommand{name: "clipboard-test"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
}

func TestWriteClipboardTextOSC52EncodesPayload(t *testing.T) {
	var out bytes.Buffer
	if err := WriteClipboardTextOSC52(&out, "https://example.com/oauth?state=xyz"); err != nil {
		t.Fatal(err)
	}
	const want = "\x1b]52;c;aHR0cHM6Ly9leGFtcGxlLmNvbS9vYXV0aD9zdGF0ZT14eXo=\a"
	if got := out.String(); got != want {
		t.Fatalf("OSC 52 = %q, want %q", got, want)
	}
}

func TestSupportsOSC52ClipboardForVev(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "vev")
	if !SupportsOSC52Clipboard() {
		t.Fatal("Vev terminal did not advertise OSC 52 clipboard support")
	}
}

func TestSupportsOSC52ClipboardRejectsUnknownTerminal(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "")
	if SupportsOSC52Clipboard() {
		t.Fatal("unknown terminal advertised OSC 52 clipboard support")
	}
}
