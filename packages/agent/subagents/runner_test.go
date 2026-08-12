package subagents

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDeadlineShutdownCommandIdentifiesOrigin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subagent inbox transport uses Unix-domain sockets")
	}
	path := filepath.Join(shortSocketDir(t), "deadline.sock")
	listener, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	a := &Agent{ID: "deadline-agent", InboxPath: path, inbox: NewInbox(path)}
	defer a.inbox.Close()
	r := &execRunner{agent: a, GracePeriod: time.Second}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	runnerDone := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		r.stopOnContextDone(ctx, &exec.Cmd{}, runnerDone)
		close(watcherDone)
	}()

	var command Envelope
	select {
	case line := <-listener.Lines():
		command, err = ParseCommand(line)
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("deadline shutdown command was not received")
	}
	close(runnerDone)
	<-watcherDone

	var payload AgentShutdownPayload
	if err := command.DecodePayload(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Origin != ShutdownOriginDeadline {
		t.Fatalf("shutdown origin = %q, want %q", payload.Origin, ShutdownOriginDeadline)
	}
}

// TestSubagentWorkerArgs locks in the exact flag set the subprocess
// runner uses to start a subagent agent in daemon mode. Past
// regressions in this area:
//
//   - "--no-sess" instead of "--no-session" (old print-mode runner):
//     every spawned agent died with "unknown flag" before it could
//     talk to the model.
//
//   - Forgetting --cwd: the child resolved tools against the parent
//     zut's working directory, defeating the whole point of the
//     worktree isolation.
//
//   - Forgetting --session: a daemon-mode agent without a session
//     file would lose context between follow-up turns, making
//     "send another message" mostly useless.
//
// The test asserts the load-bearing pieces are present in plausible
// positions. If a flag is renamed, update both the runner and this
// test so we notice immediately.
func TestCredentialStdinHelperProcess(t *testing.T) {
	if os.Getenv("ZUT_SUBAGENT_CREDENTIAL_HELPER") != "1" {
		return
	}
	if os.Getenv("ZUT_SUBAGENT_CREDENTIAL_STDIN") != "1" {
		os.Exit(2)
	}
	var credential Credential
	if err := json.NewDecoder(os.Stdin).Decode(&credential); err != nil {
		os.Exit(3)
	}
	if credential.Value != "inherited-secret" || credential.Method != "apikey" {
		os.Exit(4)
	}
	for _, arg := range os.Args {
		if strings.Contains(arg, credential.Value) {
			os.Exit(5)
		}
	}
	for _, env := range os.Environ() {
		if strings.Contains(env, credential.Value) {
			os.Exit(6)
		}
	}
	fmt.Println(`{"type":"agent_stopped","data":{"reason":"completed"}}`)
	os.Exit(0)
}

// TestMessageDeltaStreamsAndReplaysWithoutDuplicatingFinalMessage verifies
// that the durable event stream has the same visible result while live and
// after history replay.
func TestMessageDeltaStreamsAndReplaysWithoutDuplicatingFinalMessage(t *testing.T) {
	events := []Event{
		NewEvent("message.delta", map[string]any{"delta": "partial "}),
		NewEvent("message.delta", map[string]any{"delta": "answer"}),
		NewEvent("assistant_message", map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "partial answer"},
		}}),
	}

	live := &Agent{transcriptLoaded: true}
	for _, event := range events {
		applyEventToSink(event, agentSink{a: live})
	}
	if got := strings.Join(live.Transcript(), "\n"); got != "partial answer" {
		t.Fatalf("live transcript = %q, want final assistant message once", got)
	}
	if got := live.Snapshot().LastAssistant; got != "partial answer" {
		t.Fatalf("live assistant output = %q, want %q", got, "partial answer")
	}

	replayed := &Agent{transcriptLoaded: true}
	for _, event := range events {
		replayEventTranscript(replayed, event)
	}
	if got := strings.Join(replayed.Transcript(), "\n"); got != "partial answer" {
		t.Fatalf("replayed transcript = %q, want final assistant message once", got)
	}
}

