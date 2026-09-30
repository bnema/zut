package core

import (
	"reflect"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func TestRepairToolUseResultPairsPreservesRepeatedIDsAcrossTurns(t *testing.T) {
	messages := []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "reused", Name: "read"}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: "reused", Content: []provider.Content{provider.TextBlock{Text: "first"}}}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "reused", Name: "read"}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: "reused", Content: []provider.Content{provider.TextBlock{Text: "second"}}}}},
	}
	repaired := repairToolUseResultPairs(messages)
	if !reflect.DeepEqual(repaired, messages) {
		t.Fatal("session repair changed valid repeated-ID pairs")
	}
	if wire := provider.RepairOrphanedToolResults(repaired); !reflect.DeepEqual(wire, messages) {
		t.Fatal("provider repair dropped a valid pair after session repair")
	}
}
