package tui

import (
	"strings"
	"testing"
	"unicode"
)

// The spinner message is interpolated straight into the single-line
// status bar, so the default list has to stay well-formed: blank or
// untrimmed entries leave a ragged gap, control characters would break
// the line, and duplicates would quietly skew which quips show up.
func TestDefaultSpinnerMessagesAreUsable(t *testing.T) {
	if len(defaultSpinnerMessages) == 0 {
		t.Fatal("default spinner messages must not be empty")
	}

	seen := make(map[string]int, len(defaultSpinnerMessages))
	for i, msg := range defaultSpinnerMessages {
		if strings.TrimSpace(msg) == "" {
			t.Fatalf("message %d is blank", i)
		}
		if msg != strings.TrimSpace(msg) {
			t.Fatalf("message %d = %q, want surrounding whitespace trimmed", i, msg)
		}
		if strings.ContainsFunc(msg, unicode.IsControl) {
			t.Fatalf("message %d = %q, want no control characters", i, msg)
		}
		if prev, dup := seen[msg]; dup {
			t.Fatalf("message %d = %q duplicates message %d", i, msg, prev)
		}
		seen[msg] = i
	}
}
