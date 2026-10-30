package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/lsp"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/google/uuid"
)

func TestToolResultPersistenceUsesBoundSession(t *testing.T) {
	root := t.TempDir()
	original, err := core.NewSession(root, "original", "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	next, err := core.NewSession(root, "next", "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	for _, sess := range []*core.Session{original, next} {
		if err := sess.UpdateGoal(&core.SessionGoal{Objective: "finish", Status: core.GoalActive}); err != nil {
			t.Fatal(err)
		}
	}

	boundSession := original
	currentSession := next // Simulate a completed session switch before callback delivery.
	result := core.ToolResult{Details: tools.GoalUpdate{Status: core.GoalDone}}
	if err := persistToolResultState(nil, boundSession, result); err != nil {
		t.Fatal(err)
	}

	if original.Meta.Goal == nil || original.Meta.Goal.Status != core.GoalDone {
		t.Fatalf("original goal = %#v, want done", original.Meta.Goal)
	}
	if currentSession.Meta.Goal == nil || currentSession.Meta.Goal.Status != core.GoalActive {
		t.Fatalf("current goal = %#v, want active", currentSession.Meta.Goal)
	}
}

func TestPersistToolResultStateUsesSafeGoalPersistenceCategories(t *testing.T) {
	const privateGoal = "private goal text"
	const privateMission = "private-mission-id"
	const privateReason = "private active reason"
	const privateSessionContent = "private session content"
	var writeFailureGoal core.SessionGoal

	newSession := func(t *testing.T) *core.Session {
		t.Helper()
		sess, err := core.NewSession(t.TempDir(), t.TempDir(), "provider", "model", "version")
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: privateSessionContent}}}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sess.Close() })
		return sess
	}

	tests := []struct {
		name            string
		setup           func(*testing.T) *core.Session
		update          tools.GoalUpdate
		message         string
		assertUnchanged func(*testing.T, *core.Session)
	}{
		{
			name: "active goal",
			setup: func(t *testing.T) *core.Session {
				sess := newSession(t)
				if err := sess.UpdateGoal(&core.SessionGoal{Objective: privateGoal, Status: core.GoalActive}); err != nil {
					t.Fatal(err)
				}
				return sess
			},
			update:  tools.GoalUpdate{Status: core.GoalActive, Objective: privateGoal},
			message: "goal transition rejected: the current goal is still active",
		},
		{
			name: "mismatched mission",
			setup: func(t *testing.T) *core.Session {
				sess := newSession(t)
				if err := sess.EnsureMission(privateGoal, core.MissionSourceUser); err != nil {
					t.Fatal(err)
				}
				return sess
			},
			update:  tools.GoalUpdate{Status: core.GoalActive, Objective: privateGoal, MissionID: privateMission},
			message: "goal transition rejected: it does not belong to the active mission",
		},
		{
			name:    "storage unavailable",
			setup:   func(*testing.T) *core.Session { return nil },
			update:  tools.GoalUpdate{Status: core.GoalActive, Objective: privateGoal, MissionID: privateMission},
			message: "goal state unavailable: session persistence is disabled",
		},
		{
			name: "write failure",
			setup: func(t *testing.T) *core.Session {
				sess := newSession(t)
				if err := sess.UpdateGoal(&core.SessionGoal{Objective: privateGoal, Status: core.GoalActive, Reason: privateReason}); err != nil {
					t.Fatal(err)
				}
				writeFailureGoal = *sess.Meta.Goal
				if err := sess.Close(); err != nil {
					t.Fatal(err)
				}
				return sess
			},
			update:  tools.GoalUpdate{Status: core.GoalDone},
			message: "goal state could not be saved",
			assertUnchanged: func(t *testing.T, sess *core.Session) {
				t.Helper()
				assertGoal := func(source string, goal *core.SessionGoal) {
					t.Helper()
					if goal == nil || goal.Status != writeFailureGoal.Status || goal.Objective != writeFailureGoal.Objective || goal.Reason != writeFailureGoal.Reason {
						t.Fatalf("%s goal after failed update = %#v, want status=%q objective=%q reason=%q", source, goal, writeFailureGoal.Status, writeFailureGoal.Objective, writeFailureGoal.Reason)
					}
				}
				assertGoal("in-memory", sess.Meta.Goal)
				snapshot, err := core.ReadSessionSnapshot(sess.Path)
				if err != nil {
					t.Fatal(err)
				}
				assertGoal("persisted", snapshot.Meta.Goal)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sess := test.setup(t)
			err := persistToolResultState(nil, sess, core.ToolResult{Details: test.update})
			var safe *core.ToolResultCommitError
			if !errors.As(err, &safe) {
				t.Fatalf("persist error = %T %v, want safe tool result commit error", err, err)
			}
			if safe.Message != test.message {
				t.Fatalf("safe message = %q, want %q", safe.Message, test.message)
			}
			if strings.Contains(safe.Message, privateGoal) || strings.Contains(safe.Message, privateMission) || strings.Contains(safe.Message, privateSessionContent) || (sess != nil && strings.Contains(safe.Message, sess.Path)) {
				t.Fatalf("safe message leaked private state: %q", safe.Message)
			}
			if test.assertUnchanged != nil {
				test.assertUnchanged(t, sess)
			}
		})
	}
}

