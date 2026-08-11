package subagents

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWriteAgentMetaSerializesConcurrentSnapshots(t *testing.T) {
	stateDir := t.TempDir()
	a := &Agent{ID: "concurrent-meta", Task: "retain queued prompts", stateDir: stateDir}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a.setTurnCounts(i, i/2)
			a.queueResumePrompt("prompt", time.Now())
			if err := writeAgentMeta(stateDir, a); err != nil {
				t.Errorf("write metadata: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if _, err := readAgentMeta(stateDir); err != nil {
		t.Fatalf("final metadata is not a complete JSON snapshot: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(stateDir, ".meta.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary metadata files remain: %v", matches)
	}
}

// TestSpawnWritesMetaJSON asserts the durability contract: every
// successful Spawn leaves a meta.json on disk with the agent's
// identity bits. Without this, Reload on the next zut launch can't
// find the agent and the user loses access to the worktree.
func TestSpawnWritesMetaJSON(t *testing.T) {
	root := t.TempDir()
	f := New(Config{
		Root:            root,
		RepoRoot:        root,
		WebSearchPolicy: WebSearchAllow,
		NewRunner: func(a *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, _ Sink) error {
				<-ctx.Done()
				return ctx.Err()
			})
		},
	})
	defer f.StopAll()

	a, err := f.Spawn(context.Background(), "investigate widget")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	// Read meta.json straight off disk.
	metaBytes, err := os.ReadFile(filepath.Join(root, "agents", a.ID, "meta.json"))
	if err != nil {
		t.Fatalf("read meta.json: %v", err)
	}
	var got agentMeta
	if err := json.Unmarshal(metaBytes, &got); err != nil {
		t.Fatalf("parse meta.json: %v", err)
	}
	if got.ID != a.ID {
		t.Errorf("meta.ID = %q; want %q", got.ID, a.ID)
	}
	if got.Task != "investigate widget" {
		t.Errorf("meta.Task = %q", got.Task)
	}
	if got.Dir != a.Dir {
		t.Errorf("meta paths drifted: %+v vs agent %+v", got, a)
	}
	if got.InboxPath == "" || got.EventLogPath == "" || got.SessionPath == "" {
		t.Errorf("meta paths empty: %+v", got)
	}
	if got.Started.IsZero() {
		t.Error("meta.Started is zero")
	}
	if got.WebSearchPolicy != WebSearchAllow || a.WebSearchPolicy != WebSearchAllow {
		t.Fatalf("web search policy persisted as meta=%v agent=%v, want allow", got.WebSearchPolicy, a.WebSearchPolicy)
	}
}

// TestReloadRebuildsDetachedAgents simulates a zut restart by spawning
// in one Supervisor, throwing it away, then opening a fresh Supervisor against
// the same root and calling Reload. The user-visible state — id,
// task, branch, dir — must come back, and status must be Detached so
// the dashboard can offer resume.
func TestReloadRebuildsDetachedAgents(t *testing.T) {
	root := t.TempDir()

	// First incarnation: spawn two agents, then drop the supervisor.
	first := New(Config{
		Root:     root,
		RepoRoot: root,
		NewRunner: func(a *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, _ Sink) error {
				<-ctx.Done()
				return ctx.Err()
			})
		},
	})
	a1, err := first.Spawn(context.Background(), "alpha task")
	if err != nil {
		t.Fatalf("spawn a1: %v", err)
	}
	a2, err := first.Spawn(context.Background(), "beta task")
	if err != nil {
		t.Fatalf("spawn a2: %v", err)
	}
	first.StopAll()
	// Wait briefly for the runner goroutines to settle so their
	// stop event reaches the log before we move on. Reload itself
	// doesn't need this; it makes the assertions below deterministic.
	a1.Wait()
	a2.Wait()

	// Second incarnation against the same root.
	second := New(Config{
		Root:     root,
		RepoRoot: root,
	})
	loaded, errs := second.Reload()
	if len(errs) > 0 {
		t.Fatalf("reload errs: %v", errs)
	}
	if loaded != 2 {
		t.Fatalf("loaded = %d; want 2", loaded)
	}
	snap := second.SnapshotAll()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d; want 2", len(snap))
	}
	ids := map[string]AgentSnapshot{}
	for _, s := range snap {
		ids[s.ID] = s
	}
	if _, ok := ids[a1.ID]; !ok {
		t.Errorf("reloaded set missing %s", a1.ID)
	}
	if _, ok := ids[a2.ID]; !ok {
		t.Errorf("reloaded set missing %s", a2.ID)
	}
	for _, s := range snap {
		if s.Status != StatusDetached {
			t.Errorf("agent %s status = %q; want detached", s.ID, s.Status)
		}
		if s.Task == "" {
			t.Errorf("agent %s lost its task", s.ID)
		}
	}
}

func TestResumePreservesPerAgentTimeout(t *testing.T) {
	root := t.TempDir()
	wantTimeout := 37 * time.Minute
	first := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(a *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, _ Sink) error {
				<-ctx.Done()
				return ctx.Err()
			})
		},
	})
	a, err := first.SpawnReq(context.Background(), SpawnRequest{Task: "timeout test", Timeout: wantTimeout})
	if err != nil {
		t.Fatal(err)
	}
	first.StopAll()
	a.Wait()

	observedCh := make(chan time.Duration, 1)
	second := New(Config{
		Root: root, RepoRoot: root,
		Policy: SubagentPolicy{DefaultTimeout: time.Hour},
		NewRunner: func(a *Agent) Runner {
			observedCh <- a.Timeout
			return RunnerFunc(func(ctx context.Context, _ Sink) error {
				<-ctx.Done()
				return ctx.Err()
			})
		},
	})
	if loaded, errs := second.Reload(); loaded != 1 || len(errs) != 0 {
		t.Fatalf("reload loaded=%d errs=%v", loaded, errs)
	}
	resumed, err := second.ResumeSession(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	var observed time.Duration
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	select {
	case observed = <-observedCh:
	case <-deadline.C:
		t.Fatal("timed out waiting for resumed runner to observe timeout")
	}
	if resumed.Timeout != wantTimeout || observed != wantTimeout {
		t.Fatalf("resumed timeout = %s, runner observed %s; want %s", resumed.Timeout, observed, wantTimeout)
	}
	second.StopAll()
	resumed.Wait()
}

