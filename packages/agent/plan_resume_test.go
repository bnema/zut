package agent

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/modes"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// sessionWithPlan writes a session whose persisted meta carries steps, then
// closes it and returns the path for a resume.
func sessionWithPlan(t *testing.T, providerName, model string, steps []core.PlanStep) string {
	t.Helper()
	sess, err := core.NewSession(t.TempDir(), t.TempDir(), providerName, model, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "resumed transcript"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sess.UpdatePlan(&core.SessionPlan{Steps: steps}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	return sess.Path
}

// TestResumeSeedsAgentPlanFromSessionMeta covers the merge-blocking bug: without
// seeding, a resumed session's file holds steps while core holds none.
func TestResumeSeedsAgentPlanFromSessionMeta(t *testing.T) {
	want := []core.PlanStep{
		{Step: "one", Status: core.PlanPending},
		{Step: "two", Status: core.PlanInProgress},
	}
	path := sessionWithPlan(t, "stored-provider", "stored-model", want)
	sess, _, err := core.OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	current := core.NewAgent(nil, "current-model", "", nil)
	candidate, err := applySessionResume(sess, current, "current-provider", "current-model", false, false, func(providerName, model string) (*core.Agent, string, string, error) {
		return core.NewAgent(nil, model, "", nil), providerName, model, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.session.Close()
	if !candidate.rebuilt {
		t.Fatal("resume did not rebuild the agent")
	}
	if got := candidate.agent.CurrentPlan(); !slices.Equal(got, want) {
		t.Fatalf("resumed plan = %#v, want %#v", got, want)
	}
}

// TestResumeSeedsReusedAgentPlan covers the same-provider path, where the live
// agent is reused rather than rebuilt: it still has to be seeded.
func TestResumeSeedsReusedAgentPlan(t *testing.T) {
	want := []core.PlanStep{{Step: "only", Status: core.PlanInProgress}}
	path := sessionWithPlan(t, "provider", "model", want)
	sess, _, err := core.OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	current := core.NewAgent(nil, "model", "", nil)
	candidate, err := applySessionResume(sess, current, "provider", "model", false, false, func(string, string) (*core.Agent, string, string, error) {
		t.Fatal("same provider/model unexpectedly rebuilt the agent")
		return nil, "", "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.session.Close()
	if candidate.agent != current {
		t.Fatalf("reused candidate agent = %p, want the live agent %p", candidate.agent, current)
	}
	if got := current.CurrentPlan(); !slices.Equal(got, want) {
		t.Fatalf("reused agent plan = %#v, want %#v", got, want)
	}
}

func TestRPCClearDropsPlanState(t *testing.T) {
	var out bytes.Buffer
	ag := &core.Agent{}
	ag.SetPlan([]core.PlanStep{{Step: "one", Status: core.PlanPending}})
	s := &rpcServer{agent: ag, out: &out}

	s.dispatch("clear", "1", []byte(`{"type":"clear","id":"1"}`))

	if got := ag.CurrentPlan(); got != nil {
		t.Fatalf("plan after rpc clear = %#v, want nil", got)
	}
	if !strings.Contains(out.String(), `"success":true`) {
		t.Fatalf("rpc clear response = %q", out.String())
	}
}

// appendPlanCall appends a plan tool call paired with a successful result. The
// load-time replay only counts a call that has a non-error result.
func appendPlanCall(t *testing.T, sess *core.Session, callID, name, args string) {
	t.Helper()
	if err := sess.AppendMessage(provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.ToolCallBlock{ID: callID, Name: name, Arguments: []byte(args)}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{
		Role: provider.RoleTool,
		Content: []provider.Content{provider.ToolResultBlock{
			CallID:  callID,
			Content: []provider.Content{provider.TextBlock{Text: "Plan updated"}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
}

func newResumeBuilder(t *testing.T) func(providerName, model string) (*core.Agent, string, string, error) {
	t.Helper()
	return func(providerName, model string) (*core.Agent, string, string, error) {
		return core.NewAgent(nil, model, "", nil), providerName, model, nil
	}
}

// TestResumePlanSnapshotsUseFullTranscriptBeyondTrimWindow guards GO-001: at
// resume only the trim window reaches the agent, so replaying its messages
// turns an in-window delta into a rebuild from an empty base. The reconstructed
// snapshots must instead come from the untrimmed session transcript.
func TestResumePlanSnapshotsUseFullTranscriptBeyondTrimWindow(t *testing.T) {
	sess, err := core.NewSession(t.TempDir(), t.TempDir(), "stored-provider", "stored-model", "test")
	if err != nil {
		t.Fatal(err)
	}
	appendPlanCall(t, sess, "set-1", "update_plan",
		`{"plan":[{"step":"one","status":"completed"},{"step":"two","status":"in_progress"},{"step":"three","status":"pending"}]}`)
	for i := 0; i < 120; i++ {
		if err := sess.AppendMessage(provider.Message{
			Role:    provider.RoleUser,
			Content: []provider.Content{provider.TextBlock{Text: fmt.Sprintf("filler-%d", i)}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	appendPlanCall(t, sess, "add-four", "plan", `{"action":"add","steps":[{"step":"four"}]}`)
	appendPlanCall(t, sess, "upd-two", "plan", `{"action":"update","index":2,"status":"completed"}`)
	final := []core.PlanStep{
		{Step: "one", Status: core.PlanCompleted},
		{Step: "two", Status: core.PlanCompleted},
		{Step: "three", Status: core.PlanPending},
		{Step: "four", Status: core.PlanPending},
	}
	if err := sess.UpdatePlan(&core.SessionPlan{Steps: final}); err != nil {
		t.Fatal(err)
	}
	path := sess.Path
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	resumed, _, err := core.OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	current := core.NewAgent(nil, "current-model", "", nil)
	candidate, err := applySessionResume(resumed, current, "current-provider", "current-model", false, false, newResumeBuilder(t))
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.session.Close()

	if got := candidate.agent.CurrentPlan(); !slices.Equal(got, final) {
		t.Fatalf("resumed plan = %#v, want %#v", got, final)
	}
	if len(candidate.planSnapshots) != 3 {
		t.Fatalf("snapshots = %#v, want the full transcript's replays", candidate.planSnapshots)
	}
	if got := candidate.planSnapshots["set-1"].Plan; !slices.Equal(got, []core.PlanStep{
		{Step: "one", Status: core.PlanCompleted},
		{Step: "two", Status: core.PlanInProgress},
		{Step: "three", Status: core.PlanPending},
	}) {
		t.Fatalf("set snapshot = %#v", got)
	}
	if got := candidate.planSnapshots["add-four"].Plan; !slices.Equal(got, []core.PlanStep{
		{Step: "one", Status: core.PlanCompleted},
		{Step: "two", Status: core.PlanInProgress},
		{Step: "three", Status: core.PlanPending},
		{Step: "four", Status: core.PlanPending},
	}) {
		t.Fatalf("add snapshot = %#v", got)
	}
	if got := candidate.planSnapshots["upd-two"].Plan; !slices.Equal(got, final) {
		t.Fatalf("update snapshot = %#v, want %#v", got, final)
	}
}

// TestResumeSeedsInteractivePlanCountersAndSnapshots covers the CLI resume
// wiring the session picker shares: the candidate agent is seeded from
// SessionMeta.Plan, and the Interactive reconstructs its counters and per-call
// snapshot map from the resume result.
func TestResumeSeedsInteractivePlanCountersAndSnapshots(t *testing.T) {
	want := []core.PlanStep{
		{Step: "one", Status: core.PlanCompleted},
		{Step: "two", Status: core.PlanInProgress},
	}
	sess, err := core.NewSession(t.TempDir(), t.TempDir(), "stored-provider", "stored-model", "test")
	if err != nil {
		t.Fatal(err)
	}
	appendPlanCall(t, sess, "set-1", "plan", `{"action":"set","steps":[{"step":"one","status":"completed"},{"step":"two","status":"in_progress"}]}`)
	if err := sess.UpdatePlan(&core.SessionPlan{Steps: want}); err != nil {
		t.Fatal(err)
	}
	path := sess.Path
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	resumed, _, err := core.OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	current := core.NewAgent(nil, "current-model", "", nil)
	candidate, err := applySessionResume(resumed, current, "current-provider", "current-model", false, false, newResumeBuilder(t))
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.session.Close()

	iv := modes.NewInteractive(modes.InteractiveConfig{
		Agent:                candidate.agent,
		CurrentPlan:          candidate.agent.CurrentPlan,
		InitialPlanSnapshots: candidate.planSnapshots,
	})
	if got := candidate.agent.CurrentPlan(); !slices.Equal(got, want) {
		t.Fatalf("resumed plan = %#v, want %#v", got, want)
	}
	if currentStep, total, snapshots := iv.PlanStatus(); currentStep != 2 || total != 2 || snapshots != 1 {
		t.Fatalf("interactive plan status = %d/%d with %d snapshots, want 2/2 with 1", currentStep, total, snapshots)
	}
}