func TestPersistGoalToolResultStartsManagerMission(t *testing.T) {
	sess, err := core.NewSession(t.TempDir(), t.TempDir(), "provider", "model", "version")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	result := core.ToolResult{Details: tools.GoalUpdate{Status: core.GoalActive, Objective: "reproduce the reported failure"}}
	if err := persistGoalToolResult(sess, result); err != nil {
		t.Fatal(err)
	}
	goal := sess.Meta.Goal
	if goal == nil || goal.Status != core.GoalActive || goal.Objective != "reproduce the reported failure" || goal.Owner != core.GoalOwnerManager {
		t.Fatalf("persisted goal = %#v", goal)
	}
	mission := sess.Meta.Mission
	if mission == nil || mission.Source != core.MissionSourceManager || mission.Objective != goal.Objective || mission.ID != goal.MissionID || mission.ActiveGoalID != goal.ID {
		t.Fatalf("persisted mission = %#v, goal = %#v", mission, goal)
	}
}

func TestPersistGoalToolResultAppliesConfiguredBudget(t *testing.T) {
	sess, err := core.NewSession(t.TempDir(), t.TempDir(), "provider", "model", "version")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.EnsureMission("reproduce the reported failure", core.MissionSourceUser); err != nil {
		t.Fatal(err)
	}

	budget := uint64(1_000_000)
	result := core.ToolResult{Details: tools.GoalUpdate{Status: core.GoalActive, Objective: "reproduce the reported failure", MissionID: sess.Meta.Mission.ID}}
	if err := persistGoalToolResult(sess, result, &budget); err != nil {
		t.Fatal(err)
	}
	if goal := sess.Meta.Goal; goal == nil || goal.TokenBudget == nil || *goal.TokenBudget != budget {
		t.Fatalf("persisted goal budget = %#v", goal)
	}
}

func TestPersistGoalToolResultStartsGoalInCurrentMissionWithoutID(t *testing.T) {
	sess, err := core.NewSession(t.TempDir(), t.TempDir(), "provider", "model", "version")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.EnsureMission("fix the reported failure", core.MissionSourceUser); err != nil {
		t.Fatal(err)
	}
	missionID := sess.Meta.Mission.ID

	result := core.ToolResult{Details: tools.GoalUpdate{Status: core.GoalActive, Objective: "inspect the failure"}}
	if err := persistGoalToolResult(sess, result); err != nil {
		t.Fatal(err)
	}
	goal := sess.Meta.Goal
	if goal == nil || goal.Status != core.GoalActive || goal.MissionID != missionID {
		t.Fatalf("goal = %#v, want active goal in mission %q", goal, missionID)
	}
}

func TestPersistGoalToolResultPausesAndResumesCurrentGoal(t *testing.T) {
	sess, err := core.NewSession(t.TempDir(), t.TempDir(), "provider", "model", "version")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.UpdateGoal(&core.SessionGoal{Objective: "current work", Status: core.GoalActive}); err != nil {
		t.Fatal(err)
	}
	goalID := sess.Meta.Goal.ID
	missionID := sess.Meta.Mission.ID

	if err := persistGoalToolResult(sess, core.ToolResult{Details: tools.GoalUpdate{Status: core.GoalPaused}}); err != nil {
		t.Fatal(err)
	}
	if got := sess.Meta.Goal; got == nil || got.Status != core.GoalPaused || got.ID != goalID || got.Objective != "current work" {
		t.Fatalf("paused goal = %#v", got)
	}

	if err := persistGoalToolResult(sess, core.ToolResult{Details: tools.GoalUpdate{Status: core.GoalActive, MissionID: missionID}}); err != nil {
		t.Fatal(err)
	}
	if got := sess.Meta.Goal; got == nil || got.Status != core.GoalActive || got.ID != goalID || got.Objective != "current work" {
		t.Fatalf("resumed goal = %#v", got)
	}
}