func TestReloadRejectsMetadataPathsOutsideState(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "agents", "agent-1")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := agentMeta{
		ID: "agent-1", Task: "malicious", Dir: root,
		EventLogPath: outside, SessionPath: outside, InboxPath: outside,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "meta.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	f := New(Config{Root: root, RepoRoot: root})
	if loaded, errs := f.Reload(); loaded != 0 || len(errs) == 0 {
		t.Fatalf("malicious metadata reload loaded=%d errs=%v", loaded, errs)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "keep" {
		t.Fatalf("outside path was touched: data=%q err=%v", got, err)
	}
}

// TestReloadIsIdempotent calls Reload twice in a row and asserts the
// second call neither duplicates rows nor errors.
func TestReloadIsIdempotent(t *testing.T) {
	root := t.TempDir()
	first := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(a *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, _ Sink) error { <-ctx.Done(); return ctx.Err() })
		},
	})
	if _, err := first.Spawn(context.Background(), "only one"); err != nil {
		t.Fatal(err)
	}
	first.StopAll()

	second := New(Config{Root: root, RepoRoot: root})
	loaded1, _ := second.Reload()
	loaded2, errs := second.Reload()
	if len(errs) > 0 {
		t.Fatalf("errs on second reload: %v", errs)
	}
	if loaded1 != 1 || loaded2 != 0 {
		t.Fatalf("loaded counts = (%d, %d); want (1, 0)", loaded1, loaded2)
	}
	if got := len(second.SnapshotAll()); got != 1 {
		t.Fatalf("snapshot len = %d; want 1", got)
	}
}

// TestReloadReplaysTranscriptFromEventLog drops a curated events.jsonl
// next to a meta.json and checks that the transcript surfaces in the
// reloaded agent. This is the user-facing payoff of Reload: opening
// /subagents logs <id> after a restart shows what the agent said before.
func TestReloadReplaysTranscriptFromEventLog(t *testing.T) {
	root := t.TempDir()
	id := "alpha-9"
	stateDir := filepath.Join(root, "agents", id)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// meta.json
	m := agentMeta{
		ID: id, Task: "do thing",
		Dir: root, Started: time.Now().Add(-time.Hour),
		InboxPath:    filepath.Join(stateDir, "in.sock"),
		EventLogPath: filepath.Join(stateDir, "events.jsonl"),
		SessionPath:  filepath.Join(stateDir, "session.json"),
	}
	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(stateDir, "meta.json"), mb, 0o644); err != nil {
		t.Fatal(err)
	}
	// Event log with one assistant turn and a graceful stop.
	log, err := OpenEventLog(m.EventLogPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = log.Append(NewEvent("assistant_message", map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "hello from the past"}},
	}))
	_ = log.Append(NewEvent("agent_stopped", map[string]any{"reason": "shutdown"}))
	_ = log.Close()

	f := New(Config{Root: root, RepoRoot: root})
	loaded, errs := f.Reload()
	if len(errs) > 0 || loaded != 1 {
		t.Fatalf("reload loaded=%d errs=%v", loaded, errs)
	}
	a := f.Get(id)
	if a == nil {
		t.Fatal("reloaded agent not found")
	}
	// Legacy metadata has no persisted dashboard status, so startup keeps the
	// agent detached until the user asks to inspect its durable history.
	if a.Status() != StatusDetached {
		t.Errorf("startup status = %q; want detached", a.Status())
	}
	got := a.Transcript()
	if a.Status() != StatusDone {
		t.Errorf("hydrated status = %q; want done (offline)", a.Status())
	}
	found := false
	for _, line := range got {
		if line == "hello from the past" {
			found = true
		}
	}
	if !found {
		t.Errorf("transcript missing replayed line: %v", got)
	}
}

// TestReloadUsesPersistedSummaryBeforeTranscriptHydration ensures startup
// rebuilds the dashboard from compact metadata rather than replaying the
// event log. Opening the transcript remains the explicit point that loads
// durable history.
func TestReloadUsesPersistedSummaryBeforeTranscriptHydration(t *testing.T) {
	root := t.TempDir()
	id := "summary-1"
	stateDir := filepath.Join(root, "agents", id)
	agent := &Agent{
		ID:            id,
		Task:          "persist summary",
		OriginalTask:  "persist summary",
		Dir:           root,
		Started:       time.Now().Add(-time.Hour),
		EventLogPath:  filepath.Join(stateDir, "events.jsonl"),
		SessionPath:   filepath.Join(stateDir, "session.json"),
		status:        StatusDone,
		activity:      "done",
		finished:      time.Now().Add(-time.Minute),
		processState:  ProcessExited,
		turnState:     TurnSucceeded,
		currentTurnID: "turn-1",
		done:          make(chan struct{}),
	}
	if err := writeAgentMeta(stateDir, agent); err != nil {
		t.Fatal(err)
	}

	log, err := OpenEventLog(agent.EventLogPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(NewEvent("assistant_message", map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "historical reply"}},
	})); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(NewEvent("agent_stopped", map[string]any{"reason": "cancelled"})); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	supervisor := New(Config{Root: root, RepoRoot: root})
	if loaded, errs := supervisor.Reload(); loaded != 1 || len(errs) != 0 {
		t.Fatalf("reload loaded=%d errs=%v", loaded, errs)
	}
	reloaded := supervisor.Get(id)
	if reloaded == nil {
		t.Fatal("reloaded agent missing")
	}
	before := reloaded.Snapshot()
	if before.Status != StatusDetached || before.Activity != "done" || before.ProcessState != ProcessExited || before.TurnState != TurnSucceeded {
		t.Fatalf("startup summary = (%q, %q, %q, %q), want (detached, done, exited, succeeded)", before.Status, before.Activity, before.ProcessState, before.TurnState)
	}
	if len(before.Lines) != 0 {
		t.Fatalf("startup loaded transcript lines: %v", before.Lines)
	}
	lastActivity := before.LastActivity

	lines := reloaded.Transcript()
	if len(lines) != 1 || lines[0] != "historical reply" {
		t.Fatalf("hydrated transcript = %v", lines)
	}
	after := reloaded.Snapshot()
	if after.Status != StatusDetached || after.Activity != "done" {
		t.Fatalf("hydration overwrote persisted summary: (%q, %q)", after.Status, after.Activity)
	}
	if !after.LastActivity.Equal(lastActivity) {
		t.Fatalf("hydration changed last activity from %s to %s", lastActivity, after.LastActivity)
	}
}