func TestMessageDeltaBoundsLiveProjection(t *testing.T) {
	a := &Agent{maxOutputBytes: 32, maxOutputLines: 100, transcriptLoaded: true}
	applyEventToSink(NewEvent("message.delta", map[string]any{"delta": strings.Repeat("x", 64)}), agentSink{a: a})
	applyEventToSink(NewEvent("message.delta", map[string]any{"delta": "discarded"}), agentSink{a: a})

	a.mu.Lock()
	streamed := a.streamingAssistantText
	truncated := a.streamingAssistantTruncated
	a.mu.Unlock()
	if !truncated {
		t.Fatal("streamed output was not marked truncated")
	}
	if got := len([]byte(streamed)); got > 32 {
		t.Fatalf("streamed output length = %d, want at most 32", got)
	}
	snapshot := a.Snapshot()
	if got := snapshot.LastAssistant; got != streamed {
		t.Fatalf("live assistant output = %q, want bounded stream %q", got, streamed)
	}
	if !snapshot.OutputTruncated {
		t.Fatal("snapshot did not expose truncated streamed output")
	}

	events := []Event{
		NewEvent(EventMessageDelta, map[string]any{"delta": "first partial"}),
		NewEvent("turn_end", map[string]any{"stop": "cancelled"}),
		NewEvent(EventMessageDelta, map[string]any{"delta": "second partial"}),
	}
	want := "first partial\nsecond partial"
	assertProjection := func(t *testing.T, agent *Agent) {
		t.Helper()
		if got := strings.Join(agent.Transcript(), "\n"); got != want {
			t.Fatalf("transcript = %q, want %q", got, want)
		}
		agent.mu.Lock()
		truncated := agent.streamingAssistantTruncated
		agent.mu.Unlock()
		if truncated {
			t.Fatal("fresh second-turn stream was incorrectly truncated")
		}
	}

	live := &Agent{maxOutputBytes: 100, maxOutputLines: 100, transcriptLoaded: true}
	for _, event := range events {
		applyEventToSink(event, agentSink{a: live})
	}
	assertProjection(t, live)

	replayed := &Agent{maxOutputBytes: 100, maxOutputLines: 100, transcriptLoaded: true}
	replayTranscriptIntoAgent(replayed, events)
	assertProjection(t, replayed)

	legacy := &Agent{maxOutputBytes: 100, maxOutputLines: 100, transcriptLoaded: true}
	replayEventsIntoAgent(legacy, events)
	assertProjection(t, legacy)

	truncatedTurn := &Agent{maxOutputBytes: 16, maxOutputLines: 100, transcriptLoaded: true}
	applyEventToSink(NewEvent(EventMessageDelta, map[string]any{"delta": strings.Repeat("x", 64)}), agentSink{a: truncatedTurn})
	applyEventToSink(NewEvent("turn_end", map[string]any{"stop": "cancelled"}), agentSink{a: truncatedTurn})
	truncatedTurn.mu.Lock()
	streamText := truncatedTurn.streamingAssistantText
	streamTruncated := truncatedTurn.streamingAssistantTruncated
	truncatedTurn.mu.Unlock()
	if streamText != "" || streamTruncated {
		t.Fatalf("stream state after turn boundary = (%q, %v), want empty and not truncated", streamText, streamTruncated)
	}
}

func TestWorkerEnvironmentRedactsProviderSecrets(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "openai-secret")
	t.Setenv("CUSTOM_PROVIDER_API_KEY", "custom-secret")
	t.Setenv("HF_TOKEN", "hf-secret")
	t.Setenv("GITHUB_TOKEN", "github-secret")
	t.Setenv("CUSTOM_PROVIDER_TOKEN", "token-secret")
	t.Setenv("CUSTOM_PROVIDER_KEY", "key-secret")
	t.Setenv("AWS_ACCESS_KEY_ID", "aws-access-secret")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/tmp/service-account.json")
	t.Setenv("ZUT_SUBAGENT_TEST_VALUE", "not-secret")
	env := strings.Join(workerEnvironment("openai"), "\n")
	for _, secret := range []string{
		"OPENAI_API_KEY=openai-secret",
		"CUSTOM_PROVIDER_API_KEY=custom-secret",
		"HF_TOKEN=hf-secret",
		"GITHUB_TOKEN=github-secret",
		"CUSTOM_PROVIDER_TOKEN=token-secret",
		"CUSTOM_PROVIDER_KEY=key-secret",
		"AWS_ACCESS_KEY_ID=aws-access-secret",
		"GOOGLE_APPLICATION_CREDENTIALS=/tmp/service-account.json",
	} {
		if strings.Contains(env, secret) {
			t.Fatalf("worker environment leaked %s", secret)
		}
	}
	if !strings.Contains(env, "ZUT_SUBAGENT_TEST_VALUE=not-secret") {
		t.Fatal("worker environment dropped unrelated variables")
	}

	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "bedrock-secret")
	bedrockEnv := strings.Join(workerEnvironment("amazon-bedrock"), "\n")
	if !strings.Contains(bedrockEnv, "AWS_BEARER_TOKEN_BEDROCK=bedrock-secret") {
		t.Fatal("Bedrock ambient credential chain was unexpectedly removed")
	}
	if !strings.Contains(bedrockEnv, "AWS_ACCESS_KEY_ID=aws-access-secret") {
		t.Fatal("Bedrock AWS access-key credential was unexpectedly removed")
	}
}

