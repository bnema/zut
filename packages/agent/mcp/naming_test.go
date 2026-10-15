package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSanitizeAndQualifiedName(t *testing.T) {
	cases := []struct{ server, tool, want string }{
		{"fake", "echo", "mcp__fake__echo"},
		{"my server", "do.it!", "mcp__my_server__do_it_"},
		{"a.b", "c", "mcp__a_b__c"},
		{"ok-name_1", "Tool-2", "mcp__ok-name_1__Tool-2"},
		{"", "", "mcp__" + "_" + "__" + "_"},
		{"é", "x", "mcp__" + "_" + "__x"},
	}
	for _, c := range cases {
		got := QualifiedName(c.server, c.tool)
		if got != c.want {
			t.Errorf("QualifiedName(%q,%q) = %q, want %q", c.server, c.tool, got, c.want)
		}
	}
	if got := sanitizeName(""); got != "_" {
		t.Errorf("sanitizeName(\"\") = %q", got)
	}
}

func TestQualifiedNameLengthBound(t *testing.T) {
	server := strings.Repeat("s", 40)
	toolA := strings.Repeat("t", 50) + "A"
	toolB := strings.Repeat("t", 50) + "B"
	a := QualifiedName(server, toolA)
	b := QualifiedName(server, toolB)
	for _, n := range []string{a, b} {
		if len(n) != maxToolNameLength {
			t.Errorf("name %q has length %d, want %d", n, len(n), maxToolNameLength)
		}
		if !strings.HasPrefix(n, ToolPrefix) {
			t.Errorf("name %q lost its prefix", n)
		}
	}
	if a == b {
		t.Fatalf("overflowing names with different tools collided: %q", a)
	}
	if a != QualifiedName(server, toolA) {
		t.Fatal("QualifiedName is not stable")
	}
	// A name that fits exactly is left alone.
	exact := QualifiedName("s", strings.Repeat("x", maxToolNameLength-len("mcp__s__")))
	if want := "mcp__s__" + strings.Repeat("x", maxToolNameLength-len("mcp__s__")); exact != want {
		t.Errorf("exact-fit name unexpectedly altered: %q", exact)
	}
}

func TestAssignNamesCollisions(t *testing.T) {
	pairs := [][2]string{{"a.b", "t"}, {"a_b", "t"}, {"solo", "t"}}
	names := assignNames(pairs, nil)
	if len(names) != 3 {
		t.Fatalf("got %d names", len(names))
	}
	seen := map[string]bool{}
	for p, n := range names {
		if seen[n] {
			t.Errorf("duplicate name %q", n)
		}
		seen[n] = true
		if len(n) > maxToolNameLength {
			t.Errorf("name %q too long for %v", n, p)
		}
	}
	if names[[2]string{"solo", "t"}] != "mcp__solo__t" {
		t.Errorf("non-colliding name changed: %q", names[[2]string{"solo", "t"}])
	}
	for _, p := range pairs[:2] {
		n := names[p]
		if n == "mcp__a_b__t" || !strings.HasPrefix(n, "mcp__a_b__t_") {
			t.Errorf("colliding name %q should carry a hash suffix", n)
		}
	}
	// Stable across calls and independent of input order.
	again := assignNames([][2]string{pairs[2], pairs[1], pairs[0]}, nil)
	for _, p := range pairs {
		if again[p] != names[p] {
			t.Errorf("name for %v not stable: %q vs %q", p, again[p], names[p])
		}
	}
}

func TestAssignNamesReserved(t *testing.T) {
	reserved := func(n string) bool { return n == "mcp__s__t" }
	names := assignNames([][2]string{{"s", "t"}}, reserved)
	got := names[[2]string{"s", "t"}]
	if got == "mcp__s__t" || reserved(got) || len(got) > maxToolNameLength {
		t.Fatalf("reserved name not avoided: %q", got)
	}
	// Reserving the first hashed candidate forces a further distinct one.
	first := got
	reserved2 := func(n string) bool { return n == "mcp__s__t" || n == first }
	got2 := assignNames([][2]string{{"s", "t"}}, reserved2)[[2]string{"s", "t"}]
	if got2 == first || got2 == "mcp__s__t" {
		t.Fatalf("second reservation not avoided: %q", got2)
	}
}

func TestAssignNamesLongCollisionsStayUnique(t *testing.T) {
	long := strings.Repeat("x", 80)
	pairs := [][2]string{{long, "a"}, {long, "a"}, {long, "b"}}
	names := assignNames(pairs, nil)
	// Duplicate input pairs map to the same key; the distinct pairs differ.
	if names[[2]string{long, "a"}] == names[[2]string{long, "b"}] {
		t.Fatal("distinct tools share a name")
	}
}

func TestNormalizeSchema(t *testing.T) {
	empty := `{"type":"object","properties":{}}`
	for name, in := range map[string]string{
		"nil":     "",
		"blank":   "  \n",
		"invalid": "{not json",
		"null":    "null",
		"array":   "[1,2]",
	} {
		got := string(NormalizeSchema(json.RawMessage(in)))
		if got != empty {
			t.Errorf("%s: got %s, want %s", name, got, empty)
		}
	}

	in := `{
		"$schema":"http://json-schema.org/draft-07/schema#","$id":"x",
		"type":"string",
		"anyOf":[{"required":["a"]}],"oneOf":[{}],"allOf":[{}],"not":{},
		"required":[],
		"properties":{"a":{"type":"integer","description":"d"}}
	}`
	var got map[string]any
	if err := json.Unmarshal(NormalizeSchema(json.RawMessage(in)), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"anyOf", "oneOf", "allOf", "not", "$schema", "$id", "required"} {
		if _, ok := got[k]; ok {
			t.Errorf("key %q should have been removed", k)
		}
	}
	if got["type"] != "object" {
		t.Errorf("type = %v, want object", got["type"])
	}
	props, _ := got["properties"].(map[string]any)
	a, _ := props["a"].(map[string]any)
	if a["type"] != "integer" || a["description"] != "d" {
		t.Errorf("properties not preserved: %v", got["properties"])
	}

	// Missing properties are added; non-empty required is kept.
	var got2 map[string]any
	_ = json.Unmarshal(NormalizeSchema(json.RawMessage(`{"required":["x"]}`)), &got2)
	if p, ok := got2["properties"].(map[string]any); !ok || p == nil {
		t.Errorf("properties not defaulted: %v", got2)
	}
	if req, _ := got2["required"].([]any); len(req) != 1 {
		t.Errorf("required dropped: %v", got2)
	}
}