// TestReloadSkipsBareDirsAndCorruptMeta ensures one bad meta.json
// doesn't blow up the whole reload. Directories with no meta.json
// at all are silently ignored (Spawn that failed mid-way leaves
// these); corrupt meta files are reported as errors but don't stop
// the rest of the load.
func TestReloadSkipsBareDirsAndCorruptMeta(t *testing.T) {
	root := t.TempDir()
	agentsDir := filepath.Join(root, "agents")
	// Bare directory, no meta.json.
	if err := os.MkdirAll(filepath.Join(agentsDir, "leftover"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Directory with garbage meta.json.
	if err := os.MkdirAll(filepath.Join(agentsDir, "corrupt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "corrupt", "meta.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// One good agent.
	good := "good-1"
	stateDir := filepath.Join(agentsDir, good)
	_ = os.MkdirAll(stateDir, 0o755)
	m := agentMeta{ID: good, Task: "x", Dir: "/tmp/x", Started: time.Now()}
	mb, _ := json.MarshalIndent(m, "", "  ")
	_ = os.WriteFile(filepath.Join(stateDir, "meta.json"), mb, 0o644)

	f := New(Config{Root: root, RepoRoot: root})
	loaded, errs := f.Reload()
	if loaded != 1 {
		t.Errorf("loaded = %d; want 1", loaded)
	}
	if len(errs) != 1 {
		t.Errorf("errs len = %d; want 1 (the corrupt entry): %v", len(errs), errs)
	}
}

// TestResumeRestartsRunnerOnSameSession is the headline test. We:
//
//  1. Spawn an agent with a counting runner (records each Run call).
//  2. Stop it.
//  3. Resume it.
//  4. Assert Run was called twice — once for spawn, once for resume —
//     against the same SessionPath / InboxPath / Dir.
func TestResumeRestartsRunnerOnSameSession(t *testing.T) {
	root := t.TempDir()
	var (
		mu       sync.Mutex
		runs     []*Agent
		runsDone int32
	)
	f := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(a *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, sink Sink) error {
				mu.Lock()
				runs = append(runs, a)
				mu.Unlock()
				sink.Activity("ran")
				atomic.AddInt32(&runsDone, 1)
				<-ctx.Done()
				return ctx.Err()
			})
		},
	})
	defer f.StopAll()

	a, err := f.Spawn(context.Background(), "do thing")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	originalSession := a.SessionPath
	originalDir := a.Dir
	originalInbox := a.InboxPath

	// Wait for the runner to record its presence, then stop the agent.
	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt32(&runsDone) < 1 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if err := f.Stop(a.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	a.Wait()
	if a.Status() != StatusKilled {
		t.Fatalf("post-stop status = %q; want killed", a.Status())
	}
	previousAttempt := a.Snapshot().Attempt

	// Now resume.
	a2, err := f.Resume(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if a2.ID != a.ID {
		t.Errorf("resume returned different id: %s vs %s", a2.ID, a.ID)
	}
	if a2.SessionPath != originalSession {
		t.Errorf("resume changed session path: %s vs %s", a2.SessionPath, originalSession)
	}
	if a2.Dir != originalDir {
		t.Errorf("resume changed dir: %s vs %s", a2.Dir, originalDir)
	}
	if a2.InboxPath != originalInbox {
		t.Errorf("resume changed inbox path: %s vs %s", a2.InboxPath, originalInbox)
	}
	if got := a2.Snapshot().Attempt; got != previousAttempt+1 {
		t.Errorf("resume attempt = %d; want %d", got, previousAttempt+1)
	}
	// Two runner invocations: spawn + resume.
	deadline = time.Now().Add(time.Second)
	for atomic.LoadInt32(&runsDone) < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(runs) != 2 {
		t.Fatalf("runner invocations = %d; want 2", len(runs))
	}
	if runs[1].ID != a.ID {
		t.Errorf("second run had wrong id: %s", runs[1].ID)
	}
	if got := len(f.SnapshotAll()); got != 1 {
		t.Errorf("snapshot len = %d; want 1 (in-place replace)", got)
	}
}

func TestRestartTaskRetainsQueuedResumePrompt(t *testing.T) {
	root := t.TempDir()
	started := make(chan *Agent, 2)
	f := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(a *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, _ Sink) error {
				started <- a
				<-ctx.Done()
				return ctx.Err()
			})
		},
	})
	t.Cleanup(f.StopAll)

	existing, err := f.Spawn(context.Background(), "review the implementation")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-started:
		if got != existing {
			t.Fatalf("initial runner agent = %p, want %p", got, existing)
		}
	case <-time.After(time.Second):
		t.Fatal("initial runner did not start")
	}

	const followUp = "I applied your review. What do you think now?"
	acceptedAt := time.Now().Add(-time.Minute)
	existing.setResumePrompt(followUp, acceptedAt)
	f.persistAgent(existing)
	if err := f.Stop(existing.ID); err != nil {
		t.Fatal(err)
	}
	existing.Wait()

	restarted, err := f.RestartTask(context.Background(), existing.ID)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-started:
		if got != restarted {
			t.Fatalf("restarted runner agent = %p, want %p", got, restarted)
		}
		resumePrompt, resumePromptAt := got.ResumePromptInfo()
		if got.Resuming || resumePrompt != followUp || !resumePromptAt.Equal(acceptedAt) {
			t.Fatalf("restarted lifecycle = resuming %t prompt %q accepted %s, want false, prompt %q, and accepted %s", got.Resuming, resumePrompt, resumePromptAt, followUp, acceptedAt)
		}
	case <-time.After(time.Second):
		t.Fatal("restarted runner did not start")
	}

	persisted, err := readAgentMeta(filepath.Join(root, "agents", existing.ID))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ResumePrompt != followUp || !persisted.ResumePromptAt.Equal(acceptedAt) {
		t.Fatalf("persisted restart prompt = %q accepted %s, want prompt %q and accepted %s", persisted.ResumePrompt, persisted.ResumePromptAt, followUp, acceptedAt)
	}
}

func TestResumeRecalculatesInvalidStateInboxPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subagent inbox runtime relocation applies to Unix sockets")
	}
	root := t.TempDir()
	f := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(*Agent) Runner {
			return RunnerFunc(func(context.Context, Sink) error { return nil })
		},
	})
	a, err := f.Spawn(context.Background(), "invalid inbox")
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()

	invalidPath := filepath.Join(root, "agents", a.ID, "in.sock")
	a.InboxPath = invalidPath
	a2, err := f.Resume(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	a2.Wait()
	if a2.InboxPath == invalidPath {
		t.Fatalf("resume kept state-directory inbox path %q", invalidPath)
	}
	if strings.HasPrefix(a2.InboxPath, root+string(filepath.Separator)) {
		t.Fatalf("resumed inbox %q remains below state root %q", a2.InboxPath, root)
	}
}

