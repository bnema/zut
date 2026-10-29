package agent

import (
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/core"
)

// TestBotSessionPersistsPlanMutation guards GO-002 for daemon-hosted bot
// sessions: a headless session built by the session assembly must persist
// a plan mutation through the tool-result commit path, so a resume seeds
// the plan from the file instead of empty core state.
func TestBotSessionPersistsPlanMutation(t *testing.T) {
	sessionAssemblyEnv(t)
	server := sessionAssemblyProvider(t)
	deps, cleanup := newTestSessionDeps(t, session.CreateSpec{CWD: t.TempDir(), Env: []string{}, Args: sessionAssemblyArgs(server), Headless: true})
	ag, _, _, err := deps.AgentBuilder.BuildAgent()
	if err != nil {
		t.Fatal(err)
	}
	if ag.CommitToolResult == nil {
		t.Fatal("bot session did not wire CommitToolResult")
	}
	want := []core.PlanStep{{Step: "one", Status: core.PlanPending}}
	if err := ag.CommitToolResult("plan-call", planMutationResult(want)); err != nil {
		t.Fatal(err)
	}
	path := deps.SessionStore.CurrentSessionPath()
	if path == "" {
		t.Fatal("bot session has no session file")
	}
	deps.SessionStore.FlushSession()
	cleanup()

	reopened, _, err := core.OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Meta.Plan == nil {
		t.Fatal("reopened bot session lost the persisted plan")
	}
}
