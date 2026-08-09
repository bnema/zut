package subagents

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestResumeWithPromptQueuesFollowUpsForActiveWorker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subagent inbox transport uses Unix-domain sockets")
	}

	root := shortSocketDir(t)
	ready := make(chan struct{}, 1)
	commands := make(chan Envelope, 2)
	f := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(a *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, _ Sink) error {
				listener, err := Listen(a.InboxPath)
				if err != nil {
					return err
				}
				defer listener.Close()
				a.setProcessState(ProcessAlive)
				a.setTurnState(TurnRunning, "turn-1")
				ready <- struct{}{}
				for {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case line, ok := <-listener.Lines():
						if !ok {
							return nil
						}
						command, err := ParseCommand(line)
						if err != nil {
							return err
						}
						if command.Type == CommandAgentShutdown {
							return nil
						}
						commands <- command
					}
				}
			})
		},
	})
	defer f.StopAll()

	a, err := f.Spawn(context.Background(), "review the implementation")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("active worker did not open its inbox")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.ResumeWithPrompt(canceled, a.ID, "must not be accepted"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled active follow-up error = %v, want context canceled", err)
	}

	const (
		firstFollowUp  = "Check the parser behavior."
		secondFollowUp = "Then check the error handling."
	)
	tracker := NewCompletionTracker()
	if _, err := f.ResumeWithPromptBefore(context.Background(), a.ID, firstFollowUp, func(agent *Agent, prompt string) func() {
		return tracker.TrackFutureTurn(agent, prompt, true)
	}); err != nil {
		t.Fatalf("queue first active follow-up: %v", err)
	}
	if a.OnTurnEnd == nil {
		t.Fatal("active resume did not register its turn callback before queueing")
	}
	a.OnTurnEnd(2, "")
	batch, err := tracker.WaitIdle(context.Background())
	if err != nil || len(batch) != 1 || batch[0].Task != firstFollowUp {
		t.Fatalf("active resume completion = (%+v, %v), want one %q completion", batch, err, firstFollowUp)
	}
	if _, err := f.ResumeWithPrompt(context.Background(), a.ID, secondFollowUp); err != nil {
		t.Fatalf("queue second active follow-up: %v", err)
	}
	select {
	case command := <-commands:
		t.Fatalf("active follow-up was delivered before idle: %v", command.Type)
	default:
	}

	persisted, err := readAgentMeta(filepath.Join(root, "agents", a.ID))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ResumePrompt != "" || len(persisted.ResumeQueue) != 2 {
		t.Fatalf("active follow-ups persisted as prompt %q queue %d, want empty prompt and queue 2", persisted.ResumePrompt, len(persisted.ResumeQueue))
	}
	if firstID, secondID := persisted.ResumeQueue[0].CommandID, persisted.ResumeQueue[1].CommandID; firstID == "" || secondID == "" || firstID == secondID {
		t.Fatalf("queued follow-up command IDs = %q, %q; want distinct non-empty IDs", firstID, secondID)
	}

	if err := updateAgentFromEvent(a, NewEvent(EventAgentIdle, map[string]any{"turn_id": "turn-1"})); err != nil {
		t.Fatalf("apply first idle event: %v", err)
	}
	select {
	case command := <-commands:
		var payload TurnStartPayload
		if command.Type != CommandTurnStart || command.DecodePayload(&payload) != nil || payload.Prompt != firstFollowUp {
			t.Fatalf("first idle delivery = type %q prompt %q, want turn start %q", command.Type, payload.Prompt, firstFollowUp)
		}
		if !payload.NewRun {
			t.Fatal("first queued follow-up did not request a fresh run budget")
		}
	case <-time.After(time.Second):
		t.Fatal("first active follow-up was not delivered at idle")
	}
	if got := a.TurnState(); got != TurnQueued {
		t.Fatalf("turn state after first queued delivery = %s, want %s", got, TurnQueued)
	}

	if err := updateAgentFromEvent(a, NewEvent(EventTurnStarted, map[string]any{
		"turn_id": "turn-2", "step": float64(2), "lifetime_turns": 2, "current_run_turns": 1,
	})); err != nil {
		t.Fatalf("apply second turn started event: %v", err)
	}
	if err := updateAgentFromEvent(a, NewEvent(EventAgentIdle, map[string]any{"turn_id": "turn-2"})); err != nil {
		t.Fatalf("apply second idle event: %v", err)
	}
	select {
	case command := <-commands:
		var payload TurnStartPayload
		if command.Type != CommandTurnStart || command.DecodePayload(&payload) != nil || payload.Prompt != secondFollowUp {
			t.Fatalf("second idle delivery = type %q prompt %q, want turn start %q", command.Type, payload.Prompt, secondFollowUp)
		}
	case <-time.After(time.Second):
		t.Fatal("second active follow-up was not delivered at the next idle turn")
	}

	persisted, err = readAgentMeta(filepath.Join(root, "agents", a.ID))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ResumePrompt != secondFollowUp || len(persisted.ResumeQueue) != 0 {
		t.Fatalf("second follow-up persistence = prompt %q queue %d, want prompt %q and empty queue", persisted.ResumePrompt, len(persisted.ResumeQueue), secondFollowUp)
	}
	if persisted.ResumePromptID == "" {
		t.Fatal("promoted follow-up lost its command ID")
	}
	if err := f.Stop(a.ID); err != nil {
		t.Fatal(err)
	}
	a.Wait()
}

