package core

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
)

func TestAgentRejectsEmptyPrompt(t *testing.T) {
	ag := NewAgent(nil, "model", "", nil)
	if err := ag.Prompt(context.Background(), "", nil, nil); err == nil {
		t.Fatal("empty prompt was accepted")
	}
	if got := ag.Messages(); len(got) != 0 {
		t.Fatalf("empty prompt appended %d messages", len(got))
	}
}

func TestAgentSetPromptConfigSwapsPromptAndToolsAtomically(t *testing.T) {
	oldTool := &recordingTool{}
	newTool := &recordingTool{}
	oldRegistry := Registry{"old": oldTool}
	newRegistry := Registry{"new": newTool}
	ag := NewAgent(nil, "model", "old system", oldRegistry)

	previous := ag.SetPromptConfig("new system", newRegistry)
	if previous["old"] != oldTool {
		t.Fatalf("previous registry = %#v, want old registry", previous)
	}
	system, tools := ag.PromptConfig()
	if system != "new system" {
		t.Fatalf("system prompt = %q, want new system", system)
	}
	if tools["new"] != newTool || len(tools) != 1 {
		t.Fatalf("current registry = %#v, want new registry", tools)
	}
}

func TestSessionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	os.Setenv("ZUT_HOME", dir)

	sess, err := NewSession(dir, "/tmp/project", "anthropic", "claude-sonnet-4-5", "test")
	if err != nil {
		t.Fatal(err)
	}
	msg := provider.Message{
		Role: provider.RoleUser,
		Content: []provider.Content{
			provider.TextBlock{Text: "hello"},
		},
		Time: time.Now().UTC(),
	}
	if err := sess.AppendMessage(msg); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, msgs, err := OpenSession(sess.Path)
	if err != nil {
		t.Fatal(err)
	}
	// OpenSession returns a live append writer; close it before t.TempDir
	// runs cleanup, otherwise windows refuses to delete the open file.
	t.Cleanup(func() { _ = reopened.Close() })
	if len(msgs) != 1 {
		t.Fatalf("got %d messages", len(msgs))
	}
	tb, ok := msgs[0].Content[0].(provider.TextBlock)
	if !ok || tb.Text != "hello" {
		t.Fatalf("got %+v", msgs[0])
	}
}

