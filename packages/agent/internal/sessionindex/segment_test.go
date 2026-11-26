package sessionindex

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

// normalize drops time locations so mem and disk results compare by instant.
func normalize(r Result) Result {
	for i := range r.Sessions {
		s := &r.Sessions[i]
		s.Doc.Started = s.Doc.Started.UTC()
		s.Doc.Updated = s.Doc.Updated.UTC()
		for j := range s.Hits {
			s.Hits[j].Time = s.Hits[j].Time.UTC()
		}
	}
	return r
}

func writeFixtureSegment(t *testing.T, f fixture) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "idx", "sessions.idx")
	if err := writeSegment(path, f.idx); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSegmentRoundTrip(t *testing.T) {
	f := buildFixture(t)
	path := writeFixtureSegment(t, f)
	seg, err := openSegment(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { seg.Close() })
	if len(seg.docs) != len(f.idx.docs) {
		t.Fatalf("docs = %d", len(seg.docs))
	}
	for i, d := range seg.docs {
		m := f.idx.docs[i]
		if d.Path != m.Path || d.ID != m.ID || d.CWD != m.CWD || d.Title != m.Title || !d.Started.Equal(m.Started) ||
			!d.Updated.Equal(m.Updated) || d.Size != m.Size || d.Cursor != m.Cursor || d.TotalMsgs != m.TotalMsgs {
			t.Fatalf("doc %d = %+v, want %+v", i, d, m)
		}
	}
	queries := []Query{
		{Text: "parser", Mode: ModeMessages}, {Text: "refacto"}, {Text: "problème tests"},
		{Text: "problème tests", Mode: ModeMessages}, {Text: "pa te"}, {Text: "pars", CWD: "/proj/b"},
		{Text: "parser", Limit: 1}, {Text: "nothing"}, {Text: "zzz"},
	}
	for _, q := range queries {
		want, err := f.idx.query(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		got, err := seg.query(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(normalize(got), normalize(want)) {
			t.Fatalf("query %+v:\n got %+v\nwant %+v", q, got, want)
		}
	}
	// Only the dictionary and tables are resident: no postings or rows.
	if len(seg.terms) != func() int {
		n := 0
		for _, t := range f.idx.terms {
			n += len(t)
		}
		return n
	}() || len(seg.postOffs) != len(f.idx.terms)+1 {
		t.Fatal("unexpected resident sizes")
	}
}

func TestSegmentEmpty(t *testing.T) {
	m := mergeDocs(nil)
	path := filepath.Join(t.TempDir(), "e.idx")
	if err := writeSegment(path, m); err != nil {
		t.Fatal(err)
	}
	seg, err := openSegment(path)
	if err != nil {
		t.Fatal(err)
	}
	defer seg.Close()
	if r, err := seg.query(context.Background(), Query{Text: "x1"}); err != nil || len(r.Sessions) != 0 {
		t.Fatalf("empty query = %+v, %v", r, err)
	}
}

func TestSegmentCorrupt(t *testing.T) {
	f := buildFixture(t)
	path := writeFixtureSegment(t, f)
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func([]byte){
		"body byte":  func(b []byte) { b[len(b)-5] ^= 0xff },
		"magic":      func(b []byte) { b[0] = 'X' },
		"format":     func(b []byte) { binary.LittleEndian.PutUint16(b[8:], formatVersion+1) },
		"tokenizer":  func(b []byte) { binary.LittleEndian.PutUint16(b[10:], tokenizerVersion+1) },
		"offsets":    func(b []byte) { binary.LittleEndian.PutUint64(b[40:], 1<<40) },
		"doc count":  func(b []byte) { binary.LittleEndian.PutUint32(b[16:], 99) },
		"truncation": nil,
	}
	for name, mutate := range cases {
		b := append([]byte(nil), orig...)
		if mutate == nil {
			b = b[:len(b)-10]
		} else {
			mutate(b)
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		seg, err := openSegment(path)
		if !errors.Is(err, ErrCorrupt) {
			if seg != nil {
				seg.Close()
			}
			t.Errorf("%s: err = %v, want ErrCorrupt", name, err)
		}
	}
}

func TestSegmentPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	f := buildFixture(t)
	path := writeFixtureSegment(t, f)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("modes file=%v dir=%v", fi.Mode().Perm(), di.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("leftover temp files: %d", len(entries))
	}
	// Rewriting over an existing file replaces it.
	if err := writeSegment(path, f.idx); err != nil {
		t.Fatal(err)
	}
}
