package core

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
)

type keepaliveStreamClient struct{}

func (keepaliveStreamClient) Name() string { return "keepalive-stream" }

func (keepaliveStreamClient) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	out := make(chan provider.Event, 4)
	out <- provider.EventKeepalive{}
	out <- provider.EventKeepalive{}
	out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
		Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "ok"}},
	}}
	close(out)
	return out, nil
}

func TestAgentKeepaliveResetsIdleOnly(t *testing.T) {
	a := NewAgent(keepaliveStreamClient{}, "fake-model", "system", Registry{})
	a.MaxRetries = 0
	a.StreamIdleTimeout = time.Minute
	var resets atomic.Int32
	a.streamIdleTimer = func(time.Duration) (<-chan time.Time, func(), func()) {
		return make(chan time.Time), func() {}, func() { resets.Add(1) }
	}
	var types []string
	if err := a.Prompt(context.Background(), "hello", nil, func(ev AgentEvent) { types = append(types, ev.Type()) }); err != nil {
		t.Fatal(err)
	}
	if got := resets.Load(); got != 4 {
		t.Fatalf("idle resets = %d, want open + 3 events", got)
	}
	for _, ty := range types {
		if ty == "server_tool" || ty == "text_delta" {
			t.Fatalf("keepalive produced event %q", ty)
		}
	}
}

func TestDropDanglingServerToolUses(t *testing.T) {
	use := func(id string) provider.ServerToolBlock {
		return provider.ServerToolBlock{Provider: "anthropic", Kind: provider.ServerToolKindUse, ID: id, Raw: json.RawMessage(`{}`)}
	}
	result := provider.ServerToolBlock{Provider: "anthropic", Kind: provider.ServerToolKindAdvisorResult, ID: "done", Raw: json.RawMessage(`{}`)}
	in := []provider.Content{provider.TextBlock{Text: "a"}, use("done"), result, use("pending")}
	out := dropDanglingServerToolUses(in)
	if len(out) != 3 {
		t.Fatalf("out = %+v", out)
	}
	for _, c := range out {
		if b, ok := c.(provider.ServerToolBlock); ok && b.ID == "pending" {
			t.Fatal("dangling use kept")
		}
	}
	clean := []provider.Content{provider.TextBlock{Text: "a"}}
	if got := dropDanglingServerToolUses(clean); &got[0] != &clean[0] {
		t.Fatal("clean content copied")
	}
}

func TestAgentHasTool(t *testing.T) {
	a := NewAgent(keepaliveStreamClient{}, "fake-model", "system", Registry{"result": &resultTool{}})
	if !a.HasTool("result") || a.HasTool("advisor") {
		t.Fatal("HasTool mismatch")
	}
	a.SetTools(Registry{})
	if a.HasTool("result") {
		t.Fatal("HasTool sees replaced registry")
	}
}
