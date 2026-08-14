package subagents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestResidentManagerSpawnsJournaledInProcessChild(t *testing.T) {
	run := make(chan string, 1)
	manager := NewResidentManager(t.TempDir(), func(ResidentChildSpec, *ResidentJournal) (ResidentTurnRunner, error) {
		return func(_ context.Context, prompt string) error { run <- prompt; return nil }, nil
	})
	defer manager.Close(context.Background())

	child, err := manager.Spawn(context.Background(), ResidentChildSpec{ID: "child-manager", SessionID: "child-session", Provider: "openai", Model: "gpt-5"}, "inspect this")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if child == nil {
		t.Fatal("Spawn returned nil child")
	}
	select {
	case got := <-run:
		if got != "inspect this" {
			t.Fatalf("runner prompt = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("resident runner did not execute accepted task")
	}
	deadline := time.Now().Add(time.Second)
	for {
		result, readErr := manager.Result("child-manager")
		if readErr == nil {
			if result.State != ResidentIdle || result.TurnID == "" {
				t.Fatalf("result = %#v", result)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("result projection was not written: %v", readErr)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestResidentManagerCompletionCarriesFinalSummary(t *testing.T) {
	completed := make(chan ResidentCompletion, 1)
	manager := NewResidentManager(t.TempDir(), func(_ ResidentChildSpec, journal *ResidentJournal) (ResidentTurnRunner, error) {
		return func(context.Context, string) error {
			return journal.RecordAgentEvent(core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "the actual child answer"}}}})
		}, nil
	})
	manager.SetCompletionObserver(func(completion ResidentCompletion) { completed <- completion })
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	if _, err := manager.Spawn(context.Background(), ResidentChildSpec{ID: "summary-child", SessionID: "child-session", Provider: "openai", Model: "gpt-5"}, "task"); err != nil {
		t.Fatal(err)
	}
	select {
	case completion := <-completed:
		if completion.Summary != "the actual child answer" {
			t.Fatalf("completion summary = %q", completion.Summary)
		}
	case <-time.After(time.Second):
		t.Fatal("resident child did not complete")
	}
}

func TestResidentManagerLiveReturnsChildSnapshot(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	manager := NewResidentManager(t.TempDir(), func(ResidentChildSpec, *ResidentJournal) (ResidentTurnRunner, error) {
		return func(context.Context, string) error {
			close(started)
			<-release
			return nil
		}, nil
	})
	defer manager.Close(context.Background())
	defer close(release)
	_, err := manager.Spawn(context.Background(), ResidentChildSpec{ID: "live-manager", SessionID: "child-session", Provider: "openai", Model: "gpt-5"}, "task")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("resident child did not start")
	}
	live, ok := manager.Live("live-manager")
	if !ok || live.TurnID == "" || live.State != ResidentRunning {
		t.Fatalf("Live = (%#v, %t)", live, ok)
	}
	if _, ok := manager.Live("missing"); ok {
		t.Fatal("Live returned missing child")
	}
}

func TestResidentManagerHistoryObserverSkipsTransientStreamEvents(t *testing.T) {
	started := make(chan struct{})
	publishedDelta := make(chan struct{})
	persist := make(chan struct{})
	release := make(chan struct{})
	historyUpdates := make(chan string, 1)
	manager := NewResidentManager(t.TempDir(), func(_ ResidentChildSpec, journal *ResidentJournal) (ResidentTurnRunner, error) {
		return func(context.Context, string) error {
			close(started)
			if err := journal.RecordAgentEvent(core.EvTextDelta{Delta: "streaming"}); err != nil {
				return err
			}
			close(publishedDelta)
			<-persist
			if err := journal.RecordAgentEvent(core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "finalized"}}}}); err != nil {
				return err
			}
			<-release
			return nil
		}, nil
	})
	manager.SetHistoryUpdateObserver(func(childID string) { historyUpdates <- childID })
	t.Cleanup(func() {
		close(release)
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if _, err := manager.Spawn(context.Background(), ResidentChildSpec{ID: "history-observer", SessionID: "child-session", Provider: "openai", Model: "gpt-5"}, "task"); err != nil {
		t.Fatal(err)
	}
	<-started
	<-publishedDelta
	select {
	case got := <-historyUpdates:
		t.Fatalf("transient stream event requested history reload for %q", got)
	default:
	}
	close(persist)
	select {
	case got := <-historyUpdates:
		if got != "history-observer" {
			t.Fatalf("history update child = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("finalized event did not request history reload")
	}
}

func TestResidentManagerActivityObserverTracksRunningChildrenWithoutPolling(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	activity := make(chan bool, 2)
	manager := NewResidentManager(t.TempDir(), func(ResidentChildSpec, *ResidentJournal) (ResidentTurnRunner, error) {
		return func(context.Context, string) error {
			close(started)
			<-release
			return nil
		}, nil
	})
	manager.SetActivityObserver(func(active bool) { activity <- active })
	t.Cleanup(func() {
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	select {
	case got := <-activity:
		if got {
			t.Fatal("new manager reported active resident")
		}
	default:
		t.Fatal("activity observer did not receive its initial state")
	}
	if _, err := manager.Spawn(context.Background(), ResidentChildSpec{ID: "activity-observer", SessionID: "child-session", Provider: "openai", Model: "gpt-5"}, "task"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-activity:
		if !got {
			t.Fatal("spawn marked resident activity inactive")
		}
	case <-time.After(time.Second):
		t.Fatal("spawn did not report resident activity")
	}
	<-started
	close(release)
	select {
	case got := <-activity:
		if got {
			t.Fatal("completed resident remained active")
		}
	case <-time.After(time.Second):
		t.Fatal("completion did not clear resident activity")
	}
}

func TestResidentManagerRecordsFactoryFailureAfterAcceptance(t *testing.T) {
	root := t.TempDir()
	manager := NewResidentManager(root, func(ResidentChildSpec, *ResidentJournal) (ResidentTurnRunner, error) {
		return nil, errors.New("synthetic factory failure")
	})
	spec := ResidentChildSpec{ID: "factory-failure", SessionID: "child-session", Provider: "openai", Model: "gpt-5"}
	if _, err := manager.Spawn(context.Background(), spec, "task"); err == nil {
		t.Fatal("Spawn succeeded")
	}
	metadata, err := ReadResidentMetadata(filepath.Join(root, spec.ID, residentMetadataName))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.State != ResidentFailed {
		t.Fatalf("state = %q, want failed", metadata.State)
	}
}

func TestResidentManagerReconcileIgnoresLegacyAgentsContainer(t *testing.T) {
	root := t.TempDir()
	legacyChild := filepath.Join(root, "agents", "old-child")
	if err := os.MkdirAll(legacyChild, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyChild, "events.jsonl"), []byte("legacy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewResidentManager(root, func(ResidentChildSpec, *ResidentJournal) (ResidentTurnRunner, error) {
		return nil, nil
	})
	if errs := manager.Reconcile(); len(errs) != 0 {
		t.Fatalf("Reconcile errors = %v", errs)
	}
	if snapshots := manager.Snapshot(); len(snapshots) != 0 {
		t.Fatalf("Reconcile recovered legacy child: %#v", snapshots)
	}
}

func TestResidentManagerResumeRebuildsStoppedChild(t *testing.T) {
	runs := make(chan string, 2)
	manager := NewResidentManager(t.TempDir(), func(ResidentChildSpec, *ResidentJournal) (ResidentTurnRunner, error) {
		return func(ctx context.Context, prompt string) error {
			runs <- prompt
			if prompt == "initial" {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		}, nil
	})
	defer manager.Close(context.Background())
	spec := ResidentChildSpec{ID: "stopped-child", SessionID: "child-session", Provider: "openai", Model: "gpt-5", Required: true}
	if _, err := manager.Spawn(context.Background(), spec, "initial"); err != nil {
		t.Fatal(err)
	}
	if got := <-runs; got != "initial" {
		t.Fatalf("initial prompt = %q", got)
	}
	if err := manager.Stop(context.Background(), spec.ID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Resume(context.Background(), spec.ID, "follow up"); err != nil {
		t.Fatalf("Resume stopped child: %v", err)
	}
	if got := <-runs; got != "follow up" {
		t.Fatalf("follow-up prompt = %q", got)
	}
	deadline := time.Now().Add(time.Second)
	for len(manager.UnmetRequired()) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("required child remained unmet: %#v", manager.UnmetRequired())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestResidentManagerPreparesWorkspaceBeforeAcceptance(t *testing.T) {
	root := t.TempDir()
	prepared := false
	manager := NewResidentManagerWithWorkspace(root, 0, func(_ context.Context, req WorkspaceRequest) (WorkspaceHandle, error) {
		prepared = true
		if req.AgentID != "workspace-child" || req.StateDir != filepath.Join(root, "workspace-child") {
			t.Fatalf("workspace request = %#v", req)
		}
		return SharedWorkspace{Root: req.RepositoryRoot}.Prepare(context.Background(), req)
	}, func(spec ResidentChildSpec, _ *ResidentJournal) (ResidentTurnRunner, error) {
		if !prepared || spec.Workspace != "/repo" {
			t.Fatalf("factory spec = %#v, prepared=%t", spec, prepared)
		}
		return func(context.Context, string) error { return nil }, nil
	})
	defer manager.Close(context.Background())
	if _, err := manager.Spawn(context.Background(), ResidentChildSpec{ID: "workspace-child", SessionID: "child-session", Provider: "openai", Model: "gpt-5", RepositoryRoot: "/repo"}, "task"); err != nil {
		t.Fatal(err)
	}
}

func TestResidentManagerRejectsWorkspaceOutsideAllowedRootsBeforeAcceptance(t *testing.T) {
	prepared := false
	manager := newResidentManager(t.TempDir(), SubagentPolicy{AllowedRoots: []string{t.TempDir()}}, func(context.Context, WorkspaceRequest) (WorkspaceHandle, error) {
		prepared = true
		return nil, nil
	}, func(ResidentChildSpec, *ResidentJournal) (ResidentTurnRunner, error) {
		return func(context.Context, string) error { return nil }, nil
	})
	outside := t.TempDir()
	if _, err := manager.Spawn(context.Background(), ResidentChildSpec{ID: "outside-root", SessionID: "child-session", Provider: "openai", Model: "gpt-5", RepositoryRoot: outside}, "task"); err == nil {
		t.Fatal("Spawn succeeded outside allowed roots")
	}
	if prepared {
		t.Fatal("workspace preparation ran outside allowed roots")
	}
}

func TestResidentManagerCapturesSuccessfulWorktreeBeforeCleanup(t *testing.T) {
	root := t.TempDir()
	workspace := &testResidentWorkspace{dir: "/isolated", mode: WorkspaceWorktree, capture: WorkspaceCapture{Patch: []byte("patch"), ChangedFiles: []string{"changed.go"}}}
	manager := NewResidentManagerWithWorkspace(root, 0, func(context.Context, WorkspaceRequest) (WorkspaceHandle, error) {
		return workspace, nil
	}, func(ResidentChildSpec, *ResidentJournal) (ResidentTurnRunner, error) {
		return func(context.Context, string) error { return nil }, nil
	})
	defer manager.Close(context.Background())
	if _, err := manager.Spawn(context.Background(), ResidentChildSpec{ID: "capture-child", SessionID: "child-session", Provider: "openai", Model: "gpt-5", RepositoryRoot: "/repo", WorkspaceMode: WorkspaceWorktree}, "task"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for manager.Snapshot()[0].State != ResidentIdle {
		if time.Now().After(deadline) {
			t.Fatal("turn did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	if workspace.cleaned {
		t.Fatal("idle child worktree was removed before explicit close")
	}
	result, err := manager.Result("capture-child")
	if err != nil {
		t.Fatal(err)
	}
	if result.PatchRef != PatchRef("capture-child") || len(result.ChangedFiles) != 1 || result.ChangedFiles[0] != "changed.go" {
		t.Fatalf("result = %#v", result)
	}
	patch, err := os.ReadFile(filepath.Join(root, "capture-child", residentPatchName))
	if err != nil || string(patch) != "patch" {
		t.Fatalf("patch = %q, err=%v", patch, err)
	}
	if err := manager.Stop(context.Background(), "capture-child"); err != nil {
		t.Fatal(err)
	}
	if !workspace.cleaned {
		t.Fatal("successful worktree was not cleaned on stop")
	}
}

func TestResidentManagerRetainsFailedWorktree(t *testing.T) {
	workspace := &testResidentWorkspace{dir: "/isolated", mode: WorkspaceWorktree}
	manager := NewResidentManagerWithWorkspace(t.TempDir(), 0, func(context.Context, WorkspaceRequest) (WorkspaceHandle, error) { return workspace, nil }, func(ResidentChildSpec, *ResidentJournal) (ResidentTurnRunner, error) {
		return func(context.Context, string) error { return errors.New("turn failed") }, nil
	})
	defer manager.Close(context.Background())
	if _, err := manager.Spawn(context.Background(), ResidentChildSpec{ID: "failed-worktree", SessionID: "child-session", Provider: "openai", Model: "gpt-5", RepositoryRoot: "/repo", WorkspaceMode: WorkspaceWorktree}, "task"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for manager.Snapshot()[0].State == ResidentRunning || manager.Snapshot()[0].State == ResidentQueued {
		if time.Now().After(deadline) {
			t.Fatal("turn did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	if workspace.captured || workspace.cleaned {
		t.Fatalf("failed worktree captured=%t cleaned=%t", workspace.captured, workspace.cleaned)
	}
}

func TestResidentManagerCapturesRealWorktreePatch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	root, repo := t.TempDir(), initTestRepo(t)
	manager := NewResidentManager(root, func(spec ResidentChildSpec, _ *ResidentJournal) (ResidentTurnRunner, error) {
		return func(context.Context, string) error {
			return os.WriteFile(filepath.Join(spec.Workspace, "README.md"), []byte("resident change\n"), 0o600)
		}, nil
	})
	defer manager.Close(context.Background())
	child, err := manager.Spawn(context.Background(), ResidentChildSpec{ID: "git-worktree", SessionID: "child-session", Provider: "openai", Model: "gpt-5", RepositoryRoot: repo, WorkspaceMode: WorkspaceWorktree, WorkspaceBase: "HEAD", WorkspaceCapture: CapturePatch}, "task")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for child.State() != ResidentIdle {
		if time.Now().After(deadline) {
			t.Fatal("worktree turn did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	result, err := manager.Result("git-worktree")
	if err != nil {
		t.Fatal(err)
	}
	if result.PatchRef != PatchRef("git-worktree") || len(result.ChangedFiles) != 1 || result.ChangedFiles[0] != "README.md" {
		t.Fatalf("result = %#v", result)
	}
	patch, err := os.ReadFile(filepath.Join(root, "git-worktree", residentPatchName))
	if err != nil || !strings.Contains(string(patch), "resident change") {
		t.Fatalf("patch = %q, err=%v", patch, err)
	}
	if err := manager.Stop(context.Background(), "git-worktree"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(child.workspace.Dir()); !os.IsNotExist(err) {
		t.Fatalf("worktree remains after stop: %v", err)
	}
}

type testResidentWorkspace struct {
	dir               string
	mode              WorkspaceMode
	capture           WorkspaceCapture
	captured, cleaned bool
}

func (w *testResidentWorkspace) Dir() string            { return w.dir }
func (w *testResidentWorkspace) RepositoryRoot() string { return "/repo" }
func (w *testResidentWorkspace) Mode() WorkspaceMode    { return w.mode }
func (w *testResidentWorkspace) Capture(context.Context) (WorkspaceCapture, error) {
	w.captured = true
	return w.capture, nil
}
func (w *testResidentWorkspace) Cleanup(context.Context) error { w.cleaned = true; return nil }

func TestResidentManagerRejectsDuplicateBeforeSecondJournalAcceptance(t *testing.T) {
	root := t.TempDir()
	manager := NewResidentManager(root, func(ResidentChildSpec, *ResidentJournal) (ResidentTurnRunner, error) {
		return func(context.Context, string) error { return nil }, nil
	})
	defer manager.Close(context.Background())
	spec := ResidentChildSpec{ID: "duplicate", SessionID: "child-session", Provider: "openai", Model: "gpt-5"}
	if _, err := manager.Spawn(context.Background(), spec, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Spawn(context.Background(), spec, "second"); err == nil {
		t.Fatal("duplicate Spawn succeeded")
	}
	records, err := ReadResidentJournal(filepath.Join(root, spec.ID, residentTranscriptName))
	if err != nil {
		t.Fatal(err)
	}
	accepted := 0
	for _, record := range records {
		if record.Type == residentRecordAccepted {
			accepted++
			if record.Prompt != "first" {
				t.Fatalf("accepted prompt = %q, want first", record.Prompt)
			}
		}
	}
	if accepted != 1 {
		t.Fatalf("records = %#v, want one accepted spawn", records)
	}
}

func TestResidentManagerReconcileMakesInterruptedJournalDiscoverableWithoutReplay(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "recovered")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "recovered", SessionID: "child-session", InitialTurnID: "turn-1", Provider: "openai", Model: "gpt-5", Required: true}
	if err := journal.Accept(spec, "do not replay"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnStarted(spec, spec.InitialTurnID); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	runs := 0
	manager := NewResidentManager(root, func(ResidentChildSpec, *ResidentJournal) (ResidentTurnRunner, error) {
		runs++
		return func(context.Context, string) error { return nil }, nil
	})
	if errs := manager.Reconcile(); len(errs) != 0 {
		t.Fatalf("Reconcile errors = %v", errs)
	}
	if runs != 0 {
		t.Fatalf("factory ran %d times during reconciliation", runs)
	}
	snapshots := manager.Snapshot()
	if len(snapshots) != 1 || snapshots[0].State != ResidentInterrupted || !snapshots[0].Required {
		t.Fatalf("snapshots = %#v", snapshots)
	}
	if _, err := manager.Spawn(context.Background(), spec, "overwrite"); err == nil {
		t.Fatal("Spawn overwrote recovered journal")
	}
}

func TestResidentManagerExplicitlyResumesRecoveredChildWithoutReplayingTask(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "resume-recovered")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "resume-recovered", SessionID: "child-session", InitialTurnID: "old-turn", Provider: "openai", Model: "gpt-5"}
	if err := journal.Accept(spec, "old task must not replay"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnStarted(spec, spec.InitialTurnID); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	runs := make(chan string, 1)
	manager := NewResidentManager(root, func(got ResidentChildSpec, _ *ResidentJournal) (ResidentTurnRunner, error) {
		if got.SessionID != spec.SessionID {
			t.Fatalf("session = %q, want %q", got.SessionID, spec.SessionID)
		}
		return func(_ context.Context, prompt string) error { runs <- prompt; return nil }, nil
	})
	defer manager.Close(context.Background())
	if errs := manager.Reconcile(); len(errs) != 0 {
		t.Fatalf("Reconcile = %v", errs)
	}
	if err := manager.Resume(context.Background(), spec.ID, "new explicit prompt"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-runs:
		if got != "new explicit prompt" {
			t.Fatalf("runner prompt = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("explicit resume did not run")
	}
}

func TestResidentManagerExplicitResumeReusesInterruptedWorktree(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "resume-worktree")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "resume-worktree", SessionID: "child-session", InitialTurnID: "old-turn", Provider: "openai", Model: "gpt-5", RepositoryRoot: "/repo", Workspace: "/prior-worktree", WorkspaceMode: WorkspaceWorktree}
	if err := journal.Accept(spec, "old task"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnStarted(spec, spec.InitialTurnID); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	workspace := &testResidentWorkspace{dir: "/prior-worktree", mode: WorkspaceWorktree}
	manager := NewResidentManagerWithWorkspace(root, 0, func(_ context.Context, req WorkspaceRequest) (WorkspaceHandle, error) {
		if req.ExistingPath != "/prior-worktree" {
			t.Fatalf("resume workspace request = %#v", req)
		}
		return workspace, nil
	}, func(ResidentChildSpec, *ResidentJournal) (ResidentTurnRunner, error) {
		return func(context.Context, string) error { return nil }, nil
	})
	defer manager.Close(context.Background())
	if errs := manager.Reconcile(); len(errs) != 0 {
		t.Fatal(errs)
	}
	if err := manager.Resume(context.Background(), spec.ID, "resume"); err != nil {
		t.Fatal(err)
	}
}

func TestResidentManagerAppliesPositiveQueueTimeoutDurably(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	manager := NewResidentManagerWithPolicy(t.TempDir(), SubagentPolicy{MaxConcurrent: 1, QueueTimeout: 20 * time.Millisecond}, func(spec ResidentChildSpec, _ *ResidentJournal) (ResidentTurnRunner, error) {
		return func(_ context.Context, _ string) error {
			started <- spec.ID
			if spec.ID == "first" {
				<-release
			}
			return nil
		}, nil
	})
	defer manager.Close(context.Background())
	for _, id := range []string{"first", "second"} {
		if _, err := manager.Spawn(context.Background(), ResidentChildSpec{ID: id, SessionID: id, Provider: "openai", Model: "gpt-5"}, id); err != nil {
			t.Fatalf("Spawn(%s): %v", id, err)
		}
	}
	select {
	case got := <-started:
		if got != "first" {
			t.Fatalf("first runner = %q, want first", got)
		}
	case <-time.After(time.Second):
		t.Fatal("first runner did not start")
	}
	deadline := time.Now().Add(time.Second)
	for {
		result, err := manager.Result("second")
		if err == nil && result.State == ResidentFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued child did not record timeout: result=%#v err=%v", result, err)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case got := <-started:
		t.Fatalf("timed out child ran as %q", got)
	default:
	}
	close(release)
}

func TestResidentManagerNotifiesAcceptedObserverForFollowUps(t *testing.T) {
	manager := NewResidentManager(t.TempDir(), func(ResidentChildSpec, *ResidentJournal) (ResidentTurnRunner, error) {
		return func(context.Context, string) error { return nil }, nil
	})
	defer manager.Close(context.Background())
	accepted := make(chan string, 2)
	manager.SetAcceptedObserver(func(_ ResidentChildSpec, prompt string) { accepted <- prompt })
	spec := ResidentChildSpec{ID: "observer", SessionID: "session", Provider: "openai", Model: "gpt-5"}
	if _, err := manager.Spawn(context.Background(), spec, "initial"); err != nil {
		t.Fatal(err)
	}
	if got := <-accepted; got != "initial" {
		t.Fatalf("initial observer prompt = %q", got)
	}
	if err := manager.Resume(context.Background(), spec.ID, "follow up"); err != nil {
		t.Fatal(err)
	}
	if got := <-accepted; got != "follow up" {
		t.Fatalf("follow-up observer prompt = %q", got)
	}
}

func TestResidentManagerReconcileReplacesStaleRecoveredSnapshots(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "stale")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "stale", SessionID: "child-session", Provider: "openai", Model: "gpt-5"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	manager := NewResidentManager(root, func(ResidentChildSpec, *ResidentJournal) (ResidentTurnRunner, error) { return nil, nil })
	if errs := manager.Reconcile(); len(errs) != 0 || len(manager.Snapshot()) != 1 {
		t.Fatalf("first Reconcile = %v, snapshots=%#v", errs, manager.Snapshot())
	}
	if err := os.RemoveAll(filepath.Join(root, "stale")); err != nil {
		t.Fatal(err)
	}
	if errs := manager.Reconcile(); len(errs) != 0 || len(manager.Snapshot()) != 0 {
		t.Fatalf("second Reconcile = %v, snapshots=%#v", errs, manager.Snapshot())
	}
}

func TestResidentManagerLimitsConcurrentTurnsGlobally(t *testing.T) {
	started := make(chan string, DefaultResidentConcurrency+1)
	release := make(chan struct{})
	manager := NewResidentManager(t.TempDir(), func(ResidentChildSpec, *ResidentJournal) (ResidentTurnRunner, error) {
		return func(ctx context.Context, prompt string) error {
			started <- prompt
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}, nil
	})
	defer manager.Close(context.Background())

	for i := 0; i < DefaultResidentConcurrency+1; i++ {
		spec := ResidentChildSpec{ID: fmt.Sprintf("child-%d", i), SessionID: fmt.Sprintf("session-%d", i), Provider: "openai", Model: "gpt-5"}
		if _, err := manager.Spawn(context.Background(), spec, spec.ID); err != nil {
			t.Fatalf("Spawn(%d): %v", i, err)
		}
	}
	for i := 0; i < DefaultResidentConcurrency; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("did not fill the default six slots")
		}
	}
	select {
	case extra := <-started:
		t.Fatalf("seventh turn started before a slot released: %q", extra)
	default:
	}
	close(release)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("queued seventh turn did not start after a slot released")
	}
}
