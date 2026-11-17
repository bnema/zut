package daemonproto_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/agent/modes"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestServerToolEventsRoundTrip(t *testing.T) {
	msg := provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{
		provider.ServerToolBlock{Provider: "anthropic", Kind: provider.ServerToolKindUse, ID: "srvtoolu_test1", Name: "advisor",
			Raw: json.RawMessage(`{"type":"server_tool_use","id":"srvtoolu_test1","name":"advisor","input":{}}`)},
		provider.ServerToolBlock{Provider: "anthropic", Kind: provider.ServerToolKindAdvisorResult, ID: "srvtoolu_test1", Name: "advisor", Redacted: true,
			Raw: json.RawMessage(`{"type":"advisor_tool_result","tool_use_id":"srvtoolu_test1","content":{"type":"advisor_redacted_result","encrypted_content":"ZW5jcnlwdGVk"}}`)},
		provider.TextBlock{Text: "done"},
	}}
	for _, ev := range []core.AgentEvent{
		core.EvAssistantMessage{Message: msg},
		core.EvServerTool{ID: "srvtoolu_test1", Name: "advisor", Phase: provider.ServerToolPhaseStart},
		core.EvServerTool{ID: "srvtoolu_test1", Name: "advisor", Phase: provider.ServerToolPhaseResult, Text: "advice", ErrorCode: "x", Redacted: true},
	} {
		t.Run(ev.Type(), func(t *testing.T) {
			kind, raw, err := daemonproto.EncodeAgentEvent(ev)
			if err != nil {
				t.Fatal(err)
			}
			got, err := daemonproto.DecodeAgentEvent(kind, raw)
			if err != nil {
				t.Fatal(err)
			}
			a, _ := json.Marshal(ev)
			b, _ := json.Marshal(got)
			if string(a) != string(b) {
				t.Fatalf("round trip\n%s\n%s", a, b)
			}
			if am, ok := got.(core.EvAssistantMessage); ok {
				if _, ok := am.Message.Content[0].(provider.ServerToolBlock); !ok {
					t.Fatalf("decoded block = %T", am.Message.Content[0])
				}
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			projection := modes.EventToJSON(ev)
			for k, v := range projection {
				expected, _ := json.Marshal(v)
				var x, y any
				_ = json.Unmarshal(expected, &x)
				_ = json.Unmarshal(wire[k], &y)
				if !reflect.DeepEqual(x, y) {
					t.Fatalf("projection field %s: %s != %s", k, wire[k], expected)
				}
			}
			if p, _ := json.Marshal(projection); strings.Contains(string(p), "ZW5jcnlwdGVk") {
				t.Fatalf("projection leaked raw block: %s", p)
			}
		})
	}
}
