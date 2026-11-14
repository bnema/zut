package tools

import (
	"context"
	"reflect"
	"testing"

	"github.com/bnema/zut/packages/core"
)

type codemodeOwnedTestTool struct {
	bridgeTestTool
	server string
}

func (t codemodeOwnedTestTool) Server() string { return t.server }

func TestCodemodeIdentifier(t *testing.T) {
	for input, want := range map[string]string{"list-things": "list_things", "a.b!": "a_b_", "9srv": "_9srv", "class": "class_", "async": "async_", "my_server": "my_server", "": "_"} {
		if got := codemodeIdentifier(input); got != want {
			t.Errorf("%q: %q != %q", input, got, want)
		}
	}
}

func TestCodemodeNamespaces(t *testing.T) {
	tools := core.NewRegistry(
		bridgeTestTool{"mcp__srv__list-things", ""},
		bridgeTestTool{"mcp__srv__class", ""},
		bridgeTestTool{"mcp__srv__a-b", ""}, bridgeTestTool{"mcp__srv__a_b", ""},
		bridgeTestTool{"mcp__a-b__one", ""}, bridgeTestTool{"mcp__a_b__two", ""},
		bridgeTestTool{"mcp__my_server__run", ""},
		bridgeTestTool{"mcp__9srv__9run", ""},
		bridgeTestTool{"mcp__ambiguous__server__tool", ""},
		codemodeOwnedTestTool{bridgeTestTool{"mcp__real__server__tool__name", ""}, "real__server"},
		bridgeTestTool{"read", ""}, bridgeTestTool{"codemode", ""},
	)
	want := map[string]map[string]string{
		"srv":          {"list_things": "mcp__srv__list-things", "class_": "mcp__srv__class"},
		"my_server":    {"run": "mcp__my_server__run"},
		"_9srv":        {"_9run": "mcp__9srv__9run"},
		"real__server": {"tool__name": "mcp__real__server__tool__name"},
	}
	b := newCodemodeBridge(&bridgeTestInvoker{tools: tools}, "")
	reply := b.handle(context.Background(), []byte(`{"method":"namespaces"}`))
	if reply.Error != nil || !reflect.DeepEqual(reply.Result, want) {
		t.Fatalf("%#v", reply)
	}
	// Collision exclusion does not remove canonical tools from the registry.
	if _, err := tools.Get("mcp__srv__a-b"); err != nil {
		t.Fatal(err)
	}
}
