package modes

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
)

// newInteractiveForSupervisorTest builds the minimal Interactive scaffolding
// runSubagents needs. It does NOT call NewInteractive (which would pull in
// the whole TUI); the runSubagents method only touches cfg.Supervisor, the
// status mutex, and the subagent dialog, so we hand-build those.
func newInteractiveForSupervisorTest(t *testing.T) (*Interactive, *subagents.Supervisor) {
	t.Helper()
	root := t.TempDir()
	f := subagents.New(subagents.Config{
		Root:     root,
		RepoRoot: root,
		NewRunner: func(a *subagents.Agent) subagents.Runner {
			return subagents.RunnerFunc(func(ctx context.Context, sink subagents.Sink) error {
				<-ctx.Done()
				return ctx.Err()
			})
		},
	})
	iv := &Interactive{
		subagentsDialog: newSubagentsDialog(),
		dirty:           make(chan struct{}, 1),
	}
	iv.cfg.Supervisor = f
	return iv, f
}

// TestRunSupervisorBareDoesNotPanic regression-tests the slice-out-of-range
// panic that hit when /subagents was typed with no subcommand: runSubagents
// did args[1:] without checking len(args), which panics as [1:0].
func TestRunSupervisorBareDoesNotPanic(t *testing.T) {
	iv, _ := newInteractiveForSupervisorTest(t)
	defer iv.cfg.Supervisor.StopAll()

	// Bare /subagents: parts[1:] from the dispatcher is an empty slice.
	iv.runSubagents(context.Background(), nil)

	if !iv.subagentsDialog.Active() {
		t.Fatal("bare /subagents should open the dashboard")
	}
}

func TestRunSupervisorSubcommandsDoNotPanic(t *testing.T) {
	iv, _ := newInteractiveForSupervisorTest(t)
	defer iv.cfg.Supervisor.StopAll()

	// Each row is the slice that the dispatcher hands to runSubagents —
	// i.e. parts[1:] where parts was strings.Fields of the slash
	// command. Mixing zero-arg and arg'd forms exercises both
	// branches of the reslice guard.
	cases := [][]string{
		{"list"},
		{"new"},
		{"new", "fix", "the", "thing"},
		{"kill"},
		{"kill", "no-such-id"},
		{"remove"},
		{"remove", "no-such-id"},
		{"send"},
		{"send", "no-such-id"},
		{"send", "no-such-id", "hello", "world"},
		{"bogus"},
	}
	for _, args := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("runSubagents(%v) panicked: %v", args, r)
				}
			}()
			iv.runSubagents(context.Background(), args)
		}()
	}
	iv.mu.Lock()
	statusErr := iv.statusErr
	iv.mu.Unlock()
	if !strings.Contains(statusErr, "cancel") || !strings.Contains(statusErr, "wait") {
		t.Fatalf("unknown-command hint = %q; want cancel and wait", statusErr)
	}
}

func TestRunSupervisorRejectsNamedProfileWithoutResolver(t *testing.T) {
	iv, f := newInteractiveForSupervisorTest(t)
	defer f.StopAll()

	iv.runSubagents(context.Background(), []string{"new", "--agent", "reviewer", "review", "auth"})
	if !strings.Contains(iv.statusErr, "named subagent profiles are unavailable") {
		t.Fatalf("status error = %q, want unavailable profile resolver", iv.statusErr)
	}
	if got := len(f.List()); got != 0 {
		t.Fatalf("spawned agents = %d, want 0", got)
	}
}

func TestRunSupervisorNewSpawnsAgent(t *testing.T) {
	iv, f := newInteractiveForSupervisorTest(t)
	defer f.StopAll()

	iv.runSubagents(context.Background(), []string{"new", "do", "stuff"})
	agents := f.List()
	if len(agents) != 1 {
		t.Fatalf("want 1 agent; got %d", len(agents))
	}
	if agents[0].Task != "do stuff" {
		t.Fatalf("task = %q; want %q", agents[0].Task, "do stuff")
	}
}

func TestRunSupervisorNamedProfileAppliesFastModeRestriction(t *testing.T) {
	root := t.TempDir()
	profileFastMode := false
	f := subagents.New(subagents.Config{
		Root:     root,
		RepoRoot: root,
		FastMode: true,
		NewRunner: func(*subagents.Agent) subagents.Runner {
			return subagents.RunnerFunc(func(ctx context.Context, _ subagents.Sink) error {
				<-ctx.Done()
				return ctx.Err()
			})
		},
	})
	defer f.StopAll()
	iv := &Interactive{subagentsDialog: newSubagentsDialog(), dirty: make(chan struct{}, 1)}
	iv.cfg.Supervisor = f
	iv.cfg.ResolveSubagent = func(name string) (*subagents.Profile, error) {
		if name != "reviewer" {
			return nil, nil
		}
		return &subagents.Profile{Name: name, FastMode: &profileFastMode}, nil
	}

	iv.runSubagents(context.Background(), []string{"new", "--agent", "reviewer", "review", "auth"})
	agents := f.List()
	if len(agents) != 1 || agents[0].FastMode {
		t.Fatalf("agent fast mode = %#v, want disabled by profile", agents)
	}
}

