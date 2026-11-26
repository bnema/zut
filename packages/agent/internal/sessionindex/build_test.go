package sessionindex

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func msg(role provider.Role, text string) provider.Message {
	return provider.Message{Role: role, Content: []provider.Content{provider.TextBlock{Text: text}}}
}

func writeSession(t *testing.T, dir, name, cwd string, hidden bool, msgs ...provider.Message) string {
	t.Helper()
	path := filepath.Join(dir, name)
	s, err := core.NewSessionAtPath(path, cwd, "p", "m", "v")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if err := s.AppendMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if hidden {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		patched := strings.Replace(string(raw), `"meta":{`, `"meta":{"hide_from_sessions":true,`, 1)
		if patched == string(raw) {
			t.Fatal("meta row not found")
		}
		if err := os.WriteFile(path, []byte(patched), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

type fixture struct {
	a, b, hidden string
	idx          *memIndex
}

func buildFixture(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	f := fixture{}
	f.a = writeSession(t, dir, "a.jsonl", "/proj/a", false,
		msg(provider.RoleUser, "Please refactor the parser"),
		msg(provider.RoleTool, "parser tool output"),
		msg(provider.RoleAssistant, "Refactoring done, tests pass"),
		msg(provider.RoleUser, "SECRET-skip parser"),
	)
	f.b = writeSession(t, dir, "b.jsonl", "/proj/b", false,
		msg(provider.RoleUser, "Le problème du parser"),
		msg(provider.RoleAssistant, "Les tests échouent"),
	)
	f.hidden = writeSession(t, dir, "h.jsonl", "/proj/a", true,
		msg(provider.RoleUser, "parser refactoring in hidden branch"),
	)
	opts := Options{Skip: func(m provider.Message) bool {
		for _, c := range m.Content {
			if tb, ok := c.(provider.TextBlock); ok && strings.HasPrefix(tb.Text, "SECRET") {
				return true
			}
		}
		return false
	}}
	var calls int
	idx, err := buildMem(context.Background(), []string{f.a, f.b, f.hidden}, opts, func(done, total int) {
		calls++
		if total != 3 || done > total {
			t.Errorf("progress %d/%d", done, total)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("progress calls = %d", calls)
	}
	f.idx = idx
	return f
}

func paths(r Result) []string {
	var out []string
	for _, s := range r.Sessions {
		out = append(out, filepath.Base(s.Doc.Path))
	}
	sort.Strings(out)
	return out
}

func seqs(m SessionMatch) []int {
	var out []int
	for _, h := range m.Hits {
		out = append(out, h.MsgSeq)
	}
	return out
}

func run(t *testing.T, idx *memIndex, q Query) Result {
	t.Helper()
	r, err := idx.query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBuildDocsAndHidden(t *testing.T) {
	f := buildFixture(t)
	if len(f.idx.docs) != 2 {
		t.Fatalf("docs = %d, want 2 (hidden excluded)", len(f.idx.docs))
	}
	d := f.idx.docs[0]
	if d.Path != f.a || d.CWD != "/proj/a" || d.TotalMsgs != 4 || d.ID == "" || d.Title == "" || d.Updated.IsZero() || d.Size == 0 {
		t.Fatalf("doc a = %+v", d)
	}
	if f.idx.first[1] != 4 || f.idx.nMsgs() != 6 {
		t.Fatalf("first = %v", f.idx.first)
	}
	if !sort.StringsAreSorted(f.idx.terms) {
		t.Fatal("terms not sorted")
	}
	if got := run(t, f.idx, Query{Text: "hidden"}); len(got.Sessions) != 0 {
		t.Fatalf("hidden session indexed: %v", paths(got))
	}
}

func TestQueryPrefixSkipAndRows(t *testing.T) {
	f := buildFixture(t)
	r := run(t, f.idx, Query{Text: "Refacto", Mode: ModeMessages})
	if got := paths(r); len(got) != 1 || got[0] != "a.jsonl" {
		t.Fatalf("refacto sessions = %v", got)
	}
	// msg 0 "refactor", msg 2 "refactoring"; tool row 1 and skipped row 3 are absent.
	if got := seqs(r.Sessions[0]); len(got) != 2 || got[0] != 0 || got[1] != 2 {
		t.Fatalf("refacto hits = %v", got)
	}
	r = run(t, f.idx, Query{Text: "parser", Mode: ModeMessages})
	for _, s := range r.Sessions {
		for _, h := range s.Hits {
			if h.Path == f.a && (h.MsgSeq == 1 || h.MsgSeq == 3) {
				t.Fatalf("tool or skipped row indexed: %+v", h)
			}
			row, err := core.ReadSessionTextRow(h.Path, h.Offset, h.Len)
			if err != nil {
				t.Fatal(err)
			}
			if row.Fingerprint != h.Fingerprint || row.Role != h.Role || !strings.Contains(strings.ToLower(strings.Join(row.Texts, " ")), "parser") {
				t.Fatalf("hit %+v does not resolve: %+v", h, row)
			}
			if h.TotalMsgs == 0 {
				t.Fatalf("TotalMsgs unset: %+v", h)
			}
		}
	}
	if got := run(t, f.idx, Query{Text: "secret"}); len(got.Sessions) != 0 {
		t.Fatal("skipped message indexed")
	}
}

func TestQueryModes(t *testing.T) {
	f := buildFixture(t)
	// "problème" and "tests" are in different messages of b, same message nowhere.
	if got := paths(run(t, f.idx, Query{Text: "problème tests", Mode: ModeSessions})); len(got) != 1 || got[0] != "b.jsonl" {
		t.Fatalf("session AND = %v", got)
	}
	if got := run(t, f.idx, Query{Text: "problème tests", Mode: ModeMessages}); len(got.Sessions) != 0 {
		t.Fatalf("message AND = %v", paths(got))
	}
	// "refactoring tests" share msg 2 of a.
	r := run(t, f.idx, Query{Text: "refactoring tests", Mode: ModeMessages})
	if got := paths(r); len(got) != 1 || got[0] != "a.jsonl" || r.Sessions[0].Count != 1 {
		t.Fatalf("message AND = %v", got)
	}
	// Session mode matches "parser te" across messages in both sessions.
	if got := paths(run(t, f.idx, Query{Text: "parser te", Mode: ModeSessions})); len(got) != 2 {
		t.Fatalf("session prefix AND = %v", got)
	}
	if got := paths(run(t, f.idx, Query{Text: "pars", CWD: "/proj/b"})); len(got) != 1 || got[0] != "b.jsonl" {
		t.Fatalf("cwd filter = %v", got)
	}
	if got := run(t, f.idx, Query{Text: "parser", Limit: 1}); len(got.Sessions) != 1 {
		t.Fatalf("limit = %d", len(got.Sessions))
	}
}

func TestBuildCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := buildMem(ctx, []string{"x"}, Options{}, nil); err == nil {
		t.Fatal("expected cancellation error")
	}
}
