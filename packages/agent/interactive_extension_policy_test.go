package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/extensions"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

// The daemon's late-bound hooks report no interactive-tool support until a
// client with that capability is bound.
func TestSessionHooksInteractiveSupportRequiresBoundClient(t *testing.T) {
	if newSessionBindings().hooks().SupportsInteractiveTools() {
		t.Fatal("unbound session hooks permit interactive tools")
	}
}

func TestExtensionAdapterPreservesInteractivePolicy(t *testing.T) {
	manager := extensions.New(t.TempDir(), t.TempDir(), "test", "provider", "model", newSessionBindings().hooks())
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

func TestResidentRegistryRejectsInteractiveExtensionTools(t *testing.T) {
	tool := extensions.NewTool(nil, extensions.ToolInfo{Name: "ask_user", Interactive: true, Schema: json.RawMessage(`{"type":"object"}`)})
	if _, err := residentChildRegistry(core.Registry{"ask_user": tool}, []string{"ask_user"}); err == nil || !strings.Contains(err.Error(), "interactive") {
		t.Fatalf("resident registry accepted interactive tool: %v", err)
	}
}

func TestNativeQuestionsExcludedFromBackgroundRegistries(t *testing.T) {
	tool := &tools.AskUserQuestionTool{}
	if !extensions.IsInteractiveTool(tool) {
		t.Fatal("native question tool is not interactive")
	}
	reg := core.Registry{tool.Name(): tool}
	if _, err := residentChildRegistry(reg, []string{tool.Name()}); err == nil || !strings.Contains(err.Error(), "interactive") {
		t.Fatalf("resident registry: %v", err)
	}
}
