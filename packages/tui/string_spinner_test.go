package tui

import (
	"testing"
	"time"
)

func TestStringSpinnerAdvancesAtConfiguredInterval(t *testing.T) {
	spinner := NewStringSpinner([]string{"Zzzz", "zZzz", "zzZz", "zzzZ"}, time.Second)
	started := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		elapsed time.Duration
		want    string
	}{
		{elapsed: 0, want: "Zzzz"},
		{elapsed: 999 * time.Millisecond, want: "Zzzz"},
		{elapsed: time.Second, want: "zZzz"},
		{elapsed: 2 * time.Second, want: "zzZz"},
		{elapsed: 3 * time.Second, want: "zzzZ"},
		{elapsed: 4 * time.Second, want: "Zzzz"},
	}
	for _, test := range tests {
		if got := spinner.FrameAt(started, started.Add(test.elapsed)); got != test.want {
			t.Fatalf("FrameAt(%s) = %q, want %q", test.elapsed, got, test.want)
		}
	}
}
