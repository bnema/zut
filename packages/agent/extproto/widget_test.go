package extproto

import (
	"encoding/json"
	"testing"
)

func TestNormalizeWidgetPosition(t *testing.T) {
	cases := map[string]string{
		WidgetPositionRightBar: WidgetPositionAboveInput,
		"right_bar":            WidgetPositionAboveInput,
		"":                     WidgetPositionAboveInput,
		"unknown":              WidgetPositionAboveInput,
	}
	for input, want := range cases {
		if got := NormalizeWidgetPosition(input); got != want {
			t.Errorf("NormalizeWidgetPosition(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestWidgetFrameKeepsUnknownAndMissingPositionsCompatible(t *testing.T) {
	var missing WidgetFromExt
	if err := json.Unmarshal([]byte(`{"type":"widget","id":"plan","title":"Plan"}`), &missing); err != nil {
		t.Fatal(err)
	}
	if got := NormalizeWidgetPosition(missing.Position); got != WidgetPositionAboveInput {
		t.Fatalf("missing position normalized to %q, want %q", got, WidgetPositionAboveInput)
	}

	var removedPlacement WidgetFromExt
	if err := json.Unmarshal([]byte(`{"type":"widget","id":"plan","position":"right_bar","title":"Plan"}`), &removedPlacement); err != nil {
		t.Fatal(err)
	}
	if got := NormalizeWidgetPosition(removedPlacement.Position); got != WidgetPositionAboveInput {
		t.Fatalf("removed right_bar position normalized to %q, want %q", got, WidgetPositionAboveInput)
	}
}