func TestPersistGoalToolResultRejectsPauseForDifferentMission(t *testing.T) {
	sess, err := core.NewSession(t.TempDir(), t.TempDir(), "provider", "model", "version")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.UpdateGoal(&core.SessionGoal{Objective: "current work", Status: core.GoalActive}); err != nil {
		t.Fatal(err)
	}

	err = persistGoalToolResult(sess, core.ToolResult{Details: tools.GoalUpdate{Status: core.GoalPaused, MissionID: "other-mission"}})
	if !errors.Is(err, errGoalMissionMismatch) {
		t.Fatalf("error = %v, want %v", err, errGoalMissionMismatch)
	}
	if got := sess.Meta.Goal; got == nil || got.Status != core.GoalActive {
		t.Fatalf("goal changed after rejected pause: %#v", got)
	}
}

func TestPersistGoalToolResultRejectsActiveGoalReplacement(t *testing.T) {
	sess, err := core.NewSession(t.TempDir(), t.TempDir(), "provider", "model", "version")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.UpdateGoal(&core.SessionGoal{Objective: "current work", Status: core.GoalActive}); err != nil {
		t.Fatal(err)
	}

	err = persistGoalToolResult(sess, core.ToolResult{Details: tools.GoalUpdate{Status: core.GoalActive, Objective: "unrelated work"}})
	if err == nil {
		t.Fatal("active goal replacement succeeded")
	}
	if got := sess.Meta.Goal; got == nil || got.Objective != "current work" || got.Status != core.GoalActive {
		t.Fatalf("persisted goal = %#v", got)
	}
}

func TestPersistGoalToolResultSupersedesActiveGoalInMission(t *testing.T) {
	sess, err := core.NewSession(t.TempDir(), t.TempDir(), "provider", "model", "version")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.UpdateGoal(&core.SessionGoal{Objective: "try the legacy API", Status: core.GoalActive}); err != nil {
		t.Fatal(err)
	}
	missionID := sess.Meta.Mission.ID
	originalID := sess.Meta.Goal.ID

	result := core.ToolResult{Details: tools.GoalUpdate{Status: core.GoalSuperseded, Objective: "use the supported API", GoalID: originalID, MissionID: missionID}}
	if err := persistGoalToolResult(sess, result); err != nil {
		t.Fatal(err)
	}
	if got := sess.Meta.Goal; got == nil || got.Objective != "use the supported API" || got.Status != core.GoalActive || got.MissionID != missionID || got.Owner != core.GoalOwnerManager {
		t.Fatalf("replacement goal = %#v", got)
	}
	if len(sess.Meta.GoalHistory) != 1 || sess.Meta.GoalHistory[0].ID != originalID || sess.Meta.GoalHistory[0].Status != core.GoalSuperseded {
		t.Fatalf("goal history = %#v", sess.Meta.GoalHistory)
	}
}

func TestPersistGoalToolResultRejectsStaleSupersedingGoal(t *testing.T) {
	sess, err := core.NewSession(t.TempDir(), t.TempDir(), "provider", "model", "version")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.UpdateGoal(&core.SessionGoal{Objective: "new user goal", Status: core.GoalActive}); err != nil {
		t.Fatal(err)
	}
	before := *sess.Meta.Goal

	result := core.ToolResult{Details: tools.GoalUpdate{Status: core.GoalSuperseded, Objective: "stale replacement", GoalID: "older-goal", MissionID: sess.Meta.Mission.ID}}
	if err := persistGoalToolResult(sess, result); !errors.Is(err, errGoalStaleUpdate) {
		t.Fatalf("persist stale supersede error = %v", err)
	}
	if *sess.Meta.Goal != before || len(sess.Meta.GoalHistory) != 0 {
		t.Fatalf("stale supersede changed state: goal=%#v history=%#v", sess.Meta.Goal, sess.Meta.GoalHistory)
	}
}

func TestPersistGoalToolResultCreatesNextGoalInMission(t *testing.T) {
	sess, err := core.NewSession(t.TempDir(), t.TempDir(), "provider", "model", "version")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.UpdateGoal(&core.SessionGoal{Objective: "reproduce", Status: core.GoalActive}); err != nil {
		t.Fatal(err)
	}
	missionID := sess.Meta.Mission.ID
	if err := persistGoalToolResult(sess, core.ToolResult{Details: tools.GoalUpdate{Status: core.GoalDone}}); err != nil {
		t.Fatal(err)
	}
	if err := persistGoalToolResult(sess, core.ToolResult{Details: tools.GoalUpdate{Status: core.GoalActive, Objective: "implement fix", MissionID: missionID}}); err != nil {
		t.Fatal(err)
	}
	goal := sess.Meta.Goal
	if goal == nil || goal.Objective != "implement fix" || goal.MissionID != missionID || goal.Owner != core.GoalOwnerManager || goal.Ordinal != 3 {
		t.Fatalf("next goal = %#v", goal)
	}
}