func TestSessionRoundTripPreservesThoughtSignatures(t *testing.T) {
	dir := t.TempDir()
	sess, err := NewSession(dir, "/tmp/project", "google", "gemini-3-flash", "test")
	if err != nil {
		t.Fatal(err)
	}
	msg := provider.Message{
		Role: provider.RoleAssistant,
		Content: []provider.Content{
			provider.ReasoningBlock{Summary: "thinking"},
			provider.TextBlock{Text: "answer", ThoughtSignature: "text-sig"},
			provider.ImageBlock{MimeType: "image/png", Data: []byte("png"), ThoughtSignature: "image-sig"},
			provider.ToolCallBlock{
				ID:               "call-1",
				Name:             "read",
				Arguments:        []byte(`{"path":"a"}`),
				ThoughtSignature: "tool-sig",
			},
		},
	}
	if err := sess.AppendMessage(msg); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{
		Role: provider.RoleTool,
		Content: []provider.Content{provider.ToolResultBlock{
			CallID:  "call-1",
			Content: []provider.Content{provider.TextBlock{Text: "result"}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, msgs, err := OpenSession(sess.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if len(msgs) != 2 || len(msgs[0].Content) != 4 {
		t.Fatalf("round trip content = %+v", msgs)
	}
	if rb, ok := msgs[0].Content[0].(provider.ReasoningBlock); !ok || rb.Summary != "thinking" {
		t.Fatalf("reasoning block = %#v", msgs[0].Content[0])
	}
	if tb, ok := msgs[0].Content[1].(provider.TextBlock); !ok || tb.ThoughtSignature != "text-sig" {
		t.Fatalf("text block = %#v", msgs[0].Content[1])
	}
	if ib, ok := msgs[0].Content[2].(provider.ImageBlock); !ok || ib.ThoughtSignature != "image-sig" {
		t.Fatalf("image block = %#v", msgs[0].Content[2])
	}
	if tc, ok := msgs[0].Content[3].(provider.ToolCallBlock); !ok || tc.ThoughtSignature != "tool-sig" {
		t.Fatalf("tool call block = %#v", msgs[0].Content[3])
	}
}

// TestNewSessionAtPathCreatesAtExplicitPath proves that callers with an
// explicit persistence location get the requested file and parent
// directories, independent of SessionsDir(root, cwd).
func TestNewSessionAtPathCreatesAtExplicitPath(t *testing.T) {
	dir := t.TempDir()
	want := dir + "/nested/sub/session.json"

	s, err := NewSessionAtPath(want, "/tmp/proj", "anthropic", "claude", "test")
	if err != nil {
		t.Fatalf("NewSessionAtPath: %v", err)
	}
	if s.Path != want {
		t.Errorf("Path = %q; want %q", s.Path, want)
	}
	if err := s.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "hi"}},
		Time:    time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen at the same path and the message must still be there.
	reopened, msgs, err := OpenSession(want)
	if err != nil {
		t.Fatalf("OpenSession at fixed path: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if len(msgs) != 1 {
		t.Fatalf("reopen got %d msgs; want 1", len(msgs))
	}

	// A second call to NewSessionAtPath at the same path must fail
	// (O_EXCL): callers should use OpenSession to reattach to an
	// existing file.
	if _, err := NewSessionAtPath(want, "/tmp/proj", "anthropic", "claude", "test"); err == nil {
		t.Error("NewSessionAtPath on existing path returned nil; want error")
	}
}

func TestSessionExtensionStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sess, err := NewSession(dir, "/tmp/project", "anthropic", "claude", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "prompt"}},
	}); err != nil {
		t.Fatal(err)
	}
	state := json.RawMessage(`{"phases":[{"id":"phase-1"}]}`)
	if err := sess.AppendExtensionState("tasked-phases", state); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, _, err := OpenSession(sess.Path)
	if err != nil {
		t.Fatal(err)
	}
	firstReopened := reopened
	t.Cleanup(func() { _ = firstReopened.Close() })
	if got := string(reopened.ExtensionState["tasked-phases"]); got != string(state) {
		t.Fatalf("extension state = %q, want %q", got, state)
	}
	if err := reopened.AppendExtensionState("tasked-phases", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.ExtensionState["tasked-phases"]; ok {
		t.Fatal("nil extension state did not clear the snapshot")
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _, err = OpenSession(sess.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, ok := reopened.ExtensionState["tasked-phases"]; ok {
		t.Fatal("cleared extension state was restored after reopening")
	}
}

func TestSessionRetainsExtensionStateWithoutMessages(t *testing.T) {
	dir := t.TempDir()
	sess, err := NewSession(dir, "/tmp/project", "anthropic", "claude", "test")
	if err != nil {
		t.Fatal(err)
	}
	state := json.RawMessage(`{"version":1}`)
	if err := sess.AppendExtensionState("tasked-phases", state); err != nil {
		t.Fatal(err)
	}
	path := sess.Path
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _, err := OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := string(reopened.ExtensionState["tasked-phases"]); got != string(state) {
		t.Fatalf("extension state = %q, want %q", got, state)
	}
}

func TestSessionCompactHandoffRoundTripAndClear(t *testing.T) {
	sess, err := NewSession(t.TempDir(), "/tmp/project", "anthropic", "claude", "test")
	if err != nil {
		t.Fatal(err)
	}
	state := json.RawMessage(`{"version":1,"reason":"status_rescue","rescue_attempts":1}`)
	if err := sess.UpdateCompactHandoff(state); err != nil {
		t.Fatal(err)
	}
	if got := string(sess.Meta.CompactHandoff); got != string(state) {
		t.Fatalf("live compact handoff = %q, want %q", got, state)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, _, err := OpenSession(sess.Path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(reopened.Meta.CompactHandoff); got != string(state) {
		t.Fatalf("reopened compact handoff = %q, want %q", got, state)
	}
	if err := reopened.UpdateCompactHandoff(nil); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, _, err = OpenSession(sess.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(reopened.Meta.CompactHandoff) != 0 {
		t.Fatalf("cleared compact handoff = %q, want empty", reopened.Meta.CompactHandoff)
	}
}

func TestNormalizeSessionGoalMetaMigratesLegacyGoal(t *testing.T) {
	meta := SessionMeta{ID: "legacy-session", Goal: &SessionGoal{Objective: "finish migration", Status: GoalActive}}
	normalizeSessionGoalMeta(&meta)
	if meta.Mission == nil || meta.Mission.ID != "legacy-legacy-session" || meta.Mission.Source != MissionSourceUser || meta.Mission.ActiveGoalID != "legacy-goal-legacy-session" {
		t.Fatalf("mission = %#v", meta.Mission)
	}
	if meta.Goal.Owner != GoalOwnerUser || meta.Goal.ID != "legacy-goal-legacy-session" || meta.Goal.MissionID != meta.Mission.ID || meta.Goal.Ordinal != 1 {
		t.Fatalf("goal = %#v", meta.Goal)
	}
}

func TestSessionGoalTransitionLimitRollsBackState(t *testing.T) {
	sess, err := NewSession(t.TempDir(), "/tmp/project", "anthropic", "claude", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.UpdateGoal(&SessionGoal{Objective: "initial", Status: GoalActive, Owner: GoalOwnerUser}); err != nil {
		t.Fatal(err)
	}
	for transition := 0; transition < maxMissionGoalTransitions; transition++ {
		done := *sess.Meta.Goal
		done.Status = GoalDone
		if err := sess.UpdateGoal(&done); err != nil {
			t.Fatalf("settle transition %d: %v", transition, err)
		}
		if err := sess.UpdateGoal(&SessionGoal{Objective: "next", Status: GoalActive, Owner: GoalOwnerManager}); err != nil {
			t.Fatalf("start transition %d: %v", transition, err)
		}
	}
	done := *sess.Meta.Goal
	done.Status = GoalDone
	if err := sess.UpdateGoal(&done); err != nil {
		t.Fatal(err)
	}
	previousMission := *sess.Meta.Mission
	previousGoal := *sess.Meta.Goal
	if err := sess.UpdateGoal(&SessionGoal{Objective: "one too many", Status: GoalActive, Owner: GoalOwnerManager}); err == nil {
		t.Fatal("transition limit was not enforced")
	}
	if *sess.Meta.Mission != previousMission || *sess.Meta.Goal != previousGoal {
		t.Fatalf("state changed after rejected transition: mission %#v, goal %#v", sess.Meta.Mission, sess.Meta.Goal)
	}
}

func TestSessionGoalResumeDoesNotConsumeTransitionLimit(t *testing.T) {
	sess, err := NewSession(t.TempDir(), "/tmp/project", "anthropic", "claude", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.UpdateGoal(&SessionGoal{Objective: "initial", Status: GoalActive, Owner: GoalOwnerUser}); err != nil {
		t.Fatal(err)
	}
	done := *sess.Meta.Goal
	done.Status = GoalDone
	if err := sess.UpdateGoal(&done); err != nil {
		t.Fatal(err)
	}
	if err := sess.UpdateGoal(&SessionGoal{Objective: "manager work", Status: GoalActive, Owner: GoalOwnerManager}); err != nil {
		t.Fatal(err)
	}
	for cycle := 0; cycle < maxMissionGoalTransitions*2; cycle++ {
		paused := *sess.Meta.Goal
		paused.Status = GoalPaused
		if err := sess.UpdateGoal(&paused); err != nil {
			t.Fatalf("pause cycle %d: %v", cycle, err)
		}
		resumed := *sess.Meta.Goal
		resumed.Status = GoalActive
		if err := sess.UpdateGoal(&resumed); err != nil {
			t.Fatalf("resume cycle %d: %v", cycle, err)
		}
	}
	if got := sess.Meta.Mission.TransitionCount; got != 1 {
		t.Fatalf("transition count = %d, want 1", got)
	}
}

func TestSessionLegacyGoalMigrationPersistsOnNextTransition(t *testing.T) {
	path := t.TempDir() + "/legacy.jsonl"
	meta := SessionMeta{ID: "legacy-session", CWD: "/tmp/project", Goal: &SessionGoal{Objective: "finish migration", Status: GoalActive}}
	line, err := json.Marshal(sessionLine{Type: "meta", Meta: &meta})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	sess, _, err := OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Meta.Mission == nil || sess.Meta.Goal.MissionID != sess.Meta.Mission.ID {
		t.Fatalf("legacy migration = mission %#v, goal %#v", sess.Meta.Mission, sess.Meta.Goal)
	}
	done := *sess.Meta.Goal
	done.Status = GoalDone
	if err := sess.UpdateGoal(&done); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, _, err := OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Meta.Mission == nil || reopened.Meta.Goal == nil || reopened.Meta.Goal.Status != GoalDone || reopened.Meta.Goal.MissionID != reopened.Meta.Mission.ID {
		t.Fatalf("persisted migration = mission %#v, goal %#v", reopened.Meta.Mission, reopened.Meta.Goal)
	}
}

func TestSessionEnsureMissionCreatesUserBoundaryOnce(t *testing.T) {
	sess, err := NewSession(t.TempDir(), "/tmp/project", "anthropic", "claude", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.EnsureMission("fix the reported failure", MissionSourceUser); err != nil {
		t.Fatal(err)
	}
	if sess.Meta.Mission == nil || sess.Meta.Mission.Objective != "fix the reported failure" || sess.Meta.Mission.Source != MissionSourceUser {
		t.Fatalf("mission = %#v", sess.Meta.Mission)
	}
	missionID := sess.Meta.Mission.ID
	if err := sess.EnsureMission("unrelated follow-up", MissionSourceUser); err != nil {
		t.Fatal(err)
	}
	if sess.Meta.Mission.ID != missionID || sess.Meta.Mission.Objective != "fix the reported failure" {
		t.Fatalf("mission was replaced: %#v", sess.Meta.Mission)
	}
	path := sess.Path
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, _, err := OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Meta.Mission == nil || reopened.Meta.Mission.ID != missionID {
		t.Fatalf("reopened mission = %#v", reopened.Meta.Mission)
	}
}

func TestSessionGoalRoundTripAndClear(t *testing.T) {
	sess, err := NewSession(t.TempDir(), "/tmp/project", "anthropic", "claude", "test")
	if err != nil {
		t.Fatal(err)
	}
	goal := &SessionGoal{Objective: "finish the requested change", Status: GoalActive}
	if err := sess.UpdateGoal(goal); err != nil {
		t.Fatal(err)
	}
	if sess.Meta.Goal == nil || sess.Meta.Goal.Objective != goal.Objective || sess.Meta.Goal.Status != goal.Status || sess.Meta.Goal.Owner != GoalOwnerUser || sess.Meta.Goal.ID == "" || sess.Meta.Goal.MissionID == "" || sess.Meta.Goal.Ordinal != 1 {
		t.Fatalf("live goal = %#v", sess.Meta.Goal)
	}
	if sess.Meta.Mission == nil || sess.Meta.Mission.ID != sess.Meta.Goal.MissionID || sess.Meta.Mission.Objective != goal.Objective || sess.Meta.Mission.ActiveGoalID != sess.Meta.Goal.ID {
		t.Fatalf("live mission = %#v", sess.Meta.Mission)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, _, err := OpenSession(sess.Path)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Meta.Goal == nil || reopened.Meta.Goal.Objective != goal.Objective || reopened.Meta.Goal.Status != goal.Status || reopened.Meta.Goal.MissionID != sess.Meta.Goal.MissionID {
		t.Fatalf("reopened goal = %#v", reopened.Meta.Goal)
	}
	if err := reopened.UpdateGoal(nil); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, _, err = OpenSession(sess.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Meta.Goal != nil {
		t.Fatalf("cleared goal = %#v, want nil", reopened.Meta.Goal)
	}
	if len(reopened.Meta.GoalHistory) != 1 || reopened.Meta.GoalHistory[0].Objective != goal.Objective || reopened.Meta.GoalHistory[0].Status != GoalActive {
		t.Fatalf("goal history = %#v", reopened.Meta.GoalHistory)
	}
}

func TestSessionGoalRuntimeRoundTripDoesNotAddHistory(t *testing.T) {
	sess, err := NewSession(t.TempDir(), "/tmp/project", "anthropic", "claude", "test")
	if err != nil {
		t.Fatal(err)
	}
	budget := uint64(1_000_000)
	if err := sess.UpdateGoal(&SessionGoal{Objective: "finish the requested change", Status: GoalActive, TokenBudget: &budget}); err != nil {
		t.Fatal(err)
	}
	goal := cloneSessionGoal(sess.Meta.Goal)
	goal.TokensUsed = 1234
	goal.ContinuationID = "run-1"
	goal.ConsecutiveNoProgressTurns = 1
	if err := sess.UpdateGoalRuntime(goal); err != nil {
		t.Fatal(err)
	}
	if len(sess.Meta.GoalHistory) != 0 {
		t.Fatalf("runtime update added history: %#v", sess.Meta.GoalHistory)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _, err := OpenSession(sess.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if goal := reopened.Meta.Goal; goal == nil || goal.TokenBudget == nil || *goal.TokenBudget != budget || goal.TokensUsed != 1234 || goal.ContinuationID != "run-1" || goal.ConsecutiveNoProgressTurns != 1 {
		t.Fatalf("reopened runtime goal = %#v", goal)
	}
	if len(reopened.Meta.GoalHistory) != 0 {
		t.Fatalf("reopened runtime history = %#v", reopened.Meta.GoalHistory)
	}
}

func TestSessionGoalTerminalStatusSettlesMission(t *testing.T) {
	for _, test := range []struct {
		goalStatus    GoalStatus
		missionStatus MissionStatus
	}{
		{GoalDone, MissionCompleted},
		{GoalPaused, MissionPaused},
		{GoalBlocked, MissionBlocked},
		{GoalBudgetLimited, MissionBlocked},
		{GoalStalled, MissionBlocked},
	} {
		t.Run(string(test.goalStatus), func(t *testing.T) {
			sess, err := NewSession(t.TempDir(), "/tmp/project", "anthropic", "claude", "test")
			if err != nil {
				t.Fatal(err)
			}
			defer sess.Close()
			if err := sess.UpdateGoal(&SessionGoal{Objective: "finish", Status: GoalActive}); err != nil {
				t.Fatal(err)
			}
			goal := cloneSessionGoal(sess.Meta.Goal)
			goal.Status = test.goalStatus
			goal.Reason = "stopped"
			if err := sess.UpdateGoal(goal); err != nil {
				t.Fatal(err)
			}
			if sess.Meta.Mission == nil || sess.Meta.Mission.Status != test.missionStatus || sess.Meta.Mission.ActiveGoalID != "" {
				t.Fatalf("mission = %#v", sess.Meta.Mission)
			}
		})
	}
}

func TestCostAdd(t *testing.T) {
	var c CostTracker
	c.Add(provider.Usage{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 20, ReasoningTokensKnown: true, CostUSD: 0.01})
	c.Add(provider.Usage{InputTokens: 200, OutputTokens: 25, ReasoningTokens: 10, ReasoningTokensKnown: true, CostUSD: 0.02})
	if c.Total.InputTokens != 300 || c.Total.OutputTokens != 75 {
		t.Fatalf("got %+v", c.Total)
	}
	if c.Total.ReasoningTokens != 30 || !c.Total.ReasoningTokensKnown {
		t.Fatalf("got reasoning usage %+v", c.Total)
	}
	if c.Total.CostUSD < 0.0299 || c.Total.CostUSD > 0.0301 {
		t.Fatalf("got cost %v", c.Total.CostUSD)
	}
}
