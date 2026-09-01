package modes

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/tui"
)

func TestLoginDialogProviderPickerFiltersLikeModelPicker(t *testing.T) {
	d := newLoginDialog()
	d.step = loginStepProvider
	d.method = "apikey"
	d.status = map[string]string{}
	for _, r := range "llamacpp" {
		d.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: r})
	}
	options := d.providerOptions()
	if len(options) != 1 || options[0] != "llama.cpp" {
		t.Fatalf("options = %v", options)
	}
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if d.step != loginStepLlamaURL {
		t.Fatalf("step = %v", d.step)
	}
}

func TestLoginDialogProviderPickerShowsNoMatches(t *testing.T) {
	d := newLoginDialog()
	d.step = loginStepProvider
	d.method = "apikey"
	d.status = map[string]string{}
	d.providerQuery = "not-a-provider"
	if options := d.providerOptions(); len(options) != 0 {
		t.Fatalf("options = %v", options)
	}
	d.HandleKey(tui.Key{Kind: tui.KeyPageDown})
	if d.cursor != 0 {
		t.Fatalf("cursor = %d", d.cursor)
	}
	if action := d.HandleKey(tui.Key{Kind: tui.KeyEnter}); action != (loginDialogAction{}) {
		t.Fatalf("action = %+v", action)
	}
}

func TestLoginDialogLlamaCPPValidatesURLAndAcceptsOptionalKey(t *testing.T) {
	d := newLoginDialog()
	d.step = loginStepLlamaURL
	d.provider = "llama.cpp"
	d.method = "apikey"
	d.llamaEd = tui.NewEditor("")

	d.llamaEd.SetValue("file:///tmp/router")
	if action := d.HandleKey(tui.Key{Kind: tui.KeyEnter}); action.SaveLlama || d.step != loginStepLlamaURL {
		t.Fatalf("invalid URL advanced dialog: action=%+v step=%v", action, d.step)
	}
	if !strings.Contains(d.message, "http or https") {
		t.Fatalf("validation message = %q", d.message)
	}

	d.llamaEd.SetValue("http://127.0.0.1:8080/v1/")
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if d.step != loginStepLlamaKey || d.llamaURL != "http://127.0.0.1:8080" {
		t.Fatalf("step=%v URL=%q", d.step, d.llamaURL)
	}
	action := d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if !action.SaveLlama || action.LlamaURL != "http://127.0.0.1:8080" || action.LlamaAPIKey != "" {
		t.Fatalf("action = %+v", action)
	}
}

func TestLoginDialogCursorPosMatchesPaddedInputRow(t *testing.T) {
	d := newLoginDialog()
	d.Open(t.TempDir())
	d.method = "oauth"
	d.provider = "anthropic"
	d.ShowWaiting("https://example.com/oauth/authorize?code_challenge=abc&state=xyz")

	// Floating panes render this dialog narrower than the main terminal, so
	// CursorPos must use the identical width to retain wrapping alignment.
	const paneWidth = 62
	lines := padDialogFrame(d.Render(tui.Theme{}, paneWidth))
	row, _ := d.CursorPos(paneWidth)
	if row < 0 || row >= len(lines) {
		t.Fatalf("CursorPos row = %d outside rendered lines %d", row, len(lines))
	}
	if got := stripANSIBytes(lines[row]); !strings.Contains(got, "▌") {
		t.Fatalf("CursorPos row %d = %q; want editor input row", row, got)
	}
}

func TestLoginDialogWaitingShowsCopyURLShortcut(t *testing.T) {
	for _, provider := range []string{"openai-codex", "kimi", "xai", "github-copilot"} {
		t.Run(provider, func(t *testing.T) {
			d := newLoginDialog()
			d.Open(t.TempDir())
			d.method = "oauth"
			d.provider = provider
			d.ShowWaiting("https://example.com/oauth/authorize?state=xyz")

			lines := strings.Join(d.Render(tui.Theme{}, 80), "\n")
			if !strings.Contains(lines, "alt+c copies URL") {
				t.Fatalf("login dialog does not advertise the copy shortcut: %q", lines)
			}
		})
	}
}

func TestInteractiveAltCSendsLoginURLToTerminalClipboard(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", "")

	const loginURL = "https://example.com/oauth/authorize?state=xyz"
	term := &alertTestTerminal{}
	i := NewInteractive(InteractiveConfig{Terminal: term})
	i.dialog.Open(t.TempDir())
	i.dialog.method = "oauth"
	i.dialog.provider = "openai-codex"
	i.dialog.ShowWaiting(loginURL)
	i.dialog.Render(tui.Theme{}, 80)

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: 'c', Alt: true})

	const want = "\x1b]52;c;aHR0cHM6Ly9leGFtcGxlLmNvbS9vYXV0aC9hdXRob3JpemU/c3RhdGU9eHl6\a"
	if got := term.String(); got != want {
		t.Fatalf("terminal output = %q, want OSC 52 copy %q", got, want)
	}
	if got := i.dialog.codeEd.SubmitValue(); got != "" {
		t.Fatalf("authorization code input = %q after alt+c, want unchanged", got)
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.statusOK != "sent login URL to terminal clipboard" || i.statusErr != "" {
		t.Fatalf("status = (%q, %q), want successful terminal copy request", i.statusOK, i.statusErr)
	}
}

func TestInteractiveAltCUsesTerminalClipboardBeforeSystemClipboard(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "wl-copy-called")
	if err := os.WriteFile(filepath.Join(dir, "wl-copy"), []byte("#!/bin/sh\n: > \"$CLIPBOARD_TEST_STATE\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("WAYLAND_DISPLAY", "wayland-test")
	t.Setenv("DISPLAY", "")
	t.Setenv("CLIPBOARD_TEST_STATE", state)

	const loginURL = "https://example.com/oauth/authorize?state=xyz"
	term := &alertTestTerminal{}
	i := NewInteractive(InteractiveConfig{Terminal: term})
	i.dialog.Open(t.TempDir())
	i.dialog.ShowWaiting(loginURL)

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: 'c', Alt: true})

	const want = "\x1b]52;c;aHR0cHM6Ly9leGFtcGxlLmNvbS9vYXV0aC9hdXRob3JpemU/c3RhdGU9eHl6\a"
	if got := term.String(); got != want {
		t.Fatalf("terminal output = %q, want OSC 52 copy %q", got, want)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("system clipboard helper ran: %v", err)
	}
}