// TestRunSupervisorSendDeliversToAgentInbox spins up a real agent with a
// fake Runner whose only job is to forward inbox lines to a channel,
// then asserts the /subagents send <id> <text...> path routes through
// Supervisor.SendUserTurn and lands at the agent verbatim.
func TestRunSupervisorSendDeliversToAgentInbox(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subagent inbox transport uses Unix-domain sockets")
	}
	root := t.TempDir()
	recv := make(chan string, 4)
	ready := make(chan error, 1)
	f := subagents.New(subagents.Config{
		Root:     root,
		RepoRoot: root,
		NewRunner: func(a *subagents.Agent) subagents.Runner {
			return subagents.RunnerFunc(func(ctx context.Context, sink subagents.Sink) error {
				// Stand up a real Listener on the agent's inbox path so
				// SendUserTurn (which dials a unix socket) actually has
				// something to talk to. The runner-test stubs do the
				// same; this is the minimum to exercise the wire.
				ln, err := subagents.Listen(a.InboxPath)
				ready <- err
				if err != nil {
					return err
				}
				defer ln.Close()
				for {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case line, ok := <-ln.Lines():
						if !ok {
							return nil
						}
						recv <- line
					}
				}
			})
		},
	})
	defer f.StopAll()
	iv := &Interactive{subagentsDialog: newSubagentsDialog(), dirty: make(chan struct{}, 1)}
	iv.cfg.Supervisor = f

	a, err := f.Spawn(context.Background(), "do thing")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for agent inbox listener")
	}

	// Run /subagents send <id> <text...>. The dispatcher would have
	// already strings.Fields-ed the input; mirror that here.
	iv.runSubagents(context.Background(), []string{"send", a.ID, "please", "continue"})

	select {
	case msg := <-recv:
		command, parseErr := subagents.ParseCommand(msg)
		if parseErr != nil {
			t.Fatalf("agent received invalid command %q: %v", msg, parseErr)
		}
		if command.Type != subagents.CommandTurnStart {
			t.Fatalf("agent received command %q; want %q", command.Type, subagents.CommandTurnStart)
		}
		var payload subagents.TurnStartPayload
		if err := command.DecodePayload(&payload); err != nil {
			t.Fatalf("decode turn.start payload: %v", err)
		}
		if payload.Prompt != "please continue" {
			t.Fatalf("agent received prompt %q; want %q", payload.Prompt, "please continue")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for agent to receive the prompt")
	}

	if iv.statusErr != "" {
		t.Fatalf("status err set: %q", iv.statusErr)
	}
	if iv.statusOK == "" || iv.statusOK[:7] != "sent to" {
		t.Fatalf("status ok = %q; want \"sent to ...\"", iv.statusOK)
	}
}

func TestRunSupervisorWaitHonorsRunContext(t *testing.T) {
	iv, f := newInteractiveForSupervisorTest(t)
	defer f.StopAll()

	a, err := f.Spawn(context.Background(), "long-running task")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	watcherDone := make(chan struct{})
	iv.subagentsWaitWatcherDone = func() { close(watcherDone) }
	iv.runSubagents(ctx, []string{"wait", a.ID})
	iv.mu.Lock()
	status := iv.statusOK
	iv.mu.Unlock()
	if status != "waiting for "+a.ID {
		t.Fatalf("initial wait status = %q", status)
	}
	cancel()

	// The wait watcher must stop observing the run context and must not
	// later overwrite the status with a false completion after cleanup.
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	select {
	case <-watcherDone:
	case <-deadline.C:
		t.Fatal("timed out waiting for wait watcher to exit")
	}
	iv.mu.Lock()
	status = iv.statusOK
	iv.mu.Unlock()
	if strings.Contains(status, "completed ") {
		t.Fatalf("wait reported completion after cancellation: %q", status)
	}
}