func TestPersistGoalToolResultRejectsMismatchedMission(t *testing.T) {
	sess, err := core.NewSession(t.TempDir(), t.TempDir(), "provider", "model", "version")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.EnsureMission("current work", core.MissionSourceUser); err != nil {
		t.Fatal(err)
	}

	err = persistGoalToolResult(sess, core.ToolResult{Details: tools.GoalUpdate{Status: core.GoalActive, Objective: "next work", MissionID: "other-mission"}})
	if err == nil {
		t.Fatal("mismatched mission was accepted")
	}
}

func TestPersistGoalToolResultRejectsBlockedWithoutReason(t *testing.T) {
	sess, err := core.NewSession(t.TempDir(), "cwd", "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.UpdateGoal(&core.SessionGoal{Objective: "finish the change", Status: core.GoalActive}); err != nil {
		t.Fatal(err)
	}

	result := core.ToolResult{Details: tools.GoalUpdate{Status: core.GoalBlocked}}
	if err := persistGoalToolResult(sess, result); err != nil {
		t.Fatal(err)
	}
	if got := sess.Meta.Goal; got == nil || got.Status != core.GoalActive {
		t.Fatalf("persisted goal = %#v, want active", got)
	}
}

func TestPersistGoalToolResultUpdatesActiveGoal(t *testing.T) {
	sess, err := core.NewSession(t.TempDir(), "cwd", "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.UpdateGoal(&core.SessionGoal{Objective: "finish the change", Status: core.GoalActive}); err != nil {
		t.Fatal(err)
	}

	result := core.ToolResult{Details: tools.GoalUpdate{Status: core.GoalBlocked, Reason: "need user input"}}
	if err := persistGoalToolResult(sess, result); err != nil {
		t.Fatal(err)
	}
	if got := sess.Meta.Goal; got == nil || got.Status != core.GoalBlocked || got.Reason != "need user input" {
		t.Fatalf("persisted goal = %#v", got)
	}
}

type failingStdinReader struct{}

func (failingStdinReader) Read([]byte) (int, error) { return 0, errors.New("stdin failed") }

func TestReadAllStdinFromPropagatesReadErrors(t *testing.T) {
	if _, err := readAllStdinFrom(failingStdinReader{}); err == nil || !strings.Contains(err.Error(), "stdin failed") {
		t.Fatalf("readAllStdinFrom error = %v, want stdin failure", err)
	}
}