// TestResumeSetsResumingFlag pins the contract execRunner relies on:
// after Resume, the new Agent has Resuming == true; after a fresh
// Spawn it's false. The runner branches on this to decide whether
// to pass the original Task as a positional argv to the child, so
// getting it wrong silently re-runs the initial task on every
// resume and surfaces "agent busy; send 'cancel' first" between
// assistant messages.
func TestResumeSetsResumingFlag(t *testing.T) {
	root := t.TempDir()
	f := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(a *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, _ Sink) error { <-ctx.Done(); return ctx.Err() })
		},
	})
	defer f.StopAll()

	a, err := f.Spawn(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if a.Resuming {
		t.Fatal("fresh Spawn produced Resuming==true; want false")
	}
	if err := f.Stop(a.ID); err != nil {
		t.Fatal(err)
	}
	a.Wait()

	a2, err := f.Resume(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !a2.Resuming {
		t.Error("Resume produced Resuming==false; want true so the runner skips the duplicate initial-task argv")
	}
}

func TestReloadClearsResumePromptAcknowledgedInEventLog(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "agents", "acknowledged-follow-up")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	m := agentMeta{
		ID:             "acknowledged-follow-up",
		Task:           "review the implementation",
		Dir:            root,
		Started:        time.Now().Add(-time.Minute),
		Status:         StatusPending,
		InboxPath:      filepath.Join(stateDir, "in.sock"),
		EventLogPath:   filepath.Join(stateDir, "events.jsonl"),
		SessionPath:    filepath.Join(stateDir, "session.json"),
		ResumePrompt:   "I applied your review. What do you think now?",
		ResumePromptAt: time.Now().Add(-time.Second),
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath(stateDir), data, 0o600); err != nil {
		t.Fatal(err)
	}
	log, err := OpenEventLog(m.EventLogPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(NewEvent("user_message", map[string]any{"content": []any{map[string]any{"type": "text", "text": m.ResumePrompt}}})); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(NewEvent(EventTurnStarted, map[string]any{"step": float64(1)})); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	started := make(chan *Agent, 1)
	f := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(a *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, _ Sink) error {
				started <- a
				<-ctx.Done()
				return ctx.Err()
			})
		},
	})
	t.Cleanup(f.StopAll)
	if loaded, errs := f.Reload(); loaded != 1 || len(errs) != 0 {
		t.Fatalf("reload = (%d, %v), want (1, no errors)", loaded, errs)
	}
	resumed, err := f.ResumeSession(context.Background(), m.ID)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-started:
		if got != resumed || got.resumePrompt() != "" {
			t.Fatalf("acknowledged follow-up was replayed as %q", got.resumePrompt())
		}
	case <-time.After(time.Second):
		t.Fatal("resumed runner did not start")
	}
}

func TestReloadRetainsQueuedResumePromptUntilTurnStarts(t *testing.T) {
	root := t.TempDir()
	const (
		id       = "queued-follow-up"
		original = "review the implementation"
		followUp = "I applied your review. What do you think now?"
	)
	stateDir := filepath.Join(root, "agents", id)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	m := agentMeta{
		ID:             id,
		Task:           original,
		OriginalTask:   original,
		Dir:            root,
		Started:        time.Now().Add(-time.Minute),
		Status:         StatusPending,
		ProcessState:   ProcessPending,
		TurnState:      TurnQueued,
		InboxPath:      filepath.Join(stateDir, "in.sock"),
		EventLogPath:   filepath.Join(stateDir, "events.jsonl"),
		SessionPath:    filepath.Join(stateDir, "session.json"),
		ResumePrompt:   followUp,
		ResumePromptAt: time.Now(),
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath(stateDir), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.EventLogPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	started := make(chan *Agent, 1)
	f := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(a *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, _ Sink) error {
				started <- a
				<-ctx.Done()
				return ctx.Err()
			})
		},
	})
	t.Cleanup(f.StopAll)
	if loaded, errs := f.Reload(); loaded != 1 || len(errs) != 0 {
		t.Fatalf("reload = (%d, %v), want (1, no errors)", loaded, errs)
	}
	loaded := f.Get(id)
	if loaded == nil {
		t.Fatal("reloaded agent missing")
	}
	if got := loaded.resumePrompt(); got != followUp {
		t.Fatalf("reloaded pending prompt = %q, want %q", got, followUp)
	}

	resumed, err := f.ResumeSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-started:
		if got != resumed || got.resumePrompt() != followUp {
			t.Fatalf("resumed runner = %p prompt %q, want %p prompt %q", got, got.resumePrompt(), resumed, followUp)
		}
	case <-time.After(time.Second):
		t.Fatal("resumed runner did not start")
	}

	updateAgentFromEvent(resumed, NewEvent(EventTurnStarted, map[string]any{"step": float64(1)}))
	persisted, err := readAgentMeta(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.resumePrompt() != "" || persisted.ResumePrompt != "" {
		t.Fatalf("pending prompt not cleared after turn start: agent %q meta %q", resumed.resumePrompt(), persisted.ResumePrompt)
	}
}

func TestResumeWithPromptRetainsSessionAndStartsFollowUp(t *testing.T) {
	root := t.TempDir()
	started := make(chan *Agent, 2)
	f := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(a *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, _ Sink) error {
				started <- a
				<-ctx.Done()
				return ctx.Err()
			})
		},
	})
	defer f.StopAll()

	first, err := f.Spawn(context.Background(), "review the patch")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-started:
		if got != first {
			t.Fatalf("initial runner agent = %p, want %p", got, first)
		}
	case <-time.After(time.Second):
		t.Fatal("initial runner did not start")
	}
	if err := f.Stop(first.ID); err != nil {
		t.Fatal(err)
	}
	first.Wait()

	const followUp = "I applied your feedback. Please review it again."
	resumed, err := f.ResumeWithPrompt(context.Background(), first.ID, followUp)
	if err != nil {
		t.Fatalf("resume with prompt: %v", err)
	}
	if resumed.ID != first.ID || resumed.SessionPath != first.SessionPath {
		t.Fatalf("resumed agent = id %q session %q, want id %q session %q", resumed.ID, resumed.SessionPath, first.ID, first.SessionPath)
	}
	resumePrompt, resumePromptAt := resumed.ResumePromptInfo()
	if !resumed.Resuming || resumePrompt != followUp || resumePromptAt.IsZero() {
		t.Fatalf("resumed lifecycle = resuming %t prompt %q accepted %s, want true, prompt %q, and timestamp", resumed.Resuming, resumePrompt, resumePromptAt, followUp)
	}
	persisted, err := readAgentMeta(filepath.Join(root, "agents", first.ID))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ResumePrompt != followUp || persisted.ResumePromptAt.IsZero() {
		t.Fatalf("persisted follow-up = prompt %q accepted %s, want prompt %q and timestamp", persisted.ResumePrompt, persisted.ResumePromptAt, followUp)
	}
	select {
	case got := <-started:
		if got != resumed {
			t.Fatalf("follow-up runner agent = %p, want %p", got, resumed)
		}
		resumePrompt, _ := got.ResumePromptInfo()
		if resumePrompt != followUp {
			t.Fatalf("runner follow-up = %q, want %q", resumePrompt, followUp)
		}
	case <-time.After(time.Second):
		t.Fatal("resumed runner did not start")
	}
}

