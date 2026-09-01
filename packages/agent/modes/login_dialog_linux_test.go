//go:build linux

package modes

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/tui"
)

func TestInteractiveAltCUsesSystemClipboardWithoutTerminal(t *testing.T) {
	dir := t.TempDir()
	payload := filepath.Join(dir, "clipboard-payload")
	if err := os.WriteFile(filepath.Join(dir, "wl-copy"), []byte("#!/bin/sh\nexec /bin/cat > \"$CLIPBOARD_TEST_PAYLOAD\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("WAYLAND_DISPLAY", "wayland-test")
	t.Setenv("DISPLAY", "")
	t.Setenv("CLIPBOARD_TEST_PAYLOAD", payload)

	const loginURL = "https://example.com/oauth/authorize?state=xyz"
	i := NewInteractive(InteractiveConfig{})
	i.dialog.Open(t.TempDir())
	i.dialog.ShowWaiting(loginURL)

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: 'c', Alt: true})

	got, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != loginURL {
		t.Fatalf("clipboard payload = %q, want %q", got, loginURL)
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.statusOK != "login URL copied" || i.statusErr != "" {
		t.Fatalf("status = (%q, %q), want successful system copy", i.statusOK, i.statusErr)
	}
}

func TestInteractiveAltCReportsSystemClipboardError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wl-copy"), []byte("#!/bin/sh\nexit 23\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("WAYLAND_DISPLAY", "wayland-test")
	t.Setenv("DISPLAY", "")

	i := NewInteractive(InteractiveConfig{})
	i.dialog.Open(t.TempDir())
	i.dialog.ShowWaiting("https://example.com/oauth/authorize?state=xyz")

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: 'c', Alt: true})

	i.mu.Lock()
	defer i.mu.Unlock()
	if !strings.Contains(i.statusErr, "exit status 23") {
		t.Fatalf("status error = %q, want clipboard command failure", i.statusErr)
	}
	if i.statusOK != "" {
		t.Fatalf("success status = %q, want empty", i.statusOK)
	}
}
