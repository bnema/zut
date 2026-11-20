package tui

import "testing"

func TestExtractPartialStringField(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		field     string
		wantValue string
		wantOK    bool
		wantDone  bool
	}{
		{
			name:      "empty buffer",
			raw:       "",
			field:     "content",
			wantValue: "",
			wantOK:    false,
			wantDone:  false,
		},
		{
			name:      "no such field",
			raw:       `{"path":"/x","foo":"bar"}`,
			field:     "content",
			wantValue: "",
			wantOK:    false,
			wantDone:  false,
		},
		{
			name:      "complete",
			raw:       `{"path":"/x","content":"hello"}`,
			field:     "content",
			wantValue: "hello",
			wantOK:    true,
			wantDone:  true,
		},
		{
			name:      "partial mid-word",
			raw:       `{"path":"/x","content":"hel`,
			field:     "content",
			wantValue: "hel",
			wantOK:    true,
			wantDone:  false,
		},
		{
			name:      "escaped quote",
			raw:       `{"content":"say \"hi\""}`,
			field:     "content",
			wantValue: `say "hi"`,
			wantOK:    true,
			wantDone:  true,
		},
		{
			name:      "escaped newline inside string",
			raw:       `{"content":"line1\nline2"}`,
			field:     "content",
			wantValue: "line1\nline2",
			wantOK:    true,
			wantDone:  true,
		},
		{
			name:      "trailing backslash (unfinished escape)",
			raw:       `{"content":"line1\`,
			field:     "content",
			wantValue: "line1",
			wantOK:    true,
			wantDone:  false,
		},
		{
			name:      "incomplete unicode escape",
			raw:       `{"content":"before\u00`,
			field:     "content",
			wantValue: "before",
			wantOK:    true,
			wantDone:  false,
		},
		{
			name:      "key before value",
			raw:       `{"content":`,
			field:     "content",
			wantValue: "",
			wantOK:    false,
			wantDone:  false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, ok, done := ExtractPartialStringField(c.raw, c.field)
			if v != c.wantValue {
				t.Errorf("value: want %q, got %q", c.wantValue, v)
			}
			if ok != c.wantOK {
				t.Errorf("ok: want %v, got %v", c.wantOK, ok)
			}
			if done != c.wantDone {
				t.Errorf("done: want %v, got %v", c.wantDone, done)
			}
		})
	}
}

func TestExtractCurrentEdit(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want LiveEdit
	}{
		{
			name: "second edit newText partial",
			raw:  `{"edits":[{"oldText":"x","newText":"y"},{"oldText":"a\nb","newText":"c\nd`,
			want: LiveEdit{Idx: 2, OldText: "a\nb", OldDone: true, NewText: "c\nd"},
		},
		{
			name: "third oldText started",
			raw:  `{"edits":[{"oldText":"x","newText":"y"},{"oldText":"a","newText":"b"},{"oldText":"q`,
			want: LiveEdit{Idx: 3, OldText: "q"},
		},
		{
			name: "no edits yet",
			raw:  `{"path":"a.go","edits":[`,
			want: LiveEdit{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExtractCurrentEdit(c.raw); got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestExtractLastStringField(t *testing.T) {
	cases := []struct {
		name, field, raw string
		value            string
		ok, done         bool
		idx              int
	}{
		{"missing", "newText", `{"edits":[{"oldText":"a"}`, "", false, false, 0},
		{"key without value yet", "newText", `{"edits":[{"oldText":"a","newText":`, "", false, false, 1},
		{"complete", "newText", `{"edits":[{"oldText":"a","newText":"b"}]`, "b", true, true, 1},
		{"last of several, escaped", "a", `{"a":"1","a":"two\tx`, "two\tx", true, false, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, ok, done, idx := ExtractLastStringField(c.raw, c.field)
			if v != c.value || ok != c.ok || done != c.done || idx != c.idx {
				t.Fatalf("got (%q %v %v %d), want (%q %v %v %d)", v, ok, done, idx, c.value, c.ok, c.done, c.idx)
			}
		})
	}
}