func TestOpenOrCreateSessionResumesByUUIDAcrossCWDAndAgentStores(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	otherCWD := t.TempDir()
	t.Setenv("ZUT_HOME", root)

	session, err := core.NewSession(root, cwd, "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "resume me"}},
	}); err != nil {
		t.Fatal(err)
	}
	id := session.ID
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	ag := core.NewAgent(nil, "model", "", nil)
	got, _, err := openOrCreateSessionState(context.Background(), Args{
		CWD:             otherCWD,
		Resume:          true,
		ResumeSessionID: id,
	}, Resolved{Provider: "provider", Model: "model"}, ag, "test")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != id || got.Meta.CWD != cwd {
		t.Fatalf("resumed session = %#v, want id=%q cwd=%q", got, id, cwd)
	}
	if got := firstMessageText(ag.Messages()); got != "resume me" {
		t.Fatalf("resumed message = %q, want resume me", got)
	}
	if err := got.Close(); err != nil {
		t.Fatal(err)
	}

	namedRoot := filepath.Join(root, "sessions", "agents", "demo")
	named, err := core.NewSession(namedRoot, cwd, "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := named.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "named resume me"}},
	}); err != nil {
		t.Fatal(err)
	}
	namedID := named.ID
	if err := named.Close(); err != nil {
		t.Fatal(err)
	}

	namedAgent := core.NewAgent(nil, "model", "", nil)
	namedGot, _, err := openOrCreateSessionState(context.Background(), Args{
		CWD:             otherCWD,
		Resume:          true,
		ResumeSessionID: namedID,
	}, Resolved{Provider: "provider", Model: "model"}, namedAgent, "test")
	if err != nil {
		t.Fatal(err)
	}
	if namedGot == nil || namedGot.ID != namedID || namedGot.Path != named.Path {
		t.Fatalf("named resumed session = %#v, want path %q", namedGot, named.Path)
	}
	if got := firstMessageText(namedAgent.Messages()); got != "named resume me" {
		t.Fatalf("named resumed message = %q, want named resume me", got)
	}
	if err := namedGot.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenOrCreateSessionResumesPersistedEmptyExplicitUUIDSession(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	otherCWD := t.TempDir()
	t.Setenv("ZUT_HOME", root)

	dir := core.SessionsDir(root, cwd)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	metaLine := struct {
		Type string           `json:"type"`
		Meta core.SessionMeta `json:"meta"`
	}{
		Type: "meta",
		Meta: core.SessionMeta{
			ID:       id,
			CWD:      cwd,
			Provider: "provider",
			Model:    "model",
			Started:  time.Now().UTC(),
			Version:  "test",
		},
	}
	data, err := json.Marshal(metaLine)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "persisted-empty.jsonl")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	sess, _, err := openOrCreateSessionState(context.Background(), Args{
		CWD:             otherCWD,
		Resume:          true,
		ResumeSessionID: id,
	}, Resolved{Provider: "provider", Model: "model"}, core.NewAgent(nil, "model", "", nil), "test")
	if err != nil {
		t.Fatal(err)
	}
	if sess == nil || sess.Path != path {
		t.Fatalf("resumed persisted empty session = %#v, want path %q", sess, path)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenOrCreateSessionUnknownUUIDDoesNotCreateSession(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ZUT_HOME", root)
	id := uuid.NewString()

	_, _, err := openOrCreateSessionState(context.Background(), Args{
		CWD:             cwd,
		Resume:          true,
		ResumeSessionID: id,
	}, Resolved{Provider: "provider", Model: "model"}, core.NewAgent(nil, "model", "", nil), "test")
	if err == nil {
		t.Fatal("unknown UUID resume unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), id) {
		t.Fatalf("unknown UUID error = %v, want session ID", err)
	}
	if sessions := core.ListSessions(root, cwd); len(sessions) != 0 {
		t.Fatalf("unknown UUID created sessions: %v", sessions)
	}
}

func TestOpenOrCreateSessionUUIDLookupIgnoresUnrelatedCorruption(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ZUT_HOME", root)
	target, err := core.NewSession(root, cwd, "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "resume me"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(core.SessionsDir(root, cwd), "bad.jsonl"), []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	session, _, err := openOrCreateSessionState(context.Background(), Args{
		CWD:             cwd,
		Resume:          true,
		ResumeSessionID: target.ID,
	}, Resolved{Provider: "provider", Model: "model"}, core.NewAgent(nil, "model", "", nil), "test")
	if err != nil {
		t.Fatalf("openOrCreateSessionState: %v", err)
	}
	defer session.Close()
	if session.Path != target.Path {
		t.Fatalf("resumed session path = %q, want %q", session.Path, target.Path)
	}
}

func TestTrimMessagesForResumeCarriesDeferredToolActivation(t *testing.T) {
	msgs := make([]provider.Message, 0, 101)
	msgs = append(msgs, provider.Message{
		Role:           provider.RoleTool,
		AddedToolNames: []string{"lookup_weather"},
		Content:        []provider.Content{provider.ToolResultBlock{CallID: "old-call"}},
	})
	for idx := 1; idx < 101; idx++ {
		msgs = append(msgs, provider.Message{
			Role:    provider.RoleUser,
			Content: []provider.Content{provider.TextBlock{Text: "message"}},
		})
	}
	trimmed := trimMessagesForResume(msgs, 100)
	if len(trimmed) != 100 {
		t.Fatalf("trimmed message count = %d, want 100", len(trimmed))
	}
	if len(trimmed[0].AddedToolNames) != 1 || trimmed[0].AddedToolNames[0] != "lookup_weather" {
		t.Fatalf("trimmed activation markers = %v, want lookup_weather", trimmed[0].AddedToolNames)
	}
}

func TestTrimMessagesForResumeKeepsCompactionSummaryAtTailBoundary(t *testing.T) {
	for _, total := range []int{100, 101, 102} {
		t.Run(fmt.Sprintf("%d messages", total), func(t *testing.T) {
			msgs := make([]provider.Message, 0, total)
			msgs = append(msgs, provider.Message{
				Role: provider.RoleUser,
				Meta: map[string]string{"compaction": "true"},
				Content: []provider.Content{
					provider.TextBlock{Text: "## Context Summary (compacted)"},
				},
			})
			for idx := 1; idx < total; idx++ {
				msgs = append(msgs, provider.Message{
					Role:    provider.RoleUser,
					Content: []provider.Content{provider.TextBlock{Text: fmt.Sprintf("message-%d", idx)}},
				})
			}

			trimmed := trimMessagesForResume(msgs, 100)
			if len(trimmed) > 100 {
				t.Fatalf("trimmed message count = %d, want at most 100", len(trimmed))
			}
			if len(trimmed) == 0 || trimmed[0].Meta["compaction"] != "true" {
				t.Fatalf("trimmed transcript lost compaction summary: %+v", trimmed)
			}
		})
	}
}

func TestPersistModelCallbackDoesNotReenterSessionTransition(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	sess, err := core.NewSession(t.TempDir(), t.TempDir(), "old-provider", "old-model", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	var persistMu sync.Mutex
	activeProvider, activeModel := "old-provider", "old-model"
	persistModel := newPersistModelCallback(&persistMu, &sess, &activeProvider, &activeModel, nil)

	var transitionMu sync.RWMutex
	sessionTransition := newSessionTransition(&transitionMu)
	done := make(chan struct{})
	go func() {
		sessionTransition(func() {
			persistModel("new-provider", "new-model")
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("model persistence deadlocked inside the session transition")
	}
	if sess.Meta.Provider != "new-provider" || sess.Meta.Model != "new-model" {
		t.Fatalf("session model = %q/%q, want new-provider/new-model", sess.Meta.Provider, sess.Meta.Model)
	}
	if activeProvider != "new-provider" || activeModel != "new-model" {
		t.Fatalf("active model = %q/%q, want new-provider/new-model", activeProvider, activeModel)
	}
}

func firstMessageText(messages []provider.Message) string {
	if len(messages) == 0 || len(messages[0].Content) == 0 {
		return ""
	}
	text, _ := messages[0].Content[0].(provider.TextBlock)
	return text.Text
}

func syntheticSession(t *testing.T, providerName, model string, usage provider.Usage) string {
	t.Helper()
	root := t.TempDir()
	cwd := t.TempDir()
	sess, err := core.NewSession(root, cwd, providerName, model, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "resumed transcript"}},
	}); err != nil {
		t.Fatal(err)
	}
	if usage != (provider.Usage{}) {
		if err := sess.AppendUsage(usage, usage); err != nil {
			t.Fatal(err)
		}
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	return sess.Path
}

func TestApplySessionResumeKeepsFreshEmptySessionOwned(t *testing.T) {
	root := t.TempDir()
	sess, err := core.NewSession(root, "/workspace", "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	ag := core.NewAgent(nil, "model", "", nil)
	candidate, err := applySessionResume(sess, ag, "provider", "model", false, false, func(string, string) (*core.Agent, string, string, error) {
		t.Fatal("empty session rebuilt the agent")
		return nil, "", "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	gotSess, gotAgent, providerName, model := candidate.session, candidate.agent, candidate.provider, candidate.model
	if gotSess != sess || gotAgent != ag || providerName != "provider" || model != "model" {
		t.Fatalf("empty startup resume = session %p/%p agent %p/%p provider/model %q/%q", gotSess, sess, gotAgent, ag, providerName, model)
	}
	if _, err := os.Stat(sess.Path); err != nil {
		t.Fatalf("fresh session disappeared before close: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sess.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fresh empty session remained after close: %v", err)
	}
}

func TestPrepareSessionResumeRebuildsForStoredProviderAndModel(t *testing.T) {
	old := core.NewAgent(nil, "old-model", "", nil)
	old.SetMessages([]provider.Message{{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "current transcript"}},
	}})
	path := syntheticSession(t, "stored-provider", "stored-model", provider.Usage{
		InputTokens:  12,
		OutputTokens: 7,
	})

	var gotProvider, gotModel string
	candidate, err := prepareSessionResume(path, old, "current-provider", "old-model", func(providerName, model string) (*core.Agent, string, string, error) {
		gotProvider, gotModel = providerName, model
		return core.NewAgent(nil, model, "", nil), providerName, model, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.session.Close()

	if gotProvider != "stored-provider" || gotModel != "stored-model" {
		t.Fatalf("builder selection = %q/%q, want stored-provider/stored-model", gotProvider, gotModel)
	}
	if !candidate.rebuilt || candidate.agent == old {
		t.Fatalf("candidate rebuilt = %v, agent=%p, old=%p", candidate.rebuilt, candidate.agent, old)
	}
	if got := firstMessageText(candidate.agent.Messages()); got != "resumed transcript" {
		t.Fatalf("candidate transcript = %q, want resumed transcript", got)
	}
	if got := candidate.agent.Cost(); got.InputTokens != 12 || got.OutputTokens != 7 {
		t.Fatalf("candidate usage = %+v, want input=12 output=7", got)
	}
	if got := old.Messages(); len(got) != 1 || firstMessageText(got) != "current transcript" {
		t.Fatalf("current transcript changed during preparation: %+v", got)
	}
}

func TestApplySessionResumeBindsRebuiltAgentToDurableSessionID(t *testing.T) {
	path := syntheticSession(t, "stored-provider", "stored-model", provider.Usage{})
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
	if got := candidate.agent.SessionID(); got != candidate.session.Meta.ID {
		t.Fatalf("rebuilt agent session ID = %q, want durable session ID %q", got, candidate.session.Meta.ID)
	}
}

func TestPrepareSessionResumeForWorkspaceRebuildsMatchingProviderAndModel(t *testing.T) {
	path := syntheticSession(t, "provider", "model", provider.Usage{})
	current := core.NewAgent(nil, "model", "", nil)
	builds := 0
	candidate, err := prepareSessionResumeForWorkspace(path, current, "provider", "model", func(providerName, model string) (*core.Agent, string, string, error) {
		builds++
		return core.NewAgent(nil, model, "", nil), providerName, model, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.session.Close()
	if builds != 1 || !candidate.rebuilt || candidate.agent == current {
		t.Fatalf("workspace resume = builds %d rebuilt %v agent=%p current=%p", builds, candidate.rebuilt, candidate.agent, current)
	}
}

func TestPrepareSessionResumePreservesLegacyMissingMetadata(t *testing.T) {
	old := core.NewAgent(nil, "old-model", "", nil)
	old.SetMessages([]provider.Message{{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "current transcript"}},
	}})
	path := syntheticSession(t, "", "", provider.Usage{})

	candidate, err := prepareSessionResume(path, old, "current-provider", "old-model", func(string, string) (*core.Agent, string, string, error) {
		t.Fatal("legacy session unexpectedly requested a rebuild")
		return nil, "", "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.session.Close()

	if candidate.rebuilt || candidate.agent != old {
		t.Fatalf("legacy candidate = rebuilt %v, agent=%p; want current agent without rebuild", candidate.rebuilt, candidate.agent)
	}
	if got := firstMessageText(old.Messages()); got != "current transcript" {
		t.Fatalf("current transcript changed before commit: %q", got)
	}
	if got := firstMessageText(candidate.messages); got != "resumed transcript" {
		t.Fatalf("legacy candidate transcript = %q, want resumed transcript", got)
	}
}

func TestPrepareSessionResumePreservesCompactHandoffMetadata(t *testing.T) {
	path := syntheticSession(t, "stored-provider", "stored-model", provider.Usage{})
	session, _, err := core.OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	handoff := json.RawMessage(`{"version":1,"reason":"status_rescue","rescue_attempts":1}`)
	if err := session.UpdateCompactHandoff(handoff); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	current := core.NewAgent(nil, "current-model", "", nil)
	candidate, err := prepareSessionResume(path, current, "stored-provider", "stored-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.session.Close()
	if got := string(candidate.session.Meta.CompactHandoff); got != string(handoff) {
		t.Fatalf("candidate compact handoff = %q, want %q", got, handoff)
	}
}

func TestPrepareSessionResumeFailureLeavesCurrentAndCandidateFileUsable(t *testing.T) {
	old := core.NewAgent(nil, "old-model", "", nil)
	old.SetMessages([]provider.Message{{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "current transcript"}},
	}})
	path := syntheticSession(t, "stored-provider", "stored-model", provider.Usage{})
	wantErr := errors.New("synthetic builder failure")

	if _, err := prepareSessionResume(path, old, "current-provider", "old-model", func(string, string) (*core.Agent, string, string, error) {
		return nil, "", "", wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("prepare error = %v, want %v", err, wantErr)
	}
	if got := firstMessageText(old.Messages()); got != "current transcript" {
		t.Fatalf("current transcript changed after failed preparation: %q", got)
	}

	// The failed candidate must have released its append handle, leaving
	// the selected file readable and reopenable for a later retry.
	reopened, _, err := core.OpenSession(path)
	if err != nil {
		t.Fatalf("reopen candidate after failed preparation: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("candidate session disappeared after failed preparation: %v", err)
	}
}

func TestPrepareSessionResumeHonorsExplicitProviderModelFields(t *testing.T) {
	for _, tc := range []struct {
		name             string
		explicitProvider bool
		explicitModel    bool
		wantProvider     string
		wantModel        string
		wantBuild        bool
	}{
		{name: "both", explicitProvider: true, explicitModel: true, wantProvider: "current-provider", wantModel: "current-model"},
		{name: "provider-only", explicitProvider: true, wantProvider: "current-provider", wantModel: "stored-model", wantBuild: true},
		{name: "model-only", explicitModel: true, wantProvider: "stored-provider", wantModel: "current-model", wantBuild: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := core.NewAgent(nil, "old-model", "", nil)
			path := syntheticSession(t, "stored-provider", "stored-model", provider.Usage{})
			var gotProvider, gotModel string
			candidate, err := prepareSessionResumeWithOptions(path, old, "current-provider", "current-model", tc.explicitProvider, tc.explicitModel, false, func(providerName, model string) (*core.Agent, string, string, error) {
				gotProvider, gotModel = providerName, model
				return core.NewAgent(nil, model, "", nil), providerName, model, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer candidate.session.Close()
			if tc.wantBuild {
				if gotProvider != tc.wantProvider || gotModel != tc.wantModel {
					t.Fatalf("builder selection = %q/%q, want %q/%q", gotProvider, gotModel, tc.wantProvider, tc.wantModel)
				}
			} else if candidate.provider != tc.wantProvider || candidate.model != tc.wantModel || candidate.rebuilt {
				t.Fatalf("candidate selection = %q/%q rebuilt=%v, want %q/%q without rebuild", candidate.provider, candidate.model, candidate.rebuilt, tc.wantProvider, tc.wantModel)
			}
		})
	}
}

func TestWriteNewTranscriptTracksPartialAppend(t *testing.T) {
	sess, err := core.NewSession(t.TempDir(), "cwd", "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	first := provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "first"}},
	}
	invalid := provider.Message{
		Role: provider.RoleAssistant,
		Content: []provider.Content{provider.ToolCallBlock{
			ID:        "call-1",
			Name:      "tool",
			Arguments: json.RawMessage("{"),
		}},
	}
	ag := core.NewAgent(nil, "model", "", nil)
	ag.SetMessages([]provider.Message{first, invalid})
	next, err := writeNewTranscriptLocked(ag, sess, 0)
	if err == nil {
		t.Fatal("writeNewTranscriptLocked succeeded with invalid message")
	}
	if next != 1 {
		t.Fatalf("next persisted message index = %d, want 1", next)
	}

	second := provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.ToolCallBlock{ID: "call-1", Name: "tool", Arguments: json.RawMessage(`{}`)}},
	}
	third := provider.Message{
		Role:    provider.RoleTool,
		Content: []provider.Content{provider.ToolResultBlock{CallID: "call-1", Content: []provider.Content{provider.TextBlock{Text: "done"}}}},
	}
	ag.SetMessages([]provider.Message{first, second, third})
	next, err = writeNewTranscriptLocked(ag, sess, next)
	if err != nil {
		t.Fatalf("retry transcript: %v", err)
	}
	if next != 3 {
		t.Fatalf("next persisted message index after retry = %d, want 3", next)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, messages, err := core.OpenSession(sess.Path)
	if err != nil {
		t.Fatalf("reopen session: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if len(messages) != 3 {
		t.Fatalf("replayed message count = %d, want 3", len(messages))
	}
	if repaired := provider.RepairOrphanedToolResults(messages); len(repaired) != len(messages) {
		t.Fatalf("replayed transcript lost tool pairing: %#v", messages)
	}
}

func TestSessionResumeClosesDiscardedAgentLSP(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("buildFailure=%v", fail), func(t *testing.T) {
			cwd := t.TempDir()
			listener := fakeLSPWorkspace(t, cwd)
			oldManager, candidateManager := lsp.NewManager(), lsp.NewManager()
			t.Cleanup(func() { _ = oldManager.Close(); _ = candidateManager.Close() })
			old := core.NewAgent(nil, "old", "", core.Registry{"lsp": &tools.LSPTool{Manager: oldManager}})
			next := core.NewAgent(nil, "new", "", core.Registry{"lsp": &tools.LSPTool{Manager: candidateManager}})
			oldConn := startFakeLSP(t, old.ToolsSnapshot(), cwd, listener)
			nextConn := startFakeLSP(t, next.ToolsSnapshot(), cwd, listener)
			path := syntheticSession(t, "stored", "new", provider.Usage{})
			sess, _, err := core.OpenSession(path)
			if err != nil {
				t.Fatal(err)
			}
			defer sess.Close()
			candidate, err := applySessionResume(sess, old, "current", "old", false, false, func(p, m string) (*core.Agent, string, string, error) {
				if fail {
					return next, p, m, errors.New("build failed")
				}
				return next, p, m, nil
			})
			if fail {
				if err == nil {
					t.Fatal("expected build failure")
				}
				waitFakeLSPExit(t, nextConn)
				if _, err := oldManager.Capabilities(context.Background(), cwd, "", "fake"); err != nil {
					t.Fatalf("live manager closed on failure: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer candidate.session.Close()
				waitFakeLSPExit(t, oldConn)
				if _, err := candidateManager.Capabilities(context.Background(), cwd, "", "fake"); err != nil {
					t.Fatalf("replacement manager closed: %v", err)
				}
			}
		})
	}
}
