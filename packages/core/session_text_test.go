package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
)

func sessionTextLine(t *testing.T, line sessionLine) string {
	t.Helper()
	data, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n"
}

func sessionTextMeta(t *testing.T, id string) string {
	return sessionTextLine(t, sessionLine{Type: "meta", Meta: &SessionMeta{ID: id, CWD: "/w"}})
}

func sessionTextMsg(t *testing.T, msg provider.Message) string {
	return sessionTextLine(t, sessionLine{Type: "message", Message: &msg})
}

func textMsg(role provider.Role, blocks ...provider.Content) provider.Message {
	return provider.Message{Role: role, Content: blocks, Time: time.Unix(1700000000, 0).UTC()}
}

func scanAll(t *testing.T, path string, from SessionScanCursor) ([]SessionTextRow, SessionScanCursor, SessionMeta) {
	t.Helper()
	var rows []SessionTextRow
	cur, meta, err := ScanSessionText(context.Background(), path, from, func(r SessionTextRow) error {
		rows = append(rows, r)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return rows, cur, meta
}

func writeSessionText(t *testing.T, path string, parts ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(parts, "")), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestScanSessionTextFingerprintAndRoles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	user := textMsg(provider.RoleUser, provider.TextBlock{Text: "hello"}, provider.ImageBlock{MimeType: "image/png", Data: []byte{1, 2}})
	assistant := textMsg(provider.RoleAssistant,
		provider.ReasoningBlock{ID: "r1", Summary: "secret thoughts"},
		provider.TextBlock{Text: "answer"},
		provider.ToolCallBlock{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"x"}`)},
		provider.TextBlock{Text: "more"})
	tool := textMsg(provider.RoleTool, provider.ToolResultBlock{CallID: "c1", Content: []provider.Content{provider.TextBlock{Text: "tool out"}}})
	toolOnly := textMsg(provider.RoleAssistant, provider.ToolCallBlock{ID: "c0", Name: "ls", Arguments: json.RawMessage(`{}`)})
	dev := textMsg(provider.RoleDeveloper, provider.TextBlock{Text: "dev note"})
	compaction := []provider.Message{textMsg(provider.RoleUser, provider.TextBlock{Text: "summary"})}
	writeSessionText(t, path,
		sessionTextMeta(t, "s1"),
		sessionTextMsg(t, dev),
		sessionTextMsg(t, user),
		sessionTextMsg(t, toolOnly),
		sessionTextMsg(t, tool),
		sessionTextLine(t, sessionLine{Type: "compaction", Messages: &compaction}),
		sessionTextMsg(t, assistant),
	)

	rows, cur, meta := scanAll(t, path, SessionScanCursor{})
	if meta.ID != "s1" || cur.MetaID != "s1" || cur.Reset {
		t.Fatalf("meta=%+v cur=%+v", meta, cur)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2: %+v", len(rows), rows)
	}
	if rows[0].MsgSeq != 1 || rows[1].MsgSeq != 4 || cur.MsgSeq != 5 {
		t.Fatalf("seqs = %d,%d next=%d", rows[0].MsgSeq, rows[1].MsgSeq, cur.MsgSeq)
	}
	if got := strings.Join(rows[1].Texts, "|"); got != "answer|more" {
		t.Fatalf("assistant texts = %q", got)
	}
	if strings.Join(rows[0].Texts, "|") != "hello" || rows[0].Role != provider.RoleUser {
		t.Fatalf("user row = %+v", rows[0])
	}
	for i, want := range []provider.Message{user, assistant} {
		raw, _ := json.Marshal(want)
		decoded, err := DecodeMessageJSON(raw)
		if err != nil {
			t.Fatal(err)
		}
		if rows[i].Fingerprint != MessageTextFingerprint(decoded) || rows[i].Fingerprint != MessageTextFingerprint(rows[i].Message) {
			t.Fatalf("row %d fingerprint mismatch", i)
		}
	}
	if MessageTextFingerprint(user) == MessageTextFingerprint(textMsg(provider.RoleAssistant, provider.TextBlock{Text: "hello"})) {
		t.Fatal("fingerprint ignores role")
	}
	if MessageTextFingerprint(textMsg(provider.RoleUser, provider.TextBlock{Text: "ab"})) == MessageTextFingerprint(textMsg(provider.RoleUser, provider.TextBlock{Text: "a"}, provider.TextBlock{Text: "b"})) {
		t.Fatal("fingerprint ignores block boundaries")
	}
	info, _ := os.Stat(path)
	if cur.Offset != info.Size() {
		t.Fatalf("offset = %d, size %d", cur.Offset, info.Size())
	}

	got, err := readTextRow(t, path, rows[1].Offset, rows[1].Len)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fingerprint != rows[1].Fingerprint || MessageTextFingerprint(got.Message) != got.Fingerprint || strings.Join(got.Texts, "|") != "answer|more" || got.Role != provider.RoleAssistant || !got.Time.Equal(assistant.Time) {
		t.Fatalf("round trip = %+v", got)
	}
	if _, err := readTextRow(t, path, 0, rows[0].Offset-1); err == nil {
		t.Fatal("meta row read as text row")
	}
}

func TestScanSessionTextPartialLineAndResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	first := sessionTextMsg(t, textMsg(provider.RoleUser, provider.TextBlock{Text: "one"}))
	second := sessionTextMsg(t, textMsg(provider.RoleAssistant, provider.TextBlock{Text: "two"}))
	half := len(second) / 2
	later := sessionTextLine(t, sessionLine{Type: "meta", Meta: &SessionMeta{ID: "s1", CWD: "/w", Model: "later"}})
	writeSessionText(t, path, sessionTextMeta(t, "s1"), later, first, second[:half])

	rows, cur, fullMeta := scanAll(t, path, SessionScanCursor{})
	if len(rows) != 1 || rows[0].Texts[0] != "one" {
		t.Fatalf("rows = %+v", rows)
	}
	info, _ := os.Stat(path)
	if cur.Offset != info.Size()-int64(half) {
		t.Fatalf("partial line consumed: offset %d size %d", cur.Offset, info.Size())
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	third := sessionTextMsg(t, textMsg(provider.RoleUser, provider.TextBlock{Text: "three"}))
	if _, err := f.WriteString(second[half:] + third); err != nil {
		t.Fatal(err)
	}
	f.Close()

	rows, next, resumedMeta := scanAll(t, path, cur)
	if fullMeta.Model != "" || resumedMeta.Model != fullMeta.Model || resumedMeta.ID != "s1" {
		t.Fatalf("meta full=%+v resumed=%+v", fullMeta, resumedMeta)
	}
	if next.Reset || len(rows) != 2 || rows[0].Texts[0] != "two" || rows[1].Texts[0] != "three" {
		t.Fatalf("resumed rows = %+v cur=%+v", rows, next)
	}
	if rows[0].MsgSeq != 1 || rows[1].MsgSeq != 2 || next.MsgSeq != 3 {
		t.Fatalf("seqs %d %d next %d", rows[0].MsgSeq, rows[1].MsgSeq, next.MsgSeq)
	}
	again, _, _ := scanAll(t, path, next)
	if len(again) != 0 {
		t.Fatalf("rescan emitted %d rows", len(again))
	}
}

func TestScanSessionTextOversizedRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	huge := sessionTextMsg(t, textMsg(provider.RoleUser, provider.TextBlock{Text: strings.Repeat("x", 2<<20)}))
	hugeOther := sessionTextLine(t, sessionLine{Type: "rename", Title: strings.Repeat("y", 2<<20)})
	writeSessionText(t, path, sessionTextMeta(t, "s1"), huge, hugeOther,
		sessionTextMsg(t, textMsg(provider.RoleUser, provider.TextBlock{Text: "after"})))

	rows, cur, _ := scanAll(t, path, SessionScanCursor{})
	if len(rows) != 1 || rows[0].Texts[0] != "after" || rows[0].MsgSeq != 1 || cur.MsgSeq != 2 {
		t.Fatalf("rows = %+v cur=%+v", rows, cur)
	}
	got, err := readTextRow(t, path, rows[0].Offset, rows[0].Len)
	if err != nil || got.Texts[0] != "after" {
		t.Fatalf("round trip = %+v err=%v", got, err)
	}
}

func TestScanSessionTextReset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	msg := sessionTextMsg(t, textMsg(provider.RoleUser, provider.TextBlock{Text: "one"}))
	writeSessionText(t, path, sessionTextMeta(t, "s1"), msg, msg)
	_, cur, _ := scanAll(t, path, SessionScanCursor{})

	// Replaced by a different session.
	writeSessionText(t, path, sessionTextMeta(t, "s2"), msg, msg, msg)
	rows, next, meta := scanAll(t, path, cur)
	if !next.Reset || len(rows) != 3 || meta.ID != "s2" || next.MetaID != "s2" || next.MsgSeq != 3 {
		t.Fatalf("replace: rows=%d cur=%+v", len(rows), next)
	}

	// Truncated below the cursor.
	writeSessionText(t, path, sessionTextMeta(t, "s2"), msg)
	rows, next2, _ := scanAll(t, path, next)
	if !next2.Reset || len(rows) != 1 {
		t.Fatalf("truncate: rows=%d cur=%+v", len(rows), next2)
	}

	// Matching cursor: no reset.
	_, next3, _ := scanAll(t, path, next2)
	if next3.Reset {
		t.Fatal("unexpected reset")
	}
}

func TestScanSessionTextCallbackErrorAndCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	msg := sessionTextMsg(t, textMsg(provider.RoleUser, provider.TextBlock{Text: "one"}))
	writeSessionText(t, path, sessionTextMeta(t, "s1"), msg, msg)
	stop := errors.New("stop")
	cur, _, err := ScanSessionText(context.Background(), path, SessionScanCursor{}, func(SessionTextRow) error { return stop })
	if !errors.Is(err, stop) || cur.MsgSeq != 0 {
		t.Fatalf("err=%v cur=%+v", err, cur)
	}
	rows, _, _ := scanAll(t, path, cur)
	if len(rows) != 2 {
		t.Fatalf("retry rows = %d", len(rows))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := ScanSessionText(ctx, path, SessionScanCursor{}, func(SessionTextRow) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestSessionHeadHashMatchesCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeSessionText(t, path, sessionTextMeta(t, "s1"), sessionTextMsg(t, textMsg(provider.RoleUser, provider.TextBlock{Text: "hi"})))
	_, cur, _ := scanAll(t, path, SessionScanCursor{})
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h, ok, err := SessionHeadHash(f)
	if err != nil || !ok || h != cur.HeadHash {
		t.Fatalf("head hash = %x ok=%v err=%v, cursor %x", h, ok, err, cur.HeadHash)
	}
	if _, ok, _ := SessionHeadHash(strings.NewReader(`{"type":"meta"`)); ok {
		t.Fatal("partial first row reported complete")
	}
}

func TestScanSessionTextSawMetaChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	user := sessionTextMsg(t, textMsg(provider.RoleUser, provider.TextBlock{Text: "one"}))
	writeSessionText(t, path, sessionTextMeta(t, "s1"), user)
	_, cur, _ := scanAll(t, path, SessionScanCursor{})
	if cur.SawMetaChange {
		t.Fatal("plain messages reported a meta change")
	}
	cases := map[string]string{
		"message":    user,
		"rename":     sessionTextLine(t, sessionLine{Type: "rename", Title: "new"}),
		"meta":       sessionTextMeta(t, "s1"),
		"compaction": sessionTextLine(t, sessionLine{Type: "compaction", Messages: &[]provider.Message{}}),
		"malformed":  "{not json\n",
		"untyped":    "{}\n",
	}
	for name, row := range cases {
		base, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		tail := filepath.Join(t.TempDir(), "t.jsonl")
		writeSessionText(t, tail, string(base), row)
		_, got, _ := scanAll(t, tail, cur)
		if got.Reset || got.SawMetaChange != (name != "message") {
			t.Fatalf("%s: cursor = %+v", name, got)
		}
	}
	// SawMetaChange is not carried over from the input cursor.
	in := cur
	in.SawMetaChange = true
	if _, got, _ := scanAll(t, path, in); got.SawMetaChange {
		t.Fatal("input SawMetaChange leaked")
	}
}

func TestScanSessionTextRowAtLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	small := sessionTextMsg(t, textMsg(provider.RoleUser, provider.TextBlock{Text: ""}))
	pad := maxSessionSearchRowBytes - (len(small) - 1)
	exact := sessionTextMsg(t, textMsg(provider.RoleUser, provider.TextBlock{Text: strings.Repeat("x", pad)}))
	if len(exact)-1 != maxSessionSearchRowBytes {
		t.Fatalf("row size = %d", len(exact)-1)
	}
	writeSessionText(t, path, sessionTextMeta(t, "s1"), exact)
	rows, _, _ := scanAll(t, path, SessionScanCursor{})
	if len(rows) != 1 || rows[0].Len != maxSessionSearchRowBytes {
		t.Fatalf("rows at limit = %d", len(rows))
	}
	if _, err := readTextRow(t, path, rows[0].Offset, rows[0].Len); err != nil {
		t.Fatal(err)
	}
}

func readTextRow(t *testing.T, path string, off, n int64) (SessionTextRow, error) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return ReadSessionTextRowAt(f, off, n)
}
