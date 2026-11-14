package core

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestToolResultDataExcludedFromJSON(t *testing.T) {
	baseline, err := json.Marshal(ToolResult{})
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range []any{1, make(chan int)} {
		encoded, err := json.Marshal(ToolResult{Data: data})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(encoded, baseline) {
			t.Fatalf("Data changed serialized result: %s != %s", encoded, baseline)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		if _, ok := fields["Data"]; ok {
			t.Fatal("Data was persisted")
		}
	}
}