func TestExecRunnerTransfersCredentialOnlyOnStdin(t *testing.T) {
	t.Setenv("ZUT_SUBAGENT_CREDENTIAL_HELPER", "1")
	root := t.TempDir()
	a := &Agent{
		ID:           "credential-test",
		Dir:          root,
		Provider:     "custom",
		SessionPath:  filepath.Join(root, "session.json"),
		InboxPath:    filepath.Join(root, "inbox"),
		EventLogPath: filepath.Join(root, "events.jsonl"),
	}
	calls := 0
	r := &execRunner{
		agent:   a,
		Command: []string{os.Args[0], "-test.run=^TestCredentialStdinHelperProcess$"},
		resolveCredential: func(context.Context, string) (Credential, error) {
			calls++
			return Credential{Value: "inherited-secret", Method: "apikey"}, nil
		},
	}
	if err := r.Run(context.Background(), agentSink{a: a}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("credential resolver called %d times, want 1", calls)
	}
}

func TestApplyEventPreservesMultilineRolesAndFinalAssistant(t *testing.T) {
	a := &Agent{}
	sink := agentSink{a: a}
	applyEventToSink(NewEvent("user_message", map[string]any{"content": []any{
		map[string]any{"type": "text", "text": "first line\n\nlast line"},
	}}), sink)
	applyEventToSink(NewEvent("assistant_message", map[string]any{"content": []any{
		map[string]any{"type": "text", "text": "complete\nanswer"},
	}}), sink)

	snap := a.Snapshot()
	wantLines := []string{"user: first line", "user: ", "user: last line", "complete", "answer"}
	if strings.Join(snap.Lines, "\n") != strings.Join(wantLines, "\n") {
		t.Fatalf("transcript = %#v, want %#v", snap.Lines, wantLines)
	}
	if snap.LastAssistant != "complete\nanswer" {
		t.Fatalf("last assistant = %q", snap.LastAssistant)
	}
}

func TestSubagentWorkerArgs(t *testing.T) {
	args := subagentWorkerArgs(subagentWorkerArgsOpts{
		Exe:         "/path/to/zut",
		Dir:         "/tmp/worktree",
		SessionPath: "/tmp/state/session.json",
		InboxPath:   "/tmp/state/in.sock",
		Task:        "do the thing",
		Reasoning:   "high",
		Subagent:    "reviewer",
		TurnTimeout: 15 * time.Minute,
	})
	if len(args) < 7 {
		t.Fatalf("argv unexpectedly short: %v", args)
	}
	if args[0] != "/path/to/zut" {
		t.Fatalf("argv[0] = %q; want the binary path", args[0])
	}
	// The task must come last so anything that looks flag-like in
	// the task body doesn't get interpreted as a flag.
	if args[len(args)-1] != "do the thing" {
		t.Fatalf("task should be last positional; got %v", args)
	}

	mustHave := map[string]string{
		"--subagent-worker":       "/tmp/state/in.sock",
		"--session":               "/tmp/state/session.json",
		"--cwd":                   "/tmp/worktree",
		"--reasoning":             "high",
		"--subagent":              "reviewer",
		"--subagent-turn-timeout": "15m0s",
	}
	for flag, value := range mustHave {
		i := indexOf(args, flag)
		if i < 0 {
			t.Errorf("argv missing %q: %v", flag, args)
			continue
		}
		if i+1 >= len(args) || args[i+1] != value {
			t.Errorf("argv %q value = %q; want %q", flag, safeAt(args, i+1), value)
		}
	}

	// Reject prior bad flags explicitly so a future revert is caught.
	joined := strings.Join(args, " ")
	for _, bad := range []string{"--print", "--no-sess ", "--no-session"} {
		if strings.Contains(joined, bad) {
			t.Fatalf("argv contains stale/wrong flag %q: %s", bad, joined)
		}
	}
}

