package provider

import (
	"reflect"
	"testing"
)

func TestModelVersion(t *testing.T) {
	tests := map[string][]int{
		"claude-opus-4-5":            {4, 5},
		"claude-opus-4-5-20251101":   {4, 5},
		"claude-opus-4-0":            {4, 0},
		"claude-3-5-sonnet-20241022": {3, 5},
		"gpt-5.2-codex":              {5, 2},
		"gpt-4o-2024-08-06":          {4},
		"qwen3-coder-480b-a35b":      {3},
		"llama-3.1-405b":             {3, 1},
		"anthropic/claude-opus-4.5":  {4, 5},
		"kimi-for-coding":            nil,
		"gpt-oss-120b":               nil,
		"open-mixtral-8x22b":         nil,
		"magistral-small-2509":       nil,
		"kimi-k2-0905-preview":       {2},
		"grok-4.20-0309-reasoning":   {4, 20},
		"kimi-k2p6":                  {2, 6},
	}
	for id, want := range tests {
		if got := modelVersion(id); !reflect.DeepEqual(got, want) {
			t.Errorf("modelVersion(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestPickerModelsOrderAndSnapshots(t *testing.T) {
	in := []Model{
		{Provider: "openai", ID: "gpt-4o-2024-08-06"},
		{Provider: "anthropic", ID: "claude-opus-4-20250514"},
		{Provider: "anthropic", ID: "claude-opus-4-5-20251101"},
		{Provider: "anthropic", ID: "claude-sonnet-5"},
		{Provider: "anthropic", ID: "kimi-for-coding"},
		{Provider: "anthropic", ID: "claude-opus-4-5"},
		{Provider: "anthropic", ID: "claude-opus-4-0"},
		{Provider: "anthropic", ID: "claude-opus-5-5"},
		{Provider: "anthropic", ID: "claude-opus-5"},
		{Provider: "openai", ID: "gpt-5.2"},
		{Provider: "openai", ID: "gpt-4o"},
	}
	got := PickerModels(in)
	type row struct{ id, of string }
	var rows []row
	for _, m := range got {
		rows = append(rows, row{m.ID, m.SnapshotOf})
	}
	want := []row{
		{"claude-opus-5-5", ""},
		{"claude-sonnet-5", ""},
		{"claude-opus-5", ""},
		{"claude-opus-4-5", ""},
		{"claude-opus-4-5-20251101", "claude-opus-4-5"},
		{"claude-opus-4-0", ""},
		{"claude-opus-4-20250514", "claude-opus-4-0"},
		{"kimi-for-coding", ""},
		{"gpt-5.2", ""},
		{"gpt-4o", ""},
		{"gpt-4o-2024-08-06", "gpt-4o"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("PickerModels order =\n%v\nwant\n%v", rows, want)
	}
	if in[0].ID != "gpt-4o-2024-08-06" {
		t.Fatal("PickerModels modified its input")
	}
}

func TestPickerModelsNewestDateFirstWithoutAlias(t *testing.T) {
	got := PickerModels([]Model{
		{Provider: "anthropic", ID: "claude-3-5-sonnet-20240620"},
		{Provider: "anthropic", ID: "claude-3-5-sonnet-20241022"},
		{Provider: "groq", ID: "gpt-oss-120b"},
		{Provider: "groq", ID: "llama-3.3-70b"},
	})
	ids := []string{got[0].ID, got[1].ID, got[2].ID, got[3].ID}
	want := []string{"claude-3-5-sonnet-20241022", "claude-3-5-sonnet-20240620", "llama-3.3-70b", "gpt-oss-120b"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("order = %v, want %v", ids, want)
	}
}

func TestPickerModelsKeepsSnapshotWithoutAlias(t *testing.T) {
	got := PickerModels([]Model{
		{Provider: "anthropic", ID: "claude-3-opus-20240229"},
		{Provider: "openai", ID: "claude-3-opus"}, // other provider: not an alias
	})
	for _, m := range got {
		if m.SnapshotOf != "" {
			t.Fatalf("%s/%s marked as snapshot of %q", m.Provider, m.ID, m.SnapshotOf)
		}
	}
}

func TestModelLookupMatchesFindModel(t *testing.T) {
	lookup := ModelLookup()
	want, err := FindModel("anthropic", "claude-opus-4-5")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := lookup("anthropic", "claude-opus-4-5")
	if !ok || got.ID != want.ID || got.DisplayName != want.DisplayName {
		t.Fatalf("lookup = %+v, %v; want %+v", got, ok, want)
	}
	if _, ok := lookup("anthropic", "no-such-model"); ok {
		t.Fatal("lookup found an unknown model")
	}
}