// TestResumeRejectsRunningAgent prevents the user from double-running
// an agent: two runners on the same session.json would race.
func TestResumeRejectsRunningAgent(t *testing.T) {
	root := t.TempDir()
	f := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(a *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, _ Sink) error { <-ctx.Done(); return ctx.Err() })
		},
	})
	defer f.StopAll()
	a, err := f.Spawn(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	// Wait for the runner to actually start.
	deadline := time.Now().Add(time.Second)
	for a.Status() != StatusRunning && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if _, err := f.Resume(context.Background(), a.ID); err == nil {
		t.Fatal("resume on running agent did not error")
	}
}

// TestResumeAfterReload exercises the full lifecycle the user cares
// about: spawn in process A, exit A, start process B, Reload, Resume.
// The resumed agent must keep its id and start producing new output
// against the same on-disk state.
//
// Note on transcript persistence: in production the runner is
// execRunner, which writes every event to events.jsonl, and Reload's
// replayEventsIntoAgent rebuilds the transcript from there. The fake
// RunnerFunc in this test calls sink.Transcript directly (which only
// touches in-memory state), so the "first run" line is *expected* to
// be lost across the restart — the in-memory Agent went away. We
// have TestReloadReplaysTranscriptFromEventLog covering the
// log-replay path with a curated events.jsonl.
func TestResumeAfterReload(t *testing.T) {
	root := t.TempDir()
	// Process A
	a := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(ag *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, sink Sink) error {
				sink.Transcript("first run for " + ag.ID)
				<-ctx.Done()
				return ctx.Err()
			})
		},
	})
	ag, err := a.Spawn(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	// Let the runner record its transcript line before we tear it down.
	deadline := time.Now().Add(time.Second)
	for len(ag.Transcript()) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	id := ag.ID
	a.StopAll()
	ag.Wait()

	// Process B: a new Supervisor against the same root.
	resumed := make(chan struct{}, 1)
	b := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(ag *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, sink Sink) error {
				sink.Transcript("second run for " + ag.ID)
				select {
				case resumed <- struct{}{}:
				default:
				}
				<-ctx.Done()
				return ctx.Err()
			})
		},
	})
	if loaded, errs := b.Reload(); loaded != 1 || len(errs) > 0 {
		t.Fatalf("reload loaded=%d errs=%v", loaded, errs)
	}
	a2, err := b.Resume(context.Background(), id)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	defer b.StopAll()

	select {
	case <-resumed:
	case <-time.After(time.Second):
		t.Fatal("resume runner did not start")
	}
	if a2.ID != id {
		t.Errorf("resume id = %s; want %s", a2.ID, id)
	}

	// Wait for the new transcript line to land.
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, l := range a2.Transcript() {
			if l == "second run for "+id {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("resumed transcript missing fresh line: %v", a2.Transcript())
}

func TestStopMustFinishBeforeResumeOrRemove(t *testing.T) {
	root := t.TempDir()
	started := make(chan struct{})
	release := make(chan struct{})
	f := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(*Agent) Runner {
			return RunnerFunc(func(context.Context, Sink) error {
				close(started)
				<-release
				return nil
			})
		},
	})
	a, err := f.Spawn(context.Background(), "slow stop")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := f.Stop(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Resume(context.Background(), a.ID); err == nil || !strings.Contains(err.Error(), "finalizing") {
		t.Fatalf("immediate resume error = %v, want finalizing", err)
	}
	if err := f.Remove(a.ID); err == nil || !strings.Contains(err.Error(), "finalizing") {
		t.Fatalf("immediate remove error = %v, want finalizing", err)
	}
	close(release)
	a.Wait()
}

func TestResumeFencesLiveReloadedWorker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subagent inbox transport uses Unix-domain sockets")
	}
	root := t.TempDir()
	first := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(*Agent) Runner {
			return RunnerFunc(func(context.Context, Sink) error { return nil })
		},
	})
	a, err := first.Spawn(context.Background(), "live worker")
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	ln, err := Listen(a.InboxPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	second := New(Config{Root: root, RepoRoot: root})
	if loaded, errs := second.Reload(); loaded != 1 || len(errs) != 0 {
		t.Fatalf("reload loaded=%d errs=%v", loaded, errs)
	}
	if got := second.Get(a.ID).ProcessState(); got != ProcessAlive {
		t.Fatalf("reloaded process state = %s, want alive", got)
	}
	if _, err := second.Resume(context.Background(), a.ID); err == nil || !strings.Contains(err.Error(), "live worker") {
		t.Fatalf("resume error = %v, want live-worker fence", err)
	}
}

