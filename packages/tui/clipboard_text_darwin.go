//go:build darwin

package tui

import "fmt"

// ReadClipboardText reads plain text from the macOS system clipboard.
func ReadClipboardText() (string, bool, error) {
	text, ok, err := readClipboardTextCommands(clipboardTextCommand{name: "/usr/bin/pbpaste"})
	if err == errClipboardCommandUnavailable {
		return "", false, fmt.Errorf("text clipboard unavailable: pbpaste was not found")
	}
	return text, ok, err
}

// WriteClipboardText writes plain text to the macOS system clipboard.
func WriteClipboardText(text string) error {
	if err := writeClipboardTextCommands(text, clipboardTextCommand{name: "/usr/bin/pbcopy"}); err != nil {
		if err == errClipboardCommandUnavailable {
			return fmt.Errorf("text clipboard unavailable: pbcopy was not found")
		}
		return fmt.Errorf("write text clipboard: %w", err)
	}
	return nil
}