func TestResumeWithPromptRestartTracksExistingPendingPromptBeforeNewPrompt(t *testing.T) {
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

	original, err := f.Spawn(context.Background(), "review the implementation")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("original runner did not start")
	}
	if err := f.Stop(original.ID); err != nil {
		t.Fatal(err)
	}
	original.Wait()

	const (
		pendingPrompt = "finish the queued review"
		newPrompt     = "then review the follow-up"
	)
	original.setResumePrompt(pendingPrompt, time.Now())
	if err := f.persistAgent(original); err != nil {
		t.Fatal(err)
	}

	tracker := NewCompletionTracker()
	resumed, err := f.ResumeWithPromptBefore(context.Background(), original.ID, newPrompt, func(agent *Agent, prompt string) func() {
		return tracker.TrackFutureTurn(agent, prompt, true)
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-started:
		if got != resumed {
			t.Fatalf("started agent = %p, want resumed agent %p", got, resumed)
		}
	case <-time.After(time.Second):
		t.Fatal("resumed runner did not start")
	}
	if got := resumed.resumePrompt(); got != pendingPrompt {
		t.Fatalf("first scheduled prompt = %q, want pending prompt %q", got, pendingPrompt)
	}
	queue := resumed.resumePromptQueueSnapshot()
	if len(queue) != 1 || queue[0].Prompt != newPrompt {
		t.Fatalf("resume queue = %+v, want new prompt queued second", queue)
	}
	if resumed.OnTurnEnd == nil {
		t.Fatal("restart did not install completion tracking before scheduling")
	}
	resumed.OnTurnEnd(1, "")
	resumed.OnTurnEnd(2, "")
	batch, err := tracker.WaitIdle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 2 || batch[0].Task != pendingPrompt || batch[1].Task != newPrompt {
		t.Fatalf("completion order = %+v, want pending prompt then new prompt", batch)
	}

	// Recreate the durable queue-only state: no in-flight prompt, with the
	// previously queued command still awaiting scheduler consumption.
	if err := f.Stop(resumed.ID); err != nil {
		t.Fatal(err)
	}
	resumed.Wait()
	resumed.clearResumePrompt()
	if err := f.persistAgent(resumed); err != nil {
		t.Fatal(err)
	}

	const newestPrompt = "finally synthesize both reviews"
	queueTracker := NewCompletionTracker()
	restarted, err := f.ResumeWithPromptBefore(context.Background(), resumed.ID, newestPrompt, func(agent *Agent, prompt string) func() {
		return queueTracker.TrackFutureTurn(agent, prompt, true)
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-started:
		if got != restarted {
			t.Fatalf("queue-only restart agent = %p, want %p", got, restarted)
		}
	case <-time.After(time.Second):
		t.Fatal("queue-only restarted runner did not start")
	}
	if got := restarted.resumePrompt(); got != newPrompt {
		t.Fatalf("promoted queue prompt = %q, want %q", got, newPrompt)
	}
	queue = restarted.resumePromptQueueSnapshot()
	if len(queue) != 1 || queue[0].Prompt != newestPrompt {
		t.Fatalf("queue-only restart queue = %+v, want newest prompt second", queue)
	}
	restarted.OnTurnEnd(1, "")
	restarted.OnTurnEnd(2, "")
	batch, err = queueTracker.WaitIdle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 2 || batch[0].Task != newPrompt || batch[1].Task != newestPrompt {
		t.Fatalf("queue-only completion order = %+v, want promoted prompt then newest prompt", batch)
	}
}

func TestResumeWithPromptDeliversFollowUpToIdleWorker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subagent inbox transport uses Unix-domain sockets")
	}

	root := shortSocketDir(t)
	ready := make(chan struct{}, 1)
	commands := make(chan Envelope, 2)
	f := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(a *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, _ Sink) error {
				listener, err := Listen(a.InboxPath)
				if err != nil {
					return err
				}
				defer listener.Close()
				a.setProcessState(ProcessAlive)
				a.setTurnState(TurnIdle, "")
				ready <- struct{}{}
				for {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case line, ok := <-listener.Lines():
						if !ok {
							return nil
						}
						command, err := ParseCommand(line)
						if err != nil {
							return err
						}
						if command.Type == CommandAgentShutdown {
							return nil
						}
						commands <- command
					}
				}
			})
		},
	})
	defer f.StopAll()

	a, err := f.Spawn(context.Background(), "review the implementation")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("idle worker did not open its inbox")
	}
	a.setTurnCounts(4, 2)

	const followUp = "I applied your review. What do you think now?"
	tracker := NewCompletionTracker()
	continued, err := f.ResumeWithPromptBefore(context.Background(), a.ID, followUp, func(agent *Agent, prompt string) func() {
		return tracker.TrackFutureTurn(agent, prompt, true)
	})
	if err != nil {
		t.Fatalf("continue idle worker: %v", err)
	}
	if continued != a {
		t.Fatalf("continued agent = %p, want existing worker %p", continued, a)
	}
	if got := a.TurnState(); got != TurnQueued {
		t.Fatalf("turn state after sending follow-up = %s, want %s", got, TurnQueued)
	}
	if got := a.CurrentRunTurnsValue(); got != 0 {
		t.Fatalf("current-run turns after explicit resume = %d, want 0 before turn starts", got)
	}
	if got := a.LifetimeTurnsValue(); got != 4 {
		t.Fatalf("lifetime turns after explicit resume = %d, want 4", got)
	}
	if _, err := f.ResumeWithPrompt(context.Background(), a.ID, "duplicate follow-up"); err == nil {
		t.Fatal("second follow-up succeeded while the first was queued")
	}
	select {
	case command := <-commands:
		if command.Type != CommandTurnStart {
			t.Fatalf("command type = %q, want %q", command.Type, CommandTurnStart)
		}
		var payload TurnStartPayload
		if err := command.DecodePayload(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Prompt != followUp {
			t.Fatalf("follow-up prompt = %q, want %q", payload.Prompt, followUp)
		}
		if !payload.NewRun {
			t.Fatal("idle follow-up did not request a fresh run budget")
		}
		if a.OnTurnEnd == nil {
			t.Fatal("idle resume callback was not installed before prompt delivery")
		}
		a.OnTurnEnd(5, "")
	case <-time.After(time.Second):
		t.Fatal("idle worker did not receive the follow-up")
	}
	batch, err := tracker.WaitIdle(context.Background())
	if err != nil || len(batch) != 1 || batch[0].Task != followUp {
		t.Fatalf("idle resume completion = (%+v, %v), want one %q completion", batch, err, followUp)
	}
	persisted, err := readAgentMeta(filepath.Join(root, "agents", a.ID))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ResumePrompt != followUp || persisted.ResumePromptAt.IsZero() {
		t.Fatalf("queued follow-up was not persisted: prompt %q accepted %s", persisted.ResumePrompt, persisted.ResumePromptAt)
	}
	if err := f.Stop(a.ID); err != nil {
		t.Fatal(err)
	}
	a.Wait()

	restarted := make(chan *Agent, 1)
	f2 := New(Config{
		Root: root, RepoRoot: root,
		NewRunner: func(agent *Agent) Runner {
			return RunnerFunc(func(ctx context.Context, _ Sink) error {
				restarted <- agent
				<-ctx.Done()
				return ctx.Err()
			})
		},
	})
	defer f2.StopAll()
	if loaded, errs := f2.Reload(); loaded != 1 || len(errs) != 0 {
		t.Fatalf("reload = (%d, %v), want (1, no errors)", loaded, errs)
	}
	resumed, err := f2.ResumeSession(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-restarted:
		if got != resumed || got.resumePrompt() != followUp {
			t.Fatalf("unacknowledged follow-up was not replayed: agent %p prompt %q", got, got.resumePrompt())
		}
	case <-time.After(time.Second):
		t.Fatal("restarted worker did not receive the follow-up")
	}
}
