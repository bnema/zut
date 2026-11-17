package modes

import (
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestActivityServerTool(t *testing.T) {
	a := newAgentActivity("anthropic", "claude-sonnet-5-5")
	a.apply(core.EvTextDelta{Delta: "x"})
	a.apply(core.EvServerTool{ID: "srvtoolu_test1", Name: "advisor\x1b[31m", Phase: provider.ServerToolPhaseStart})
	if got := a.label(); got != "Consulting advisor[31m…" {
		t.Fatalf("start label = %q", got)
	}
	a.apply(core.EvServerTool{ID: "srvtoolu_test1", Name: "advisor", Phase: provider.ServerToolPhaseResult, Redacted: true})
	if a.kind != activityWaitingForResponse || a.tool != "" {
		t.Fatalf("after result = %+v", a.activity)
	}
}
