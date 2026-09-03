package agent

import "testing"

func TestSubagentPolicyFromConfigRetainsSchedulingAndToolSettings(t *testing.T) {
	policy := subagentPolicyFromConfig(SubagentsConfig{
		MaxConcurrent: 3,
		QueueTimeout:  "2s",
		AllowedTools:  []string{"read"},
		AllowedRoots:  []string{"/workspace"},
	})
	if policy.MaxConcurrent != 3 || policy.QueueTimeout.String() != "2s" || len(policy.AllowedTools) != 1 || len(policy.AllowedRoots) != 1 {
		t.Fatalf("policy = %#v", policy)
	}
}
