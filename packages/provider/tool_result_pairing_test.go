package provider

import (
	"reflect"
	"testing"
)

func pairingResult(id, text string) Message {
	return Message{Role: RoleTool, Content: []Content{ToolResultBlock{CallID: id, Content: []Content{TextBlock{Text: text}}}}}
}

func pairingCall(id string) Message {
	return Message{Role: RoleAssistant, Content: []Content{ToolCallBlock{ID: id, Name: "read"}}}
}

func TestRepairOrphanedToolResultsPreservesRepeatedToolCallPairs(t *testing.T) {
	msgs := []Message{pairingCall("reused"), pairingResult("reused", "first"), pairingCall("reused"), pairingResult("reused", "second")}
	got := RepairOrphanedToolResults(msgs)
	if !reflect.DeepEqual(got, msgs) {
		t.Fatalf("got %#v, want both repeated-ID pairs preserved", got)
	}
}

func TestAnthropicBuildRequestPreservesRepeatedToolCallPairs(t *testing.T) {
	c := NewAnthropic("test-key", "").(*anthropicClient)
	wire, err := c.buildRequest(Request{
		Model: "claude-sonnet-4-5",
		Messages: []Message{
			pairingCall("reused"), pairingResult("reused", "first"),
			pairingCall("reused"), pairingResult("reused", "second"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(wire.Messages) != 4 {
		t.Fatalf("messages=%d, want both repeated-ID tool result pairs", len(wire.Messages))
	}
	for _, index := range []int{1, 3} {
		if len(wire.Messages[index].Content) != 1 {
			t.Fatalf("message %d content=%d, want 1 tool result", index, len(wire.Messages[index].Content))
		}
		if _, ok := wire.Messages[index].Content[0].(anthToolResultBlock); !ok {
			t.Fatalf("message %d content type=%T, want anthToolResultBlock", index, wire.Messages[index].Content[0])
		}
	}
}

func TestRepairOrphanedToolResultsDropsLeadingOrphanBeforeReusedID(t *testing.T) {
	user := Message{Role: RoleUser, Content: []Content{TextBlock{Text: "next turn"}}}
	messages := []Message{pairingResult("reused", "orphan"), user, pairingCall("reused"), pairingResult("reused", "valid")}
	want := messages[1:]
	if got := RepairOrphanedToolResults(messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want leading orphan removed and later valid pair retained", got)
	}
}

func TestRepairOrphanedToolResultsDropsExcessAndOrphans(t *testing.T) {
	msgs := []Message{
		pairingCall("a"), pairingResult("a", "one"), pairingResult("a", "dup"), pairingResult("orphan", "x"),
	}
	got := RepairOrphanedToolResults(msgs)
	if len(got) != 2 {
		t.Fatalf("messages=%d, want 2", len(got))
	}
	if tr := got[1].Content[0].(ToolResultBlock); tr.Content[0].(TextBlock).Text != "one" {
		t.Fatalf("kept wrong result: %#v", tr)
	}
}

func TestRepairOrphanedToolResultsKeepsMixedTextAndDoesNotMutate(t *testing.T) {
	msgs := []Message{
		pairingCall("a"),
		{Role: RoleTool, Content: []Content{
			TextBlock{Text: "note"},
			ToolResultBlock{CallID: "a"},
			ToolResultBlock{CallID: "a"},
			ToolResultBlock{CallID: "orphan"},
		}},
	}
	orig := []Message{msgs[0], {Role: RoleTool, Content: append([]Content(nil), msgs[1].Content...)}}
	got := RepairOrphanedToolResults(msgs)
	want := []Content{TextBlock{Text: "note"}, ToolResultBlock{CallID: "a"}}
	if len(got) != 2 || !reflect.DeepEqual(got[1].Content, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(msgs, orig) {
		t.Fatalf("input mutated: %#v", msgs)
	}
}
