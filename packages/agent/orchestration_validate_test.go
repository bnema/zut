package agent

import (
	"strings"
	"testing"
)

func TestValidateOrchestrationArgsRequiresSpawnAction(t *testing.T) {
	base := Args{Orchestrate: true, Mode: ModePrint}
	denied := base
	denied.ToolsSet = true
	denied.Tools = []string{"read"}
	err := validateOrchestrationArgs(denied)
	if err == nil || !strings.Contains(err.Error(), "requires the subagent spawn action") {
		t.Fatalf("validateOrchestrationArgs error = %v", err)
	}
	allowed := base
	allowed.ToolsSet = true
	allowed.Tools = []string{"subagent:spawn"}
	if err := validateOrchestrationArgs(allowed); err != nil {
		t.Fatalf("spawn action must satisfy orchestration: %v", err)
	}
	resumeOnly := base
	resumeOnly.ToolsSet = true
	resumeOnly.Tools = []string{"subagent:resume"}
	if err := validateOrchestrationArgs(resumeOnly); err == nil || !strings.Contains(err.Error(), "requires the subagent spawn action") {
		t.Fatalf("resume-only error = %v", err)
	}
}