func TestSupervisorProviderSettingsPersistAcrossReloadAndResume(t *testing.T) {
	root := t.TempDir()
	newSupervisor := func(baseURL string, insecureTLS bool) *Supervisor {
		return New(Config{
			Root:        root,
			RepoRoot:    root,
			BaseURL:     baseURL,
			InsecureTLS: insecureTLS,
			NewRunner: func(a *Agent) Runner {
				return RunnerFunc(func(ctx context.Context, _ Sink) error {
					<-ctx.Done()
					return ctx.Err()
				})
			},
		})
	}

	first := newSupervisor("https://parent.example.test/v1", true)
	a, err := first.SpawnReq(context.Background(), SpawnRequest{Task: "x"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if a.BaseURL != "https://parent.example.test/v1" || !a.InsecureTLS {
		t.Fatalf("spawned settings = (%q, %v), want inherited config", a.BaseURL, a.InsecureTLS)
	}
	if snap := a.Snapshot(); snap.BaseURL != a.BaseURL || snap.InsecureTLS != a.InsecureTLS {
		t.Fatalf("snapshot settings = (%q, %v), want (%q, %v)", snap.BaseURL, snap.InsecureTLS, a.BaseURL, a.InsecureTLS)
	}
	if err := first.Stop(a.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	a.Wait()

	metaBytes, err := os.ReadFile(filepath.Join(root, "agents", a.ID, "meta.json"))
	if err != nil {
		t.Fatalf("read meta.json: %v", err)
	}
	var meta agentMeta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("decode meta.json: %v", err)
	}
	if meta.BaseURL != a.BaseURL || !meta.InsecureTLS {
		t.Fatalf("metadata settings = (%q, %v), want (%q, %v)", meta.BaseURL, meta.InsecureTLS, a.BaseURL, a.InsecureTLS)
	}
	if strings.Contains(string(metaBytes), "api_key") || strings.Contains(string(metaBytes), "parent-secret") {
		t.Fatalf("metadata contains credential material: %s", metaBytes)
	}

	second := newSupervisor("https://different.example.test/v1", false)
	if loaded, errs := second.Reload(); loaded != 1 || len(errs) != 0 {
		t.Fatalf("reload loaded=%d errs=%v", loaded, errs)
	}
	reloaded := second.Get(a.ID)
	if reloaded == nil || reloaded.BaseURL != a.BaseURL || !reloaded.InsecureTLS {
		t.Fatalf("reloaded settings = (%q, %v), want (%q, %v)", reloaded.BaseURL, reloaded.InsecureTLS, a.BaseURL, a.InsecureTLS)
	}
	resumed, err := second.Resume(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.BaseURL != a.BaseURL || !resumed.InsecureTLS {
		t.Fatalf("resumed settings = (%q, %v), want (%q, %v)", resumed.BaseURL, resumed.InsecureTLS, a.BaseURL, a.InsecureTLS)
	}
	if err := second.Stop(resumed.ID); err != nil {
		t.Fatalf("stop resumed: %v", err)
	}
	resumed.Wait()
}

func TestExplicitChildProviderDoesNotInheritHostEndpoint(t *testing.T) {
	f := New(Config{
		Root: t.TempDir(), RepoRoot: t.TempDir(), Provider: "anthropic",
		BaseURL: "https://host.example.test/v1", InsecureTLS: true,
		NewRunner: func(*Agent) Runner {
			return RunnerFunc(func(context.Context, Sink) error { return nil })
		},
	})
	a, err := f.SpawnReq(context.Background(), SpawnRequest{Task: "x", Provider: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if a.BaseURL != "" || a.InsecureTLS {
		t.Fatalf("explicit provider inherited host settings: (%q, %v)", a.BaseURL, a.InsecureTLS)
	}
}

func TestSpawnRequestProviderSettingsOverrideConfig(t *testing.T) {
	root := t.TempDir()
	f := New(Config{
		Root:     root,
		RepoRoot: root,
		BaseURL:  "https://config.example.test/v1",
		NewRunner: func(*Agent) Runner {
			return RunnerFunc(func(context.Context, Sink) error { return nil })
		},
	})
	a, err := f.SpawnReq(context.Background(), SpawnRequest{
		Task:        "x",
		BaseURL:     "https://request.example.test/v1",
		InsecureTLS: true,
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	a.Wait()
	if a.BaseURL != "https://request.example.test/v1" || !a.InsecureTLS {
		t.Fatalf("spawned settings = (%q, %v), want request override", a.BaseURL, a.InsecureTLS)
	}
}

// TestSpawnReqPersistsModel confirms that the per-agent model
// override is captured at Spawn time, surfaced via Snapshot, and
// written to meta.json so a later Reload + Resume reuses it.
func TestSpawnReqPersistsModel(t *testing.T) {
	root := t.TempDir()
	f := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(a *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, _ Sink) error { <-ctx.Done(); return ctx.Err() })
		},
	})
	f.SetActiveSession("host-session")
	a, err := f.SpawnReq(context.Background(), SpawnRequest{
		Task: "x", Model: "claude-sonnet-4-5", Provider: "anthropic",
		Reasoning: "high", Subagent: "reviewer",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if a.Model != "claude-sonnet-4-5" || a.Provider != "anthropic" || a.Reasoning != "high" || a.Subagent != "reviewer" || a.SessionID != "host-session" {
		t.Fatalf("agent fields = (%q,%q,%q,%q,%q); want model/provider/reasoning/profile/session", a.Model, a.Provider, a.Reasoning, a.Subagent, a.SessionID)
	}
	snap := a.Snapshot()
	if snap.Model != "claude-sonnet-4-5" || snap.Provider != "anthropic" || snap.Reasoning != "high" || snap.Subagent != "reviewer" {
		t.Fatalf("snapshot = (%q,%q,%q,%q); child fields not surfaced", snap.Model, snap.Provider, snap.Reasoning, snap.Subagent)
	}

	// Stop so we can read meta.json without racing the run loop.
	if err := f.Stop(a.ID); err != nil {
		t.Fatal(err)
	}
	a.Wait()

	metaBytes, err := os.ReadFile(filepath.Join(root, "agents", a.ID, "meta.json"))
	if err != nil {
		t.Fatalf("read meta.json: %v", err)
	}
	var got agentMeta
	if err := json.Unmarshal(metaBytes, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "claude-sonnet-4-5" || got.Provider != "anthropic" || got.Reasoning != "high" || got.Subagent != "reviewer" || got.SessionID != "host-session" {
		t.Errorf("meta = (%q,%q,%q,%q,%q); want model/provider/reasoning/profile/session persisted", got.Model, got.Provider, got.Reasoning, got.Subagent, got.SessionID)
	}

	// Reload in a fresh Supervisor and confirm the detached agent still
	// carries the model/provider so Resume can route the child
	// subprocess back to the same model.
	g := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(a *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, _ Sink) error { <-ctx.Done(); return ctx.Err() })
		},
	})
	if loaded, errs := g.Reload(); loaded != 1 || len(errs) > 0 {
		t.Fatalf("reload loaded=%d errs=%v", loaded, errs)
	}
	re := g.Get(a.ID)
	if re == nil {
		t.Fatal("reloaded agent missing")
	}
	if re.Model != "claude-sonnet-4-5" || re.Provider != "anthropic" || re.Reasoning != "high" || re.Subagent != "reviewer" || re.SessionID != "host-session" {
		t.Errorf("reloaded fields = (%q,%q,%q,%q,%q); want preserved", re.Model, re.Provider, re.Reasoning, re.Subagent, re.SessionID)
	}

	resumed, err := g.Resume(context.Background(), re.ID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Reasoning != "high" || resumed.Subagent != "reviewer" || resumed.SessionID != "host-session" {
		t.Errorf("resumed fields = (%q,%q,%q); want reasoning/profile/session preserved", resumed.Reasoning, resumed.Subagent, resumed.SessionID)
	}
	if err := g.Stop(resumed.ID); err != nil {
		t.Fatalf("stop resumed agent: %v", err)
	}
	resumed.Wait()
}

// TestSubagentWorkerArgsIncludesModelFlags pins the argv contract that
// connects the supervisor's per-agent model override to the child's
// --model / --provider flag set. Adding a new path to argv without
// updating this assertion is the failure mode this catches.
func TestSubagentWorkerArgsIncludesModelFlags(t *testing.T) {
	args := subagentWorkerArgs(subagentWorkerArgsOpts{
		Exe: "/zut", Dir: "/wt", SessionPath: "/s.json", InboxPath: "/in.sock",
		Task: "do x", Model: "gpt-5", Provider: "openai",
	})
	want := []string{"--model", "gpt-5", "--provider", "openai"}
	for i := 0; i+1 < len(want); i += 2 {
		flag, value := want[i], want[i+1]
		found := false
		for j := 0; j+1 < len(args); j++ {
			if args[j] == flag && args[j+1] == value {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("argv missing %s %s: %v", flag, value, args)
		}
	}
	// And the task still ends up positional last.
	if args[len(args)-1] != "do x" {
		t.Errorf("task should be last positional; argv=%v", args)
	}
}

// TestStopOnDetachedAgentIsNoopAndDoesNotPanic regression-tests the
// segfault that hit when the user pressed 'k' on a reloaded
// (detached) agent: Supervisor.Stop unconditionally called a.cancel(),
// but buildDetachedAgent never assigns a cancel func because there's
// no in-process runner to cancel. The fix is to short-circuit Stop
// for StatusDetached and — belt-and-braces — nil-check a.cancel.
func TestStopOnDetachedAgentIsNoopAndDoesNotPanic(t *testing.T) {
	root := t.TempDir()
	// Build a detached agent the same way Reload does: drop a
	// meta.json on disk, then ask a fresh Supervisor to pick it up.
	id := "alpha-1"
	stateDir := filepath.Join(root, "agents", id)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := agentMeta{
		ID: id, Task: "t",
		Dir:          root,
		Started:      time.Now().Add(-time.Hour),
		InboxPath:    filepath.Join(stateDir, "in.sock"),
		EventLogPath: filepath.Join(stateDir, "events.jsonl"),
		SessionPath:  filepath.Join(stateDir, "session.json"),
	}
	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(stateDir, "meta.json"), mb, 0o644); err != nil {
		t.Fatal(err)
	}

	f := New(Config{Root: root, RepoRoot: root})
	if loaded, errs := f.Reload(); loaded != 1 || len(errs) > 0 {
		t.Fatalf("reload loaded=%d errs=%v", loaded, errs)
	}
	a := f.Get(id)
	if a == nil {
		t.Fatal("detached agent missing from subagent")
	}
	if a.Status() != StatusDetached {
		t.Fatalf("setup: agent status = %q; want detached", a.Status())
	}

	// The real assertion: Stop returns cleanly without panicking.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Stop on detached agent panicked: %v", r)
		}
	}()
	if err := f.Stop(id); err != nil {
		t.Fatalf("Stop on detached agent: %v", err)
	}
	// Status must remain Detached (Stop is a no-op).
	if got := a.Status(); got != StatusDetached {
		t.Errorf("after Stop status = %q; want detached (no-op)", got)
	}

	// StopAll, which the cli defers on exit, must also be safe.
	f.StopAll()
}

// TestRemoveAlsoCleansStateDir ensures Remove deletes the on-disk
// state directory in addition to the worktree, so a removed agent
// doesn't reappear on the next Reload.
func TestRemoveAlsoCleansStateDir(t *testing.T) {
	root := t.TempDir()
	f := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(a *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, _ Sink) error { <-ctx.Done(); return ctx.Err() })
		},
	})
	a, err := f.Spawn(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "agents", a.ID)
	if _, err := os.Stat(filepath.Join(stateDir, "meta.json")); err != nil {
		t.Fatalf("meta.json missing pre-remove: %v", err)
	}
	if err := f.Stop(a.ID); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if err := f.Remove(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state dir should be gone after remove; got %v", err)
	}

	// A fresh Supervisor + Reload should find nothing.
	g := New(Config{Root: root, RepoRoot: root})
	if loaded, _ := g.Reload(); loaded != 0 {
		t.Fatalf("reload after remove loaded=%d; want 0", loaded)
	}
}

