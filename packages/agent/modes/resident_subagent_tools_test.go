package modes

import (
	"context"
	"testing"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

// TestApplyAutoSubagentsToolReplacesOneFacadeEntry covers the interactive
// refresh path: the previously installed facade is replaced by exactly one
// entry whose non-nil fields are the currently granted actions, and unrelated
// tools survive.
func TestApplyAutoSubagentsToolReplacesOneFacadeEntry(t *testing.T) {
	manager := subagents.NewResidentManager(t.TempDir(), nil)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	preSeeded := &tools.SubagentTool{}
	agent := core.NewAgent(nil, "model", "", core.Registry{
		"read": &tools.GrepTool{},
		// A previously installed facade, as a prior refresh would have left it.
		"subagent": preSeeded,
	})
	allowed, resumeAllowed := true, true
	interactive := NewInteractive(InteractiveConfig{
		Agent:                          agent,
		ResidentManager:                manager,
		AutoSubagentsToolAllowed:       &allowed,
		AutoSubagentsStatusToolAllowed: &allowed,
		AutoSubagentsStopToolAllowed:   &allowed,
		AutoSubagentsResumeToolAllowed: &resumeAllowed,
		// Interrupt is left unset, so it follows the spawn grant.
	})

	interactive.applyAutoSubagentsTool()

	assertFacade := func(wantResume bool) {
		t.Helper()
		snapshot := agent.ToolsSnapshot()
		if _, ok := snapshot["read"]; !ok {
			t.Fatal("interactive refresh dropped an unrelated tool")
		}
		found := 0
		for name := range snapshot {
			switch name {
			case tools.SubagentToolName:
				found++
			case "subagent_spawn", "subagent_status", "subagent_stop", "subagent_resume":
				t.Fatalf("legacy tool key %q is present", name)
			}
		}
		if found != 1 {
			t.Fatalf("registry has %d %q entries, want 1", found, tools.SubagentToolName)
		}
		facade, ok := snapshot[tools.SubagentToolName].(*tools.SubagentTool)
		if !ok {
			t.Fatalf("registry entry %q = %T, want *tools.SubagentTool", tools.SubagentToolName, snapshot[tools.SubagentToolName])
		}
		if facade == preSeeded {
			t.Fatal("interactive refresh reused the pre-seeded facade instead of replacing it")
		}
		if facade.Spawn == nil || facade.Status == nil || facade.Stop == nil || facade.Interrupt == nil {
			t.Fatalf("facade actions = spawn:%v status:%v stop:%v interrupt:%v", facade.Spawn != nil, facade.Status != nil, facade.Stop != nil, facade.Interrupt != nil)
		}
		if (facade.Resume != nil) != wantResume {
			t.Fatalf("facade resume = %v, want %v", facade.Resume != nil, wantResume)
		}
	}

	assertFacade(true)

	// A later policy change must replace the installed facade, not merge or
	// duplicate it.
	resumeAllowed = false
	interactive.applyAutoSubagentsTool()
	assertFacade(false)
}
