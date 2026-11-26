package sessionindex

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func queryRefs(t *testing.T, x *Index, text string) []HitRef {
	t.Helper()
	r, err := x.Query(context.Background(), Query{Text: text, Mode: ModeMessages})
	if err != nil {
		t.Fatal(err)
	}
	var refs []HitRef
	for _, s := range r.Sessions {
		refs = append(refs, s.Hits...)
	}
	return refs
}

func TestSnippets(t *testing.T) {
	root := t.TempDir()
	x := openTestIndex(t, root)
	long := strings.Repeat("é", maxSnippetRunes+10)
	a := newRootSession(t, root, "/proj/a",
		provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "snipword one"}, provider.TextBlock{Text: "two"}}},
		msg(provider.RoleAssistant, "snipword "+long))
	b := newRootSession(t, root, "/proj/b", msg(provider.RoleUser, "snipword in b"))
	mustUpdate(t, x)

	refs := queryRefs(t, x, "snipword")
	if len(refs) != 3 {
		t.Fatalf("refs = %d", len(refs))
	}
	snips, err := x.Snippets(context.Background(), refs)
	if err != nil {
		t.Fatal(err)
	}
	if len(snips) != 3 {
		t.Fatalf("snippets = %d", len(snips))
	}
	byText := map[string]bool{}
	for i, s := range snips {
		if s.Ref != refs[i] {
			t.Fatalf("order changed at %d", i)
		}
		byText[s.Text] = true
		if n := len([]rune(s.Text)); n > maxSnippetRunes {
			t.Fatalf("snippet runes = %d", n)
		}
	}
	if !byText["snipword one two"] || !byText["snipword in b"] {
		t.Fatalf("texts = %v", byText)
	}

	// Rewritten file (new identity): its refs are skipped.
	other := newRootSession(t, t.TempDir(), "/proj/b", msg(provider.RoleUser, "snipword in b"))
	raw, _ := os.ReadFile(other)
	if err := os.WriteFile(b, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// Truncated file: rows past the end are skipped.
	info, _ := os.Stat(a)
	if err := os.Truncate(a, info.Size()-10); err != nil {
		t.Fatal(err)
	}
	snips, err = x.Snippets(context.Background(), refs)
	if err != nil {
		t.Fatal(err)
	}
	if len(snips) != 1 || snips[0].Text != "snipword one two" {
		t.Fatalf("stale snippets = %+v", len(snips))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := x.Snippets(ctx, refs); err == nil {
		t.Fatal("expected cancellation")
	}
}

func TestSnippetsFingerprintMismatch(t *testing.T) {
	root := t.TempDir()
	x := openTestIndex(t, root)
	newRootSession(t, root, "/proj/a", msg(provider.RoleUser, "fpword"))
	mustUpdate(t, x)
	refs := queryRefs(t, x, "fpword")
	refs[0].Fingerprint++
	if snips, err := x.Snippets(context.Background(), refs); err != nil || len(snips) != 0 {
		t.Fatalf("mismatch = %d, %v", len(snips), err)
	}
}

func TestCapRunes(t *testing.T) {
	if got := capRunes("ééé", 2); got != "éé" {
		t.Fatalf("cap = %q", got)
	}
	if got := capRunes("ab", 5); got != "ab" {
		t.Fatalf("cap = %q", got)
	}
}
