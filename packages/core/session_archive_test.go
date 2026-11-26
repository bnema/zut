package core

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func archiveText(role provider.Role, s string) provider.Message {
	return textMsg(role, provider.TextBlock{Text: s})
}

func archiveSummary(tokens string) provider.Message {
	m := archiveText(provider.RoleUser, "## Context Summary (compacted)\n\nsum")
	m.Meta = map[string]string{"compaction": "true", "tokens_before": tokens}
	return m
}

func archiveCompactionLine(t *testing.T, msgs ...provider.Message) string {
	return sessionTextLine(t, sessionLine{Type: "compaction", Messages: &msgs})
}

func archiveTexts(rows []ArchivedRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, messageTexts(r.Message)...)
	}
	return out
}

func assertStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

func readArchive(t *testing.T, path string, opt ArchivedOptions) ArchivedHistory {
	t.Helper()
	h, err := ReadArchivedSessionText(context.Background(), path, opt)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestReadArchivedNoCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	ctxMsg := provider.Message{Role: provider.RoleDeveloper, Content: []provider.Content{provider.TextBlock{Text: "ctx"}}, Meta: map[string]string{internalContextMarker: "true"}}
	writeSessionText(t, path, sessionTextMeta(t, "s"),
		sessionTextMsg(t, archiveText(provider.RoleUser, "a")),
		// Summary-less dynamic-context compaction row: not real.
		archiveCompactionLine(t, ctxMsg, archiveText(provider.RoleUser, "a")),
		sessionTextMsg(t, archiveText(provider.RoleAssistant, "b")))
	h := readArchive(t, path, ArchivedOptions{})
	if len(h.Rows) != 0 || len(h.Marks) != 0 || h.Older != 0 {
		t.Fatalf("history = %+v", h)
	}
}

func TestReadArchivedOneCompactionDedupesTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	tool := provider.Message{Role: provider.RoleTool, Content: []provider.Content{provider.TextBlock{Text: "tool"}}}
	writeSessionText(t, path, sessionTextMeta(t, "s"),
		sessionTextMsg(t, archiveText(provider.RoleUser, "u1")),
		sessionTextMsg(t, tool),
		sessionTextMsg(t, archiveText(provider.RoleAssistant, "a1")),
		sessionTextMsg(t, archiveText(provider.RoleUser, "u2")),
		sessionTextMsg(t, archiveText(provider.RoleAssistant, "a2")),
		archiveCompactionLine(t, archiveSummary("1234"),
			archiveText(provider.RoleUser, "u2"), archiveText(provider.RoleAssistant, "a2")),
		sessionTextMsg(t, archiveText(provider.RoleUser, "u3")))
	h := readArchive(t, path, ArchivedOptions{})
	assertStrings(t, archiveTexts(h.Rows), []string{"u1", "a1"})
	if h.Rows[1].MsgSeq != 2 {
		t.Fatalf("MsgSeq = %d", h.Rows[1].MsgSeq)
	}
	if len(h.Marks) != 1 || h.Marks[0] != (ArchivedMark{Index: 2, TokensBefore: 1234}) {
		t.Fatalf("marks = %+v", h.Marks)
	}
}

func twoCompactionSession(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeSessionText(t, path, sessionTextMeta(t, "s"),
		sessionTextMsg(t, archiveText(provider.RoleUser, "u1")),
		sessionTextMsg(t, archiveText(provider.RoleAssistant, "a1")),
		archiveCompactionLine(t, archiveSummary("10"), archiveText(provider.RoleAssistant, "a1")),
		sessionTextMsg(t, archiveText(provider.RoleUser, "u2")),
		sessionTextMsg(t, archiveText(provider.RoleAssistant, "a2")),
		sessionTextMsg(t, archiveText(provider.RoleUser, "u3")),
		archiveCompactionLine(t, archiveSummary("20"), archiveText(provider.RoleUser, "u3")),
		sessionTextMsg(t, archiveText(provider.RoleAssistant, "a3")))
	return path
}

func TestReadArchivedTwoCompactions(t *testing.T) {
	h := readArchive(t, twoCompactionSession(t), ArchivedOptions{})
	assertStrings(t, archiveTexts(h.Rows), []string{"u1", "a1", "u2", "a2"})
	want := []ArchivedMark{{Index: 2, TokensBefore: 10}, {Index: 4, TokensBefore: 20}}
	if len(h.Marks) != 2 || h.Marks[0] != want[0] || h.Marks[1] != want[1] {
		t.Fatalf("marks = %+v", h.Marks)
	}
}

func TestReadArchivedSkip(t *testing.T) {
	h := readArchive(t, twoCompactionSession(t), ArchivedOptions{Skip: func(m provider.Message) bool {
		return m.Role == provider.RoleUser
	}})
	assertStrings(t, archiveTexts(h.Rows), []string{"a1", "a2"})
	if len(h.Marks) != 2 || h.Marks[0].Index != 1 || h.Marks[1].Index != 2 {
		t.Fatalf("marks = %+v", h.Marks)
	}
}

func TestReadArchivedLimit(t *testing.T) {
	path := twoCompactionSession(t)
	h := readArchive(t, path, ArchivedOptions{Limit: 3})
	assertStrings(t, archiveTexts(h.Rows), []string{"a1", "u2", "a2"})
	if h.Older != 1 || len(h.Marks) != 2 || h.Marks[0].Index != 1 || h.Marks[1].Index != 3 {
		t.Fatalf("history = %+v", h)
	}
	h = readArchive(t, path, ArchivedOptions{Limit: 1})
	assertStrings(t, archiveTexts(h.Rows), []string{"a2"})
	if h.Older != 3 || len(h.Marks) != 1 || h.Marks[0] != (ArchivedMark{Index: 1, TokensBefore: 20}) {
		t.Fatalf("history = %+v", h)
	}
}

func TestReadArchivedContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ReadArchivedSessionText(ctx, twoCompactionSession(t), ArchivedOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestReadArchivedLimitTrimsLongHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	parts := []string{sessionTextMeta(t, "s")}
	for _, s := range []string{"m0", "m1", "m2", "m3", "m4", "m5", "m6", "m7", "m8", "m9"} {
		parts = append(parts, sessionTextMsg(t, archiveText(provider.RoleUser, s)))
	}
	parts = append(parts, archiveCompactionLine(t, archiveSummary("5"), archiveText(provider.RoleUser, "m9")))
	writeSessionText(t, path, parts...)
	h := readArchive(t, path, ArchivedOptions{Limit: 2})
	assertStrings(t, archiveTexts(h.Rows), []string{"m7", "m8"})
	if h.Older != 7 || h.Rows[1].MsgSeq != 8 || len(h.Marks) != 1 || h.Marks[0].Index != 2 {
		t.Fatalf("history = %+v", h)
	}
}
