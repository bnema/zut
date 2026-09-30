package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/extensions"
	"github.com/bnema/zut/packages/agent/modes"
	"github.com/bnema/zut/packages/core"
)

func TestExtensionAdapterPreservesInteractivePolicy(t *testing.T) {
	manager := extensions.New(t.TempDir(), t.TempDir(), "test", "provider", "model", nonInteractiveExtHooks{})
	adapter := &extToolAdapter{mgr: manager}
	tool := adapter.NewExtensionTool(ExtensionToolInfo{
		Name: "ask_user", Interactive: true, Schema: json.RawMessage(`{"type":"object"}`),
	})
	if !extensions.IsInteractiveTool(tool) {
		t.Fatal("adapter lost interactive registration")
	}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil || !result.IsError {
		t.Fatalf("headless execution result=%+v err=%v", result, err)
	}
}

func TestInteractiveExtensionSupportRequiresAttachedHost(t *testing.T) {
	hooks := &interactiveExtHooks{}
	if hooks.SupportsInteractiveTools() {
		t.Fatal("unattached host permits interactive tools")
	}
	hooks.attachInteractive(&modes.Interactive{})
	if !hooks.SupportsInteractiveTools() {
		t.Fatal("attached TUI rejects interactive tools")
	}
}

func TestScheduledSessionRegistryExcludesInteractiveExtensionTools(t *testing.T) {
	interactive := extensions.NewTool(nil, extensions.ToolInfo{Name: "ask_user", Interactive: true, Schema: json.RawMessage(`{"type":"object"}`)})
	ordinary := extensions.NewTool(nil, extensions.ToolInfo{Name: "lookup", Schema: json.RawMessage(`{"type":"object"}`)})
	registry := scheduledSessionRegistry(core.Registry{"ask_user": interactive, "lookup": ordinary})
	if _, ok := registry["ask_user"]; ok {
		t.Fatal("scheduled background session retained an interactive tool")
	}
	if registry["lookup"] != ordinary {
		t.Fatal("scheduled background session lost an ordinary extension tool")
	}
}

func TestResidentRegistryRejectsInteractiveExtensionTools(t *testing.T) {
	tool := extensions.NewTool(nil, extensions.ToolInfo{Name: "ask_user", Interactive: true, Schema: json.RawMessage(`{"type":"object"}`)})
	if _, err := residentChildRegistry(core.Registry{"ask_user": tool}, []string{"ask_user"}); err == nil || !strings.Contains(err.Error(), "interactive") {
		t.Fatalf("resident registry accepted interactive tool: %v", err)
	}
}
