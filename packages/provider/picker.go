package provider

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// PickerModel is one model as a model picker lists it.
type PickerModel struct {
	Model
	// SnapshotOf is the id of the alias this dated snapshot stands for
	// (claude-opus-4-5-20251101 -> claude-opus-4-5) when that alias is
	// listed for the same provider; empty otherwise. Pickers hide such
	// snapshots until the user searches.
	SnapshotOf string
}

// snapshotSuffix matches a trailing release date: -20251101 or -2024-08-06.
var snapshotSuffix = regexp.MustCompile(`-(\d{8}|\d{4}-\d{2}-\d{2})$`)

// PickerModels returns models in picker order: by provider, then newest
// version first, each dated snapshot right after its alias. The input is
// not modified.
func PickerModels(models []Model) []PickerModel {
	ids := make(map[string]bool, len(models))
	for _, m := range models {
		ids[m.Provider+"\x00"+m.ID] = true
	}
	out := make([]PickerModel, len(models))
	for i, m := range models {
		out[i] = PickerModel{Model: m, SnapshotOf: snapshotAlias(m, ids)}
	}
	base := func(p PickerModel) string {
		if p.SnapshotOf != "" {
			return p.SnapshotOf
		}
		return p.ID
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		ab, bb := base(a), base(b)
		if ab != bb {
			if c := compareVersions(modelVersion(ab), modelVersion(bb)); c != 0 {
				return c > 0
			}
			return ab < bb
		}
		// Same alias: the alias first, then its snapshots newest first.
		if (a.SnapshotOf == "") != (b.SnapshotOf == "") {
			return a.SnapshotOf == ""
		}
		return a.ID > b.ID
	})
	return out
}

// snapshotAlias returns the listed alias of a dated snapshot id: the id
// without its date, or that stem plus "-0" or "-latest"
// (claude-opus-4-20250514 -> claude-opus-4-0).
func snapshotAlias(m Model, ids map[string]bool) string {
	loc := snapshotSuffix.FindStringIndex(m.ID)
	if loc == nil {
		return ""
	}
	stem := m.ID[:loc[0]]
	for _, alias := range []string{stem, stem + "-0", stem + "-latest"} {
		if ids[m.Provider+"\x00"+alias] {
			return alias
		}
	}
	return ""
}

// modelVersion extracts the version numbers of a model id: the digits of
// the first token that has any, followed by the next all-digit tokens.
// "claude-opus-4-5" -> [4 5], "gpt-5.2-codex" -> [5 2], "qwen3-coder-480b"
// -> [3], "gpt-4o" -> [4]. A trailing release date is ignored; "-0"
// aliases compare equal to their major version. Ids without digits return
// nil.
func modelVersion(id string) []int {
	id = snapshotSuffix.ReplaceAllString(id, "")
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		id = id[i+1:] // openrouter-style "vendor/model"
	}
	tokens := strings.FieldsFunc(id, func(r rune) bool { return r == '-' || r == '.' || r == '_' || r == ':' })
	var out []int
	for _, tok := range tokens {
		if len(out) == 0 {
			if n, ok := tokenDigits(tok); ok {
				out = append(out, n)
			}
			continue
		}
		n, err := strconv.Atoi(tok)
		if err != nil || len(tok) > 4 {
			break
		}
		out = append(out, n)
	}
	return out
}

// tokenDigits returns the digit run of a token: its leading digits
// ("4o" -> 4) or, after a letter prefix, its trailing digits ("qwen3" -> 3).
func tokenDigits(tok string) (int, bool) {
	start := strings.IndexFunc(tok, unicode.IsDigit)
	if start < 0 {
		return 0, false
	}
	end := start
	for end < len(tok) && unicode.IsDigit(rune(tok[end])) {
		end++
	}
	if end-start > 4 {
		return 0, false // a size or date, not a version
	}
	n, err := strconv.Atoi(tok[start:end])
	return n, err == nil
}

// compareVersions compares version number lists; missing parts count as 0
// and nil (no version) sorts below any version.
func compareVersions(a, b []int) int {
	if (a == nil) != (b == nil) {
		if a == nil {
			return -1
		}
		return 1
	}
	for i := 0; i < max(len(a), len(b)); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}
