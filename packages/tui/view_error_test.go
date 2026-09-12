package tui

import (
	"strings"
	"testing"
)

func TestRenderErrSplitsExplicitNewlines(t *testing.T) {
	v := &View{Theme: Theme{Error: Color256(1)}, Err: "failed to start\n\n  exec: python3\nstderr log: /tmp/ext.log"}
	want := []string{"✖ failed to start", "  ", "    exec: python3", "  stderr log: /tmp/ext.log"}
	rows := v.renderErr(80)
	if len(rows) != len(want) {
		t.Fatalf("rows = %q, want %d rows", rows, len(want))
	}
	for n, row := range rows {
		if expected := v.Theme.FGColor(v.Theme.Error, want[n]); row != expected {
			t.Errorf("row %d = %q, want %q", n, row, expected)
		}
	}
	for _, row := range v.renderErr(20) {
		if strings.ContainsAny(row, "\r\n") {
			t.Errorf("row contains a newline: %q", row)
		}
		if width := visibleWidth(row); width > 20 {
			t.Errorf("row width = %d, want <= 20: %q", width, row)
		}
	}
}
