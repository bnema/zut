package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bnema/zut/packages/core"
)

func TestCodemodeSignature(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","required":["pattern"],"properties":{"pattern":{"type":"string","description":"Search pattern\nnext line"},"count":{"type":"integer"},"ratio":{"type":"number"},"enabled":{"type":"boolean"},"options":{"type":"object"},"paths":{"type":"array","items":{"type":"string"}},"mode":{"enum":["fast","slow"]},"glob":{"type":["string","null"]}}}`)
	got := codemodeSignature("grep", raw)
	for _, want := range []string{"def grep(*, ", "pattern: str", "count: int = ...", "ratio: float = ...", "enabled: bool = ...", "options: dict = ...", "paths: list[str] = ...", `mode: Literal["fast", "slow"] = ...`, "glob: str | None = ...", ") -> ToolResult:", "pattern: Search pattern next line"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, "pattern: str =") {
		t.Fatal(got)
	}
	if got != codemodeSignature("grep", raw) {
		t.Fatal("unstable signature")
	}
}

func TestCodemodeSignatureBoundsAndFallback(t *testing.T) {
	props := map[string]any{}
	for i := 0; i < 1000; i++ {
		props[fmt.Sprintf("arg%d", i)] = map[string]any{"type": "string", "description": strings.Repeat("é", 1000)}
	}
	props["bad-name"] = map[string]any{"type": "string"}
	raw, _ := json.Marshal(map[string]any{"properties": props})
	got := codemodeSignature(strings.Repeat("n", 5000), raw)
	if len(got) > 4096 || !utf8.ValidString(got) || !strings.Contains(got, "omitted") || !strings.HasSuffix(got, "    ...") {
		t.Fatal(len(got), got)
	}
	for _, raw := range []string{`{}`, `null`, `not json`} {
		if got := codemodeSignature("empty", json.RawMessage(raw)); got != "def empty() -> ToolResult:\n    ..." {
			t.Fatal(got)
		}
	}
	for raw, want := range map[string]string{`{"type":"array"}`: "list[Any]", `{"enum":[true,false,null,3]}`: "Literal[True, False, None, 3]", `{"$ref":"x"}`: "Any"} {
		var schema map[string]any
		_ = json.Unmarshal([]byte(raw), &schema)
		if got := codemodeSchemaType(schema, 0); got != want {
			t.Fatal(got, want)
		}
	}
}

func TestCodemodeDescribeSignature(t *testing.T) {
	b := newCodemodeBridge(&bridgeTestInvoker{tools: core.NewRegistry(bridgeTestTool{"echo", "description"})}, "")
	reply := b.handle(context.Background(), []byte(`{"method":"describe","params":{"name":"echo"}}`))
	if reply.Error != nil {
		t.Fatal(reply.Error)
	}
	result := reply.Result.(map[string]any)
	if result["signature"] != "def echo() -> ToolResult:\n    ..." || result["schema"] == nil {
		t.Fatal(result)
	}
}
