//go:build !darwin && !linux && !windows

package tui

import "fmt"

func ReadClipboardText() (string, bool, error) {
	return "", false, fmt.Errorf("text clipboard is not supported on this platform; use the terminal paste shortcut")
}

func WriteClipboardText(string) error {
	return fmt.Errorf("text clipboard is not supported on this platform")
}
