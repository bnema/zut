package tui

import "testing"

func TestSanitizePlainText(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"\x1b]52;c;ZXZpbA==\x07hello", "hello"},
		{"\x1b]52;c;ZXZpbA==\x1b\\hello", "hello"},
		{"\x1b[31mhello\x1b[0m", "hello"},
		{"hello\x1b", "hello"},
		{"a\x07\x9b\u009b\x7f\x00\xff\u202eb", "ab"},
		{"été français\n中文 😀\toui", "été français\n中文 😀 oui"},
	} {
		if got := SanitizePlainText(tc.in); got != tc.want {
			t.Errorf("SanitizePlainText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