func TestRemoveReloadedAgentKeepsSiblingState(t *testing.T) {
	root := t.TempDir()
	first := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(*Agent) Runner {
			return RunnerFunc(func(context.Context, Sink) error { return nil })
		},
	})
	a1, err := first.Spawn(context.Background(), "first")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := first.Spawn(context.Background(), "second")
	if err != nil {
		t.Fatal(err)
	}
	a1.Wait()
	a2.Wait()

	second := New(Config{Root: root, RepoRoot: root})
	if loaded, errs := second.Reload(); loaded != 2 || len(errs) != 0 {
		t.Fatalf("reload loaded=%d errs=%v", loaded, errs)
	}
	if err := second.Remove(a1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "agents", a2.ID, "meta.json")); err != nil {
		t.Fatalf("removing reloaded sibling deleted surviving metadata: %v", err)
	}
}

// TestActiveSessionScopesSnapshotCurrentSession proves the dashboard's
// default session filter. SnapshotAll remains the explicit cross-session view.
func TestActiveSessionScopesSnapshotCurrentSession(t *testing.T) {
	root := t.TempDir()
	f := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(a *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, _ Sink) error { <-ctx.Done(); return ctx.Err() })
		},
	})

	// Spawn one agent under session A and one under session B.
	f.SetActiveSession("sess-A")
	aA, err := f.Spawn(context.Background(), "task A")
	if err != nil {
		t.Fatal(err)
	}
	f.SetActiveSession("sess-B")
	aB, err := f.Spawn(context.Background(), "task B")
	if err != nil {
		t.Fatal(err)
	}

	// Both agents must carry the session id they were spawned under,
	// regardless of any later SetActiveSession.
	if aA.SessionID != "sess-A" {
		t.Errorf("aA.SessionID = %q; want sess-A", aA.SessionID)
	}
	if aB.SessionID != "sess-B" {
		t.Errorf("aB.SessionID = %q; want sess-B", aB.SessionID)
	}

	// Active session B → only aB visible.
	only := snapshotIDs(f.SnapshotCurrentSession())
	if len(only) != 1 || only[0] != aB.ID {
		t.Errorf("scoped to sess-B, snapshot ids = %v; want [%s]", only, aB.ID)
	}

	// Switch back to A → only aA visible.
	f.SetActiveSession("sess-A")
	only = snapshotIDs(f.SnapshotCurrentSession())
	if len(only) != 1 || only[0] != aA.ID {
		t.Errorf("scoped to sess-A, snapshot ids = %v; want [%s]", only, aA.ID)
	}

	// The All filter is independent of the active session.
	all := snapshotIDs(f.SnapshotAllSessions())
	if len(all) != 2 {
		t.Errorf("all-session snapshot ids = %v; want both agents", all)
	}

	// Clear the scope → both visible from the default view too.
	f.SetActiveSession("")
	only = snapshotIDs(f.SnapshotCurrentSession())
	if len(only) != 2 {
		t.Errorf("unscoped current-session ids = %v; want both agents", only)
	}

	// Cleanup.
	_ = f.Stop(aA.ID)
	_ = f.Stop(aB.ID)
	aA.Wait()
	aB.Wait()
}

