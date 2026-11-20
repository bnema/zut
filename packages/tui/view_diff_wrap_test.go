package tui

import (
	"strings"
	"testing"
)

func TestRenderDiffRowWrapsWithoutLosingContent(t *testing.T) {
	const width = 30
	code := `const message = "alpha beta gamma delta epsilon"`
	// Content-preservation check: no tint, so rows carry no padding.
	th := Dark
	th.DiffAddBG, th.DiffDelBG = nil, nil
	v := View{Theme: th}

	rows := v.renderDiffRow("+"+code, width, v.Theme.Tool, 7, '+', "sample.go")
	if len(rows) < 2 {
		t.Fatalf("long diff line did not wrap: %q", rows)
	}

	var rebuilt strings.Builder
	for i, row := range rows {
		plain := stripANSI(row)
		if got := visibleWidth(row); got > width {
			t.Errorf("row %d width = %d, want <= %d: %q", i, got, width, plain)
		}
		prefix := "           "
		if i == 0 {
			prefix = "       7 + "
		}
		if !strings.HasPrefix(plain, prefix) {
			t.Fatalf("row %d prefix = %q, want %q", i, plain, prefix)
		}
		rebuilt.WriteString(strings.TrimPrefix(plain, prefix))
	}
	if got := rebuilt.String(); got != code {
		t.Fatalf("wrapped content changed:\n got: %q\nwant: %q", got, code)
	}
	if strings.Contains(stripANSI(strings.Join(rows, "\n")), "...") {
		t.Fatalf("wrapped diff still contains truncation ellipsis: %q", rows)
	}
}

func TestRenderDiffRowWrapsUnicodeByDisplayWidth(t *testing.T) {
	const width = 18
	code := "名前 = 🙂🙂🙂 café résumé"
	// Content-preservation check: no tint, so rows carry no padding.
	th := Dark
	th.DiffAddBG, th.DiffDelBG = nil, nil
	v := View{Theme: th}

	rows := v.renderDiffRow("-"+code, width, v.Theme.Error, 12, '-', "")
	if len(rows) < 2 {
		t.Fatalf("long Unicode diff line did not wrap: %q", rows)
	}

	var rebuilt strings.Builder
	for i, row := range rows {
		plain := stripANSI(row)
		if got := visibleWidth(row); got > width {
			t.Errorf("row %d width = %d, want <= %d: %q", i, got, width, plain)
		}
		prefix := "           "
		if i == 0 {
			prefix = "      12 - "
		}
		if !strings.HasPrefix(plain, prefix) {
			t.Fatalf("row %d prefix = %q, want %q", i, plain, prefix)
		}
		rebuilt.WriteString(strings.TrimPrefix(plain, prefix))
	}
	if got := rebuilt.String(); got != code {
		t.Fatalf("wrapped Unicode content changed:\n got: %q\nwant: %q", got, code)
	}
}
