package sessionindex

import (
	"sort"
	"strings"
	"unicode"
)

const (
	minTokenRunes = 2
	maxTokenRunes = 32
	hereToken     = "@here"
)

// Tokenize calls emit for each index token of s: lowercase runs of letters
// and digits, at least minTokenRunes long, truncated to maxTokenRunes.
func Tokenize(s string, emit func(string)) {
	var b strings.Builder
	n := 0
	flush := func() {
		if n >= minTokenRunes {
			emit(b.String())
		}
		b.Reset()
		n = 0
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if n < maxTokenRunes {
				b.WriteRune(unicode.ToLower(r))
			}
			n++
		} else if n > 0 {
			flush()
		}
	}
	if n > 0 {
		flush()
	}
}

// ParseQuery splits text into prefix tokens. A standalone "@here" word sets
// here and is removed. Duplicate tokens are dropped.
func ParseQuery(text string) (tokens []string, here bool) {
	seen := map[string]bool{}
	for _, field := range strings.Fields(text) {
		if strings.EqualFold(field, hereToken) {
			here = true
			continue
		}
		Tokenize(field, func(t string) {
			if !seen[t] {
				seen[t] = true
				tokens = append(tokens, t)
			}
		})
	}
	return tokens, here
}

// PrefixRange returns the index range [lo, hi) of terms (sorted ascending)
// that start with prefix. At most limit terms are returned when limit > 0;
// partial reports that more matched.
func PrefixRange(terms []string, prefix string, limit int) (lo, hi int, partial bool) {
	lo = sort.SearchStrings(terms, prefix)
	hi = lo
	for hi < len(terms) && strings.HasPrefix(terms[hi], prefix) {
		if limit > 0 && hi-lo == limit {
			return lo, hi, true
		}
		hi++
	}
	return lo, hi, false
}