// TestSessionIDPersistsAcrossReload confirms the SessionID field
// survives a Supervisor restart: an agent spawned under session A in one
// Supervisor instance is still SessionID="sess-A" after a fresh New +
// Reload reads it back from meta.json. Without persistence, the
// scope filter would forget which session owned each agent after
// a zut restart and the dashboard would leak everything again.
func TestSessionIDPersistsAcrossReload(t *testing.T) {
	root := t.TempDir()
	mkSupervisor := func() *Supervisor {
		return New(Config{
			Root: root, RepoRoot: root,
			NewRunner: func(a *Agent) Runner {
				return RunnerFunc(func(ctx context.Context, _ Sink) error { <-ctx.Done(); return ctx.Err() })
			},
		})
	}

	f := mkSupervisor()
	f.SetActiveSession("sess-keep")
	a, err := f.Spawn(context.Background(), "persist me")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Stop(a.ID)
	a.Wait()

	// Fresh Supervisor, same root, reload from disk.
	g := mkSupervisor()
	if loaded, errs := g.Reload(); loaded != 1 || len(errs) > 0 {
		t.Fatalf("reload loaded=%d errs=%v; want 1 / no errs", loaded, errs)
	}
	g.SetActiveSession("sess-keep")
	got := snapshotIDs(g.SnapshotCurrentSession())
	if len(got) != 1 || got[0] != a.ID {
		t.Errorf("after reload + scope to sess-keep, ids = %v; want [%s]", got, a.ID)
	}

	// A terminal persisted agent is hidden by the default session view, but
	// remains available from the explicit all-sessions filter.
	g.SetActiveSession("sess-other")
	if got := snapshotIDs(g.SnapshotCurrentSession()); len(got) != 0 {
		t.Errorf("current session scoped to other session, ids = %v; want none", got)
	}
	if got := snapshotIDs(g.SnapshotAllSessions()); len(got) != 1 || got[0] != a.ID {
		t.Errorf("all sessions ids = %v; want [%s]", got, a.ID)
	}
}

// TestEmptySessionIDIsHiddenFromScopedViews keeps historical unscoped
// agents out of a session-specific dashboard while retaining them in the
// explicit all-sessions view.
func TestEmptySessionIDIsHiddenFromScopedViews(t *testing.T) {
	root := t.TempDir()
	f := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(a *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, _ Sink) error { <-ctx.Done(); return ctx.Err() })
		},
	})
	// No SetActiveSession call → agent spawned with empty SessionID.
	a, err := f.Spawn(context.Background(), "unscoped")
	if err != nil {
		t.Fatal(err)
	}
	if a.SessionID != "" {
		t.Fatalf("unscoped spawn produced SessionID %q; want empty", a.SessionID)
	}

	f.SetActiveSession("")
	if got := snapshotIDs(f.SnapshotCurrentSession()); len(got) != 1 || got[0] != a.ID {
		t.Errorf("unscoped view ids = %v; want [%s]", got, a.ID)
	}
	for _, scope := range []string{"any-session", "some-other"} {
		f.SetActiveSession(scope)
		if got := snapshotIDs(f.SnapshotCurrentSession()); len(got) != 0 {
			t.Errorf("scope=%q: ids = %v; want none", scope, got)
		}
		if got := snapshotIDs(f.SnapshotAllSessions()); len(got) != 1 || got[0] != a.ID {
			t.Errorf("all sessions for scope=%q: ids = %v; want [%s]", scope, got, a.ID)
		}
	}

	_ = f.Stop(a.ID)
	a.Wait()
}

func TestWebSearchPolicyPersistsThroughReloadResumeAndChildArgv(t *testing.T) {
	root := t.TempDir()
	newSupervisor := func(policy WebSearchPolicy) *Supervisor {
		return New(Config{
			Root:            root,
			RepoRoot:        root,
			WebSearchPolicy: policy,
			NewRunner: func(*Agent) Runner {
				return RunnerFunc(func(context.Context, Sink) error { return nil })
			},
		})
	}

	first := newSupervisor(WebSearchAllow)
	t.Cleanup(first.StopAll)
	spawned, err := first.Spawn(context.Background(), "web policy")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	spawned.Wait()
	if spawned.WebSearchPolicy != WebSearchAllow {
		t.Fatalf("spawned policy = %v, want allow", spawned.WebSearchPolicy)
	}

	metaBytes, err := os.ReadFile(filepath.Join(root, "agents", spawned.ID, "meta.json"))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var meta agentMeta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if meta.WebSearchPolicy != WebSearchAllow {
		t.Fatalf("metadata policy = %v, want allow", meta.WebSearchPolicy)
	}

	second := newSupervisor(WebSearchDeny)
	t.Cleanup(second.StopAll)
	if loaded, errs := second.Reload(); loaded != 1 || len(errs) != 0 {
		t.Fatalf("reload loaded=%d errs=%v; want one clean load", loaded, errs)
	}
	reloaded := second.Get(spawned.ID)
	if reloaded == nil {
		t.Fatal("reloaded agent missing")
	}
	if reloaded.WebSearchPolicy != WebSearchAllow {
		t.Fatalf("reloaded policy = %v, want allow", reloaded.WebSearchPolicy)
	}

	resumed, err := second.ResumeSession(context.Background(), spawned.ID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.WebSearchPolicy != WebSearchAllow {
		t.Fatalf("resumed policy = %v, want allow", resumed.WebSearchPolicy)
	}
	args := defaultChildArgs("/zut", resumed, "/state/session.json", "/state/inbox.sock")
	idx := indexOf(args, "--web-search-policy")
	if idx < 0 || safeAt(args, idx+1) != "allow" {
		t.Fatalf("resumed child argv = %v, want explicit allow", args)
	}
	resumed.Wait()
}

func snapshotIDs(ss []AgentSnapshot) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.ID
	}
	return out
}
