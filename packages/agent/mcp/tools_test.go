package mcp

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestConvertCallResultStructuredData(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want any
	}{
		{"object", `{"items":[1,"two",true],"missing":null}`, json.RawMessage(`{"items":[1,"two",true],"missing":null}`)},
		{"array", `[1,2]`, json.RawMessage(`[1,2]`)},
		{"scalar", `"value"`, json.RawMessage(`"value"`)},
		{"null", `null`, json.RawMessage(`null`)},
		{"invalid", `{"broken":`, nil},
		{"empty", ``, nil},
		{"whitespace", `   `, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, withText := range []bool{true, false} {
				input := CallResult{StructuredContent: json.RawMessage(tc.raw)}
				wantText := tc.raw
				if withText {
					input.Content = []Content{{Type: "text", Text: "original text\n"}}
					wantText = "original text\n"
				} else if tc.raw == "" || tc.raw == "   " {
					wantText = "(no output)"
				}
				got := convertCallResult(input)
				if !reflect.DeepEqual(got.Data, tc.want) {
					t.Fatalf("Data = %#v, want %#v", got.Data, tc.want)
				}
				wantContent := []provider.Content{provider.TextBlock{Text: wantText}}
				if !reflect.DeepEqual(got.Content, wantContent) {
					t.Fatalf("Content = %#v, want %#v", got.Content, wantContent)
				}
			}
		})
	}
}

func TestDeferredCopies(t *testing.T) {
	readOnly := true
	for _, original := range []core.Tool{
		&serverTool{name: "mcp_test", raw: Tool{Annotations: &ToolAnnotations{ReadOnlyHint: &readOnly}}},
		&listResourcesTool{}, &readResourceTool{},
	} {
		deferred := Deferred(original)
		if deferred == original || original.(interface{ Deferred() bool }).Deferred() {
			t.Fatal("original modified")
		}
		reg := core.NewRegistry(deferred)
		if !reg.Specs()[0].Deferred {
			t.Fatal("spec not deferred")
		}
		got, err := reg.Get(original.Name())
		if err != nil || got != deferred || !IsTool(got) || !IsReadOnlyTool(got) {
			t.Fatalf("lookup/classification: %T %v", got, err)
		}
	}
	if Deferred(nil) != nil {
		t.Fatal("non-MCP input changed")
	}
}

func TestConvertCallResultStructuredIntegerRoundTrip(t *testing.T) {
	raw := json.RawMessage(`{"integer":9007199254740993}`)
	result := convertCallResult(CallResult{StructuredContent: raw})
	raw[0] = '[' // Data must not alias the response buffer.
	encoded, err := json.Marshal(result.Data)
	if err != nil || string(encoded) != `{"integer":9007199254740993}` {
		t.Fatalf("round trip = %s, %v", encoded, err)
	}
}
