package core

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

// planCompactionAgent returns an agent whose retained internal-context message
// is "clock", so a test can tell exactly what the plan block was merged into.
func planCompactionAgent(t *testing.T, steps []PlanStep) *Agent {
	t.Helper()
	agent := NewAgent(&compactLifecycleClient{}, "compact-model", "system", Registry{})
	agent.SetMessages([]provider.Message{
		{Role: provider.RoleDeveloper, Content: []provider.Content{provider.TextBlock{Text: "clock"}}, Meta: map[string]string{internalContextMarker: "true"}},
		textMessage(provider.RoleUser, "do the thing"),
		textMessage(provider.RoleAssistant, "working"),
	})
	agent.SetPlan(steps)
	return agent
}

func compactOnce(t *testing.T, agent *Agent) {
	t.Helper()
	if _, err := agent.Compact(context.Background(), 0, nil); err != nil {
		t.Fatalf("Compact: %v", err)
	}
}

func TestCompactCarriesPlanIntoRetainedInternalContext(t *testing.T) {
	agent := planCompactionAgent(t, []PlanStep{
		{Step: "first", Status: PlanCompleted},
		{Step: "second", Status: PlanInProgress},
		{Step: "third", Status: PlanPending},
	})
	compactOnce(t, agent)

	// Assert on the post-compaction message list, not only the summarize call:
	// the block has to survive the rebuild that replaces the transcript.
	messages := agent.Messages()
	if len(messages) != 2 {
		t.Fatalf("compacted messages = %d, want retained internal context then summary", len(messages))
	}
	if messages[0].Role != provider.RoleDeveloper || !isInternalContextMessage(messages[0]) {
		t.Fatalf("first post-compaction message = %#v, want retained internal context", messages[0])
	}
	want := "clock\n\n## Current plan\n1. [x] first\n2. [~] second\n3. [ ] third"
	if got := internalContextText(messages[0]); got != want {
		t.Fatalf("post-compaction internal context =\n%q\nwant\n%q", got, want)
	}
}

func TestCompactWithoutPlanLeavesInternalContextByteIdentical(t *testing.T) {
	agent := planCompactionAgent(t, nil)
	before := internalContextText(agent.Messages()[0])
	compactOnce(t, agent)

	messages := agent.Messages()
	got := internalContextText(messages[0])
	if got != before {
		t.Fatalf("internal context changed without a plan: got %q, want %q", got, before)
	}
	if strings.Contains(got, planContextHeader) {
		t.Fatalf("internal context gained a plan block without a plan: %q", got)
	}
}

func TestCompactPlanBlockTruncatesAfterTwelveSteps(t *testing.T) {
	steps := make([]PlanStep, 15)
	for i := range steps {
		steps[i] = PlanStep{Step: fmt.Sprintf("step %d", i+1), Status: PlanPending}
	}
	agent := planCompactionAgent(t, steps)
	compactOnce(t, agent)

	got := internalContextText(agent.Messages()[0])
	if !strings.Contains(got, "12. [ ] step 12") {
		t.Fatalf("block did not include the twelfth step:\n%s", got)
	}
	if !strings.Contains(got, `…and 3 more (call action:"show")`) {
		t.Fatalf("block missing truncation trailer:\n%s", got)
	}
	if strings.Contains(got, "13. [") {
		t.Fatalf("block included a step past the cap:\n%s", got)
	}
}

func TestCompactPlanBlockReplacesOnSecondCompaction(t *testing.T) {
	agent := planCompactionAgent(t, []PlanStep{{Step: "alpha", Status: PlanPending}})
	compactOnce(t, agent)

	agent.SetPlan([]PlanStep{{Step: "beta", Status: PlanInProgress}})
	compactOnce(t, agent)

	got := internalContextText(agent.Messages()[0])
	if count := strings.Count(got, planContextHeader); count != 1 {
		t.Fatalf("plan header appeared %d times, want 1:\n%s", count, got)
	}
	if strings.Contains(got, "alpha") {
		t.Fatalf("stale plan block survived the second compaction:\n%s", got)
	}
	want := "clock\n\n## Current plan\n1. [~] beta"
	if got != want {
		t.Fatalf("second-compaction internal context =\n%q\nwant\n%q", got, want)
	}
}

func TestCompactPlanBlockHiddenWithoutRetainedInternalContext(t *testing.T) {
	agent := NewAgent(&compactLifecycleClient{}, "compact-model", "system", Registry{})
	agent.SetMessages([]provider.Message{
		textMessage(provider.RoleUser, "do the thing"),
		textMessage(provider.RoleAssistant, "working"),
	})
	agent.SetPlan([]PlanStep{{Step: "alpha", Status: PlanPending}})
	compactOnce(t, agent)

	// No retained internal-context message means nowhere to merge the block:
	// the transcript stays the bare summary.
	for _, message := range agent.Messages() {
		if strings.Contains(internalContextText(message), planContextHeader) {
			t.Fatalf("plan block injected without a retained internal-context message: %#v", agent.Messages())
		}
	}
}
