package sessionindex

import (
	"reflect"
	"strings"
	"testing"
)

func collect(s string) []string {
	var out []string
	Tokenize(s, func(t string) { out = append(out, t) })
	return out
}

func TestTokenize(t *testing.T) {
	cases := map[string][]string{
		"Refactoring the CODE!":   {"refactoring", "the", "code"},
		"Problème à l'été, ÇA va": {"problème", "été", "ça", "va"},
		"a b c2 x_y":              {"c2"},
		strings.Repeat("é", 40):   {strings.Repeat("é", 32)},
	}
	for in, want := range cases {
		if got := collect(in); !reflect.DeepEqual(got, want) {
			t.Errorf("Tokenize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseQuery(t *testing.T) {
	toks, here := ParseQuery("@here Refacto refacto ÉTÉ")
	if !here || !reflect.DeepEqual(toks, []string{"refacto", "été"}) {
		t.Fatalf("got %q here=%v", toks, here)
	}
	toks, here = ParseQuery("mail@here.com")
	if here || !reflect.DeepEqual(toks, []string{"mail", "here", "com"}) {
		t.Fatalf("got %q here=%v", toks, here)
	}
}

func TestPrefixRange(t *testing.T) {
	terms := []string{"ref", "refacto", "refactoring", "refactors", "zz"}
	lo, hi, partial := PrefixRange(terms, collect("Refacto")[0], 0)
	if got := terms[lo:hi]; partial || !reflect.DeepEqual(got, []string{"refacto", "refactoring", "refactors"}) {
		t.Fatalf("got %q partial=%v", got, partial)
	}
	lo, hi, partial = PrefixRange(terms, "ref", 2)
	if hi-lo != 2 || !partial {
		t.Fatalf("cap: %d partial=%v", hi-lo, partial)
	}
	if lo, hi, _ = PrefixRange(terms, "zzz", 0); lo != hi {
		t.Fatal("expected empty range")
	}
}