func TestDefaultChildArgsPropagatesWebSearchPolicy(t *testing.T) {
	allow := defaultChildArgs("zut", &Agent{Task: "task", WebSearchPolicy: WebSearchAllow}, "/session", "/inbox")
	idx := indexOf(allow, "--web-search-policy")
	if idx < 0 || safeAt(allow, idx+1) != "allow" {
		t.Fatalf("allow argv = %v", allow)
	}

	// A legacy/hand-built Agent has no resolved parent policy and must not
	// inherit the child process's default-enabled normal CLI setting.
	deny := defaultChildArgs("zut", &Agent{Task: "task"}, "/session", "/inbox")
	idx = indexOf(deny, "--web-search-policy")
	if idx < 0 || safeAt(deny, idx+1) != "deny" {
		t.Fatalf("deny argv = %v", deny)
	}
}

func TestResolveSubagentExecutableUsesCurrentWhenAvailable(t *testing.T) {
	got, err := resolveSubagentExecutable(context.Background(), "/current/bin/zut", "/old/bin/zut", func(_ context.Context, candidate string) (string, error) {
		if candidate == "/current/bin/zut" {
			return candidate, nil
		}
		return "", os.ErrNotExist
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "/current/bin/zut" {
		t.Fatalf("executable = %q, want current executable", got)
	}
}

func TestResolveSubagentExecutableFallsBackToPath(t *testing.T) {
	got, err := resolveSubagentExecutable(context.Background(), "/removed/.local/bin/zut", "/removed/.local/bin/zut", func(_ context.Context, candidate string) (string, error) {
		if candidate == "zut" {
			return "/active/go/bin/zut", nil
		}
		return "", os.ErrNotExist
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "/active/go/bin/zut" {
		t.Fatalf("executable = %q, want PATH fallback", got)
	}
}

func TestResolveSubagentExecutableCancelsOtherLookupsAfterFirstSuccess(t *testing.T) {
	slowStarted := make(chan struct{})
	slowCanceled := make(chan struct{})
	got, err := resolveSubagentExecutable(context.Background(), "/current/bin/zut", "/old/bin/zut", func(ctx context.Context, candidate string) (string, error) {
		switch candidate {
		case "/current/bin/zut":
			close(slowStarted)
			<-ctx.Done()
			close(slowCanceled)
			return "", ctx.Err()
		case "zut":
			<-slowStarted
			return "/active/go/bin/zut", nil
		default:
			return "", os.ErrNotExist
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "/active/go/bin/zut" {
		t.Fatalf("executable = %q, want first successful lookup", got)
	}
	select {
	case <-slowCanceled:
	case <-time.After(time.Second):
		t.Fatal("slower lookup was not canceled")
	}
}

func TestSubagentWorkerArgsPropagatesProviderConnectionSettings(t *testing.T) {
	args := subagentWorkerArgs(subagentWorkerArgsOpts{
		Exe:         "/zut",
		Dir:         "/work",
		SessionPath: "/state/session.json",
		InboxPath:   "/state/inbox.sock",
		BaseURL:     "https://gateway.example.test/v1",
		InsecureTLS: true,
	})
	if i := indexOf(args, "--base-url"); i < 0 || safeAt(args, i+1) != "https://gateway.example.test/v1" {
		t.Fatalf("argv = %v, want --base-url with inherited endpoint", args)
	}
	if !containsArg(args, "--insecure") {
		t.Fatalf("argv = %v, want inherited --insecure", args)
	}
	for _, arg := range args {
		if strings.Contains(arg, "api-key") || strings.Contains(arg, "secret") {
			t.Fatalf("argv contains credential material: %v", args)
		}
	}
}

func TestDefaultChildArgsPropagatesProviderConnectionSettings(t *testing.T) {
	a := &Agent{
		Dir:         "/work",
		BaseURL:     "https://gateway.example.test/v1",
		InsecureTLS: true,
		Timeout:     15 * time.Minute,
	}
	args := defaultChildArgs("/zut", a, "/state/session.json", "/state/inbox.sock")
	if i := indexOf(args, "--base-url"); i < 0 || safeAt(args, i+1) != a.BaseURL {
		t.Fatalf("argv = %v, want Agent.BaseURL propagated", args)
	}
	if !containsArg(args, "--insecure") {
		t.Fatalf("argv = %v, want Agent.InsecureTLS propagated", args)
	}
	if i := indexOf(args, "--subagent-turn-timeout"); i < 0 || safeAt(args, i+1) != a.Timeout.String() {
		t.Fatalf("argv = %v, want Agent.Timeout propagated per turn", args)
	}
}

// TestSubagentWorkerArgsEmptyTaskOmitsPositional makes sure that when the
// agent is being adopted (no fresh task) we don't pass an empty
// positional which the arg parser would treat as a real prompt.
func TestSubagentWorkerArgsEmptyTaskOmitsPositional(t *testing.T) {
	args := subagentWorkerArgs(subagentWorkerArgsOpts{
		Exe: "/zut", Dir: "/wt", SessionPath: "/s.json", InboxPath: "/in.sock",
	})
	for _, a := range args {
		if a == "" {
			t.Fatalf("argv contains an empty positional: %v", args)
		}
	}
	// last arg should be a real flag value, not a stray positional
	if a := args[len(args)-1]; strings.HasPrefix(a, "--") {
		t.Fatalf("argv ends on a flag with no value: %v", args)
	}
	for _, flag := range []string{"--reasoning", "--subagent"} {
		if indexOf(args, flag) >= 0 {
			t.Fatalf("empty child options should omit %s: %v", flag, args)
		}
	}
}

// TestDefaultChildArgsSpawnIncludesTask pins the spawn shape: a
// fresh (non-resuming) Agent produces argv that ends with the
// original task as a positional, so the child runs it as the
// initial user turn.
func TestDefaultChildArgsSpawnIncludesTask(t *testing.T) {
	a := &Agent{Dir: "/wt", Task: "do thing"}
	args := defaultChildArgs("/zut", a, "/s.json", "/in.sock")
	if got := args[len(args)-1]; got != "do thing" {
		t.Fatalf("spawn argv last = %q; want %q\n%v", got, "do thing", args)
	}
}

// TestDefaultChildArgsResumeOmitsTask is the regression for the
// "agent busy; send 'cancel' first" error: when an Agent is being
// resumed (Resuming==true), the child argv MUST NOT include the
// original Task as a positional. Otherwise the child fires the task
// as a fresh user turn on every resume, racing with whatever the
// user types next via the inbox.
func TestDefaultChildArgsResumeOmitsTask(t *testing.T) {
	a := &Agent{Dir: "/wt", Task: "do thing", Resuming: true}
	args := defaultChildArgs("/zut", a, "/s.json", "/in.sock")
	for _, v := range args {
		if v == "do thing" {
			t.Fatalf("resume argv contains the task; it would re-fire as a duplicate turn\n%v", args)
		}
	}
	// And no trailing positional at all: every final token must be a
	// recognized boolean flag or a value belonging to a preceding flag,
	// never a stray empty string or the original task.
	if got := args[len(args)-1]; got == "" {
		t.Fatalf("resume argv ends with an empty positional: %v", args)
	}
}

func TestDefaultChildArgsResumeUsesFollowUpPrompt(t *testing.T) {
	a := &Agent{
		Dir:      "/wt",
		Task:     "review the change",
		Resuming: true,
	}
	const followUp = "I applied your feedback. Please review it again."
	a.setResumePrompt(followUp, time.Now())
	args := defaultChildArgs("/zut", a, "/s.json", "/in.sock")
	if got := args[len(args)-1]; got != followUp {
		t.Fatalf("resume argv last = %q, want follow-up prompt %q\n%v", got, followUp, args)
	}
	for _, v := range args[:len(args)-1] {
		if v == a.Task {
			t.Fatalf("resume argv contains original task; it would duplicate the first turn\n%v", args)
		}
	}
}

func TestReadyDoesNotMarkInitialDelegatedTurnIdle(t *testing.T) {
	a := &Agent{Task: "initial delegated task"}
	a.setTurnState(TurnQueued, "")

	if err := updateAgentFromEvent(a, NewEvent(EventAgentReady, map[string]any{"lifetime_turns": 0, "current_run_turns": 0})); err != nil {
		t.Fatal(err)
	}
	if got := a.TurnState(); got != TurnQueued {
		t.Fatalf("ready changed turn state to %s, want %s", got, TurnQueued)
	}
	if err := updateAgentFromEvent(a, NewEvent(EventTurnStarted, map[string]any{
		"turn_id":           "turn-1",
		"lifetime_turns":    1,
		"current_run_turns": 1,
	})); err != nil {
		t.Fatal(err)
	}
	if got := a.TurnState(); got != TurnRunning {
		t.Fatalf("turn.started changed turn state to %s, want %s", got, TurnRunning)
	}
}

func TestUpdateAgentFromEventIgnoresStaleRejectedFollowUp(t *testing.T) {
	a := &Agent{}
	a.setResumePrompt("current prompt", time.Now())
	if err := updateAgentFromEvent(a, NewEvent("error", map[string]any{
		"code": "turn_rejected", "reason": "busy", "command_id": "stale-command",
	})); err != nil {
		t.Fatal(err)
	}
	if prompt, _ := a.ResumePromptInfo(); prompt != "current prompt" {
		t.Fatalf("stale rejection changed active prompt to %q", prompt)
	}
}

func TestUpdateAgentFromEventRecoversRejectedFollowUp(t *testing.T) {
	a := &Agent{}
	a.setResumePrompt("retry me", time.Now())
	a.setTurnState(TurnQueued, "turn-1")
	if err := updateAgentFromEvent(a, NewEvent("error", map[string]any{
		"code":       "turn_rejected",
		"reason":     "busy",
		"command_id": a.resumePromptCommandID(),
	})); err != nil {
		t.Fatal(err)
	}
	if got := a.TurnState(); got != TurnIdle {
		t.Fatalf("rejected turn state = %s, want %s", got, TurnIdle)
	}
	if prompt, _ := a.ResumePromptInfo(); prompt != "" {
		t.Fatalf("rejected prompt remained active: %q", prompt)
	}
	queued := a.resumePromptQueueSnapshot()
	if len(queued) != 1 || queued[0].Prompt != "retry me" {
		t.Fatalf("rejected prompt queue = %+v, want retry me", queued)
	}
}

func TestUpdateAgentFromEventIgnoresProviderToolLoopTurnEnd(t *testing.T) {
	a := &Agent{}
	a.setTurnState(TurnRunning, "turn-1")
	persisted := 0
	a.persistFn = func(*Agent) error { persisted++; return nil }

	updateAgentFromEvent(a, NewEvent("turn_end", map[string]any{"stop": "tool_use"}))

	if got := a.TurnState(); got != TurnRunning {
		t.Fatalf("turn state = %q, want %q", got, TurnRunning)
	}
	if got := a.CurrentTurnID(); got != "turn-1" {
		t.Fatalf("turn ID = %q, want turn-1", got)
	}
	if persisted != 0 {
		t.Fatalf("persist called %d times for provider/tool-loop turn_end, want 0", persisted)
	}
}

func TestUpdateAgentFromEventPersistsTerminalTurnEndWithStep(t *testing.T) {
	a := &Agent{}
	persisted := 0
	a.persistFn = func(*Agent) error { persisted++; return nil }

	updateAgentFromEvent(a, NewEvent("turn_end", map[string]any{"step": float64(2), "error": "boom"}))

	if got := a.TurnState(); got != TurnFailed {
		t.Fatalf("turn state = %q, want %q", got, TurnFailed)
	}
	if persisted != 1 {
		t.Fatalf("persist called %d times for terminal turn_end, want 1", persisted)
	}
}

func TestNotifyPromptTurnEndIgnoresProviderToolLoopTurnEnd(t *testing.T) {
	called := make(chan struct{}, 1)
	a := &Agent{}
	a.SetOnTurnEnd(func(step int, errMsg string) {
		called <- struct{}{}
	})

	notifyPromptTurnEnd(a, NewEvent("turn_end", map[string]any{"stop": "tool_use"}))

	select {
	case <-called:
		t.Fatal("OnTurnEnd fired for provider/tool-loop turn_end without step")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestNotifyPromptTurnEndFiresForDaemonPromptCompletion(t *testing.T) {
	type got struct {
		step int
		err  string
	}
	called := make(chan got, 1)
	a := &Agent{}
	a.SetOnTurnEnd(func(step int, errMsg string) {
		called <- got{step: step, err: errMsg}
	})

	notifyPromptTurnEnd(a, NewEvent("turn_end", map[string]any{"step": float64(2), "error": "boom"}))

	select {
	case g := <-called:
		if g.step != 2 || g.err != "boom" {
			t.Fatalf("callback = (%d, %q); want (2, boom)", g.step, g.err)
		}
	case <-time.After(time.Second):
		t.Fatal("OnTurnEnd did not fire for prompt-level turn_end with step")
	}
}

func TestSetOnTurnEndReplaysPendingNoticesInArrivalOrder(t *testing.T) {
	a := &Agent{}
	notifyPromptTurnEnd(a, NewEvent("turn_end", map[string]any{"step": float64(1)}))
	notifyPromptTurnEnd(a, NewEvent("turn_end", map[string]any{"step": float64(2), "error": "boom"}))

	type notice struct {
		step int
		err  string
	}
	var got []notice
	a.SetOnTurnEnd(func(step int, errMsg string) {
		got = append(got, notice{step: step, err: errMsg})
	})

	want := []notice{{step: 1}, {step: 2, err: "boom"}}
	if len(got) != len(want) {
		t.Fatalf("replayed %d notices, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("replayed notice %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestUpdateAgentFromEventDoesNotCountProviderTurnStarts(t *testing.T) {
	a := &Agent{}
	a.setTurnCounts(4, 2)
	a.setTurnState(TurnRunning, "turn-1")
	persisted := 0
	a.persistFn = func(*Agent) error { persisted++; return nil }

	updateAgentFromEvent(a, NewEvent(EventTurnStarted, map[string]any{"step": float64(7), "nested_turn": true}))

	if got := a.LifetimeTurnsValue(); got != 4 {
		t.Fatalf("lifetime turns = %d, want 4", got)
	}
	if got := a.CurrentRunTurnsValue(); got != 2 {
		t.Fatalf("current-run turns = %d, want 2", got)
	}
	if got := a.TurnState(); got != TurnRunning {
		t.Fatalf("turn state = %s, want %s", got, TurnRunning)
	}
	if persisted != 0 {
		t.Fatalf("provider turn start persisted %d times, want 0", persisted)
	}
}

func TestUpdateAgentFromEventPersistsCanonicalTurnCounters(t *testing.T) {
	stateDir := t.TempDir()
	a := &Agent{ID: "counter-agent", stateDir: stateDir}
	a.persistFn = func(agent *Agent) error {
		if err := writeAgentMeta(stateDir, agent); err != nil {
			t.Errorf("write agent metadata: %v", err)
			return err
		}
		return nil
	}

	updateAgentFromEvent(a, NewEvent(EventTurnStarted, map[string]any{
		"turn_id":           "turn-3",
		"lifetime_turns":    3,
		"current_run_turns": 1,
	}))

	meta, err := readAgentMeta(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if meta.LifetimeTurns != 3 || meta.CurrentRunTurns != 1 {
		t.Fatalf("persisted counters = (%d, %d), want (3, 1)", meta.LifetimeTurns, meta.CurrentRunTurns)
	}
	if got := a.Snapshot(); got.LifetimeTurns != 3 || got.CurrentRunTurns != 1 {
		t.Fatalf("snapshot counters = (%d, %d), want (3, 1)", got.LifetimeTurns, got.CurrentRunTurns)
	}
}

func TestRecordPersistenceErrorPreservesFirstFailure(t *testing.T) {
	a := &Agent{}
	a.recordPersistenceError(fmt.Errorf("first failure"))
	a.recordPersistenceError(fmt.Errorf("later failure"))
	if got := a.Snapshot().Err; !strings.Contains(got, "first failure") || strings.Contains(got, "later failure") {
		t.Fatalf("recorded persistence error = %q, want only first failure", got)
	}
}

func TestRestoreResumePromptPreservesCommandIdentity(t *testing.T) {
	a := &Agent{}
	a.setResumePrompt("old prompt", time.Now())
	oldID := a.resumePromptCommandID()
	previous := a.setResumePrompt("new prompt", time.Now())
	a.restoreResumePrompt(previous)
	if got := a.resumePromptCommandID(); got != oldID {
		t.Fatalf("restored command ID = %q, want %q", got, oldID)
	}
}

func TestEventMatchesPendingResumeRejectsIdentityLessRejection(t *testing.T) {
	a := &Agent{resumePromptID: "command-1"}
	if eventMatchesPendingResume(a, Event{Type: "error", Data: map[string]any{"code": "turn_rejected"}}) {
		t.Fatal("identity-less turn rejection matched a pending prompt")
	}
	if !eventMatchesPendingResume(a, Event{Type: EventTurnStarted, Data: map[string]any{}}) {
		t.Fatal("identity-less legacy turn.started did not match a pending prompt")
	}
	if !eventMatchesPendingResume(a, Event{Type: "error", Data: map[string]any{"code": "turn_rejected", "command_id": "command-1"}}) {
		t.Fatal("matching turn rejection did not match a pending prompt")
	}
}

func TestEventCounterRejectsNegativeFractionalAndOverflowValues(t *testing.T) {
	cases := []struct {
		name  string
		value any
		ok    bool
	}{
		{name: "valid integer", value: float64(3), ok: true},
		{name: "negative integer", value: int64(-1)},
		{name: "fractional float", value: 1.5},
		{name: "negative float", value: -1.0},
		{name: "fractional json number", value: json.Number("1.5")},
		{name: "overflow float", value: float64(^uint(0) >> 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := eventCounter(map[string]any{"counter": tc.value}, "counter")
			if ok != tc.ok {
				t.Fatalf("eventCounter ok = %t, want %t (value %v, got %d)", ok, tc.ok, tc.value, got)
			}
		})
	}
}

func TestRecordWorkerTraceNormalizesSafeLiveObservations(t *testing.T) {
	trace := NewMemoryTraceWriter()
	t.Cleanup(func() { _ = trace.Close() })
	agent := &Agent{ID: "agent-1", trace: trace}
	recordWorkerTrace(agent, Event{Type: EventMessageDelta, TurnID: "turn-1", Data: map[string]any{"delta": "sensitive answer"}})
	recordWorkerTrace(agent, Event{Type: "request_started", TurnID: "turn-1", Data: map[string]any{"provider": "test", "model": "test"}})
	recordWorkerTrace(agent, Event{Type: "turn_end", TurnID: "turn-1", Data: map[string]any{"nested_turn": true}})
	recordWorkerTrace(agent, Event{Type: "tool_call", TurnID: "turn-1", Data: map[string]any{"id": "call-1", "name": "bash", "args": "sensitive arguments"}})
	recordWorkerTrace(agent, Event{Type: "future_event", TurnID: "turn-1", Data: map[string]any{"secret": "hidden"}})
	if err := trace.Flush(); err != nil {
		t.Fatal(err)
	}
	events := trace.Events()
	if len(events) != 5 {
		t.Fatalf("events = %#v", events)
	}
	if got, want := events[0].Type, "assistant.stream.observed"; got != want {
		t.Fatalf("type = %q, want %q", got, want)
	}
	if _, found := events[0].Data["delta"]; found {
		t.Fatalf("stream payload leaked: %#v", events[0].Data)
	}
	if got, want := events[1].Type, "provider.request.started"; got != want {
		t.Fatalf("type = %q, want %q", got, want)
	}
	if got, want := events[2].Type, "provider.request.finished"; got != want {
		t.Fatalf("type = %q, want %q", got, want)
	}
	if got, want := events[3].Type, "tool.started"; got != want {
		t.Fatalf("type = %q, want %q", got, want)
	}
	if got, _ := events[3].Data["call_id"].(string); got != "call-1" {
		t.Fatalf("tool call_id = %q", got)
	}
	if _, found := events[3].Data["args"]; found {
		t.Fatalf("tool arguments leaked: %#v", events[2].Data)
	}
	if got, want := events[4].Type, "worker.protocol.observed"; got != want {
		t.Fatalf("type = %q, want %q", got, want)
	}
	if got, _ := events[4].Data["source_event"].(string); got != "future.event" {
		t.Fatalf("source_event = %q", got)
	}
	if _, found := events[4].Data["secret"]; found {
		t.Fatalf("worker payload leaked: %#v", events[3].Data)
	}
	view := ProjectTrace(append([]TraceEvent{{Type: "turn.started", AgentID: "agent-1", TurnID: "turn-1"}}, events...))["agent-1"]
	if view.PrimaryOperation == nil || view.PrimaryOperation.Type != "tool.started" || view.PrimaryOperation.Name != "bash" {
		t.Fatalf("current-turn tool activity was not projected: %#v", view)
	}
}

func TestApplyEventToSinkProjectsToolActivityIntoTranscript(t *testing.T) {
	events := []Event{
		{Type: EventToolStarted, Data: map[string]any{"name": "bash"}},
		{Type: EventToolFinished, Data: map[string]any{
			"content": []any{map[string]any{"text": "command output"}},
		}},
	}
	want := []string{"tool: bash", "tool result: completed"}

	live := &Agent{}
	for _, event := range events {
		applyEventToSink(event, agentSink{a: live})
	}
	if got := live.Transcript(); !slices.Equal(got, want) {
		t.Fatalf("live tool transcript = %#v; want %#v", got, want)
	}

	replayed := &Agent{}
	for _, event := range events {
		replayEventTranscript(replayed, event)
	}
	if got := replayed.Transcript(); !slices.Equal(got, want) {
		t.Fatalf("replayed tool transcript = %#v; want %#v", got, want)
	}
}

func indexOf(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return -1
}

func safeAt(xs []string, i int) string {
	if i < 0 || i >= len(xs) {
		return ""
	}
	return xs[i]
}