func TestRunSupervisorWaitReportsCompletionBeforeCancellation(t *testing.T) {
	root := t.TempDir()
	f := subagents.New(subagents.Config{
		Root:     root,
		RepoRoot: root,
		NewRunner: func(*subagents.Agent) subagents.Runner {
			return subagents.RunnerFunc(func(context.Context, subagents.Sink) error { return nil })
		},
	})
	defer f.StopAll()
	iv := &Interactive{subagentsDialog: newSubagentsDialog(), dirty: make(chan struct{}, 1)}
	iv.cfg.Supervisor = f

	a, err := f.Spawn(context.Background(), "quick task")
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the ordering-sensitive path deterministically: /wait can be
	// invoked after the worker has already completed.
	a.Wait()
	watcherDone := make(chan struct{})
	iv.subagentsWaitWatcherDone = func() { close(watcherDone) }
	iv.runSubagents(context.Background(), []string{"wait", a.ID})
	select {
	case <-watcherDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for wait watcher to report completion")
	}

	iv.mu.Lock()
	status := iv.statusOK
	iv.mu.Unlock()
	if status != "completed "+a.ID {
		t.Fatalf("wait completion status = %q; want %q", status, "completed "+a.ID)
	}
}

func TestNormalizeSpawnReasoning(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{in: "OFF", want: "off", ok: true},
		{in: "minimal", want: "minimum", ok: true},
		{in: "maximum", want: "xhigh", ok: true},
		{in: "MAX", want: "max", ok: true},
		{in: "bogus", ok: false},
	}
	for _, tc := range cases {
		got, err := normalizeSpawnReasoning(tc.in)
		if tc.ok {
			if err != nil || got != tc.want {
				t.Errorf("normalizeSpawnReasoning(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("normalizeSpawnReasoning(%q) succeeded with %q; want error", tc.in, got)
		}
	}
}

func TestParseSpawnFlags(t *testing.T) {
	cases := []struct {
		in                          string
		wantModel, wantProv         string
		wantReasoning, wantSubagent string
		wantTask                    string
	}{
		{"do x", "", "", "", "", "do x"},
		{"--model claude do x", "claude", "", "", "", "do x"},
		{"--model=claude do x", "claude", "", "", "", "do x"},
		{"--provider openai --model gpt-5 do x", "gpt-5", "openai", "", "", "do x"},
		{"--provider=openai --model=gpt-5 do x", "gpt-5", "openai", "", "", "do x"},
		{"--agent reviewer --reasoning high review auth", "", "", "high", "reviewer", "review auth"},
		{"--agent --reasoning high review auth", "", "", "high", "", "review auth"},
		{"--reasoning --agent reviewer review auth", "", "", "", "reviewer", "review auth"},
		{"--thinking=max do x", "", "", "max", "", "do x"},
		// Only LEADING flags are consumed.
		{"do --model x", "", "", "", "", "do --model x"},
		// Missing value: --model with no follow-up token leaves model empty
		// and the next field starts the task.
		{"--model", "", "", "", "", ""},
	}
	for _, c := range cases {
		m, p, r, a, task := parseSpawnFlags(c.in)
		if m != c.wantModel || p != c.wantProv || r != c.wantReasoning || a != c.wantSubagent || task != c.wantTask {
			t.Errorf("parseSpawnFlags(%q) = (%q,%q,%q,%q,%q); want (%q,%q,%q,%q,%q)",
				c.in, m, p, r, a, task, c.wantModel, c.wantProv, c.wantReasoning, c.wantSubagent, c.wantTask)
		}
	}
}

func TestSplitIDAndRest(t *testing.T) {
	cases := []struct {
		in       string
		wantID   string
		wantText string
	}{
		{"", "", ""},
		{"  ", "", ""},
		{"alpha", "alpha", ""},
		{"alpha hello world", "alpha", "hello world"},
		{"  alpha   hello   world  ", "alpha", "hello   world  "},
		{"alpha\thi", "alpha", "hi"},
	}
	for _, c := range cases {
		gotID, gotText := splitIDAndRest(c.in)
		if gotID != c.wantID || gotText != c.wantText {
			t.Errorf("splitIDAndRest(%q) = (%q,%q); want (%q,%q)", c.in, gotID, gotText, c.wantID, c.wantText)
		}
	}
}

func TestRunSupervisorWithoutSupervisorIsNoop(t *testing.T) {
	iv := &Interactive{
		subagentsDialog: newSubagentsDialog(),
		dirty:           make(chan struct{}, 1),
	}
	// cfg.Supervisor stays nil. The command should set a status err and
	// otherwise be inert.
	iv.runSubagents(context.Background(), nil)
	if iv.subagentsDialog.Active() {
		t.Fatal("dialog opened despite no subagent")
	}
	if iv.statusErr == "" {
		t.Fatal("expected a status error when subagent is nil")
	}
}
