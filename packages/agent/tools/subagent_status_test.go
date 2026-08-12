package tools

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
)

type statusTestRunner struct {
	started chan<- struct{}
	release <-chan struct{}
	err     error
}

func (r statusTestRunner) Run(ctx context.Context, sink subagents.Sink) error {
	if sink != nil {
		sink.Transcript("PRIVATE_TRANSCRIPT_MARKER\n/private/worktree api_key=PRIVATE_KEY")
	}
	if r.started != nil {
		r.started <- struct{}{}
	}
	if r.err != nil {
		return r.err
	}
	if r.release == nil {
		return nil
	}
	select {
	case <-r.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestSubagentStatusIncludesTurnCounters(t *testing.T) {
	entry := publicSubagentStatus(subagents.AgentSnapshot{
		ID:              "counter-agent",
		LifetimeTurns:   7,
		CurrentRunTurns: 2,
	})
	if entry.LifetimeTurns != 7 || entry.CurrentRunTurns != 2 {
		t.Fatalf("status counters = (%d, %d), want (7, 2)", entry.LifetimeTurns, entry.CurrentRunTurns)
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["lifetime_turns"] != float64(7) || fields["current_run_turns"] != float64(2) {
		t.Fatalf("encoded status counters = %#v", fields)
	}
}

func TestSubagentStatusSchemaHasOptionalAgentID(t *testing.T) {
	var schema struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal((&SubagentStatusTool{}).Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Type != "object" {
		t.Fatalf("schema type = %q, want object", schema.Type)
	}
	if schema.Properties["agent_id"].Type != "string" {
		t.Fatalf("agent_id schema = %#v, want string", schema.Properties["agent_id"])
	}
	if len(schema.Required) != 0 {
		t.Fatalf("required = %v, want agent_id optional", schema.Required)
	}
}

func TestSubagentStatusListsLiveWorkerWithoutPrivateOutput(t *testing.T) {
	root := t.TempDir()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	manager := subagents.New(subagents.Config{
		Root:     filepath.Join(root, "subagents"),
		RepoRoot: root,
		NewRunner: func(*subagents.Agent) subagents.Runner {
			return statusTestRunner{started: started, release: release}
		},
	})
	t.Cleanup(manager.StopAll)

	agent, err := manager.Spawn(context.Background(), "review architecture\nPRIVATE_PROMPT_MARKER")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("subagent did not start")
	}

	tool := &SubagentStatusTool{
		Supervisor: manager,
		Enabled:    func() bool { return true },
	}
	res, err := tool.Execute(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", textResult(res.Content))
	}

	var got struct {
		Agents []struct {
			ID          string `json:"agent_id"`
			StartedAt   string `json:"started_at"`
			TaskSummary string `json:"task_summary"`
		} `json:"agents"`
	}
	text := textResult(res.Content)
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("status JSON: %v\n%s", err, text)
	}
	if len(got.Agents) != 1 {
		t.Fatalf("listed agents = %d, want 1", len(got.Agents))
	}
	if got.Agents[0].ID != agent.ID {
		t.Fatalf("agent id = %q, want %q", got.Agents[0].ID, agent.ID)
	}
	if got.Agents[0].StartedAt == "" {
		t.Fatal("started_at is empty")
	}
	if got.Agents[0].TaskSummary != "review architecture" {
		t.Fatalf("task summary = %q, want first task line", got.Agents[0].TaskSummary)
	}
	for _, forbidden := range []string{
		"PRIVATE_PROMPT_MARKER",
		"PRIVATE_TRANSCRIPT_MARKER",
		"/private/worktree",
		"api_key",
		"provider",
		"transcript",
	} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(forbidden)) {
			t.Fatalf("status output contains forbidden %q:\n%s", forbidden, text)
		}
	}

	close(release)
	agent.Wait()
}

func TestSubagentStatusReportsQueuedWorkerWithoutLifecycleState(t *testing.T) {
	root := t.TempDir()
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	manager := subagents.New(subagents.Config{
		Root:     filepath.Join(root, "subagents"),
		RepoRoot: root,
		Policy: subagents.SubagentPolicy{
			MaxConcurrent: 1,
		},
		NewRunner: func(*subagents.Agent) subagents.Runner {
			return statusTestRunner{started: started, release: release}
		},
	})
	t.Cleanup(manager.StopAll)

	active, err := manager.Spawn(context.Background(), "active worker")
	if err != nil {
		t.Fatal(err)
	}
	queued, err := manager.Spawn(context.Background(), "queued worker")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("active subagent did not start")
	}

	tool := &SubagentStatusTool{
		Supervisor: manager,
		Enabled:    func() bool { return true },
	}
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"agent_id":"`+queued.ID+`"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Agent struct {
			ID string `json:"agent_id"`
		} `json:"agent"`
	}
	if err := json.Unmarshal([]byte(textResult(res.Content)), &got); err != nil {
		t.Fatal(err)
	}
	if got.Agent.ID != queued.ID {
		t.Fatalf("agent id = %q, want %q", got.Agent.ID, queued.ID)
	}

	close(release)
	active.Wait()
	queued.Wait()
}

func TestSubagentStatusQueriesOneWorkerAndReportsTerminalResultMetadata(t *testing.T) {
	root := t.TempDir()
	manager := subagents.New(subagents.Config{
		Root:     filepath.Join(root, "subagents"),
		RepoRoot: root,
		NewRunner: func(*subagents.Agent) subagents.Runner {
			return statusTestRunner{}
		},
	})
	t.Cleanup(manager.StopAll)

	agent, err := manager.Spawn(context.Background(), "finish the parser")
	if err != nil {
		t.Fatal(err)
	}
	agent.Wait()

	tool := &SubagentStatusTool{
		Supervisor: manager,
		Enabled:    func() bool { return true },
	}
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"agent_id":"`+agent.ID+`"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", textResult(res.Content))
	}

	var got struct {
		Agent struct {
			ID     string `json:"agent_id"`
			Result *struct {
				State     string `json:"state"`
				Available bool   `json:"available"`
			} `json:"result"`
		} `json:"agent"`
	}
	text := textResult(res.Content)
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("status JSON: %v\n%s", err, text)
	}
	if got.Agent.ID != agent.ID {
		t.Fatalf("agent id = %q, want %q", got.Agent.ID, agent.ID)
	}
	if got.Agent.Result == nil || !got.Agent.Result.Available || got.Agent.Result.State != "completed" {
		t.Fatalf("result metadata = %#v, want available completed result", got.Agent.Result)
	}
}

func TestSubagentStatusListsEmptySupervisor(t *testing.T) {
	tool := &SubagentStatusTool{
		Supervisor: newTestSupervisor(t),
		Enabled:    func() bool { return true },
	}

	res, err := tool.Execute(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", textResult(res.Content))
	}
	var got struct {
		Agents []json.RawMessage `json:"agents"`
	}
	text := textResult(res.Content)
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatal(err)
	}
	if got.Agents == nil || len(got.Agents) != 0 {
		t.Fatalf("agents = %#v, want present empty list", got.Agents)
	}
	if text != `{"agents":[]}` {
		t.Fatalf("empty status JSON = %s, want {\"agents\":[]}", text)
	}
}

func TestSubagentStatusRejectsUnknownWorker(t *testing.T) {
	tool := &SubagentStatusTool{
		Supervisor: newTestSupervisor(t),
		Enabled:    func() bool { return true },
	}

	res, err := tool.Execute(context.Background(), json.RawMessage(`{"agent_id":"missing"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(textResult(res.Content), "no such agent") {
		t.Fatalf("result = %#v, want unknown-agent tool error", res)
	}
}

func TestSubagentStatusOmitsLifecycleStateForFailedAndCancelledWorkers(t *testing.T) {
	for _, tc := range []struct {
		name      string
		runnerErr error
		stop      bool
	}{
		{name: "failed", runnerErr: errors.New("runner failed")},
		{name: "cancelled", stop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			started := make(chan struct{}, 1)
			release := make(chan struct{})
			manager := subagents.New(subagents.Config{
				Root:     filepath.Join(root, "subagents"),
				RepoRoot: root,
				NewRunner: func(*subagents.Agent) subagents.Runner {
					return statusTestRunner{started: started, release: release, err: tc.runnerErr}
				},
			})
			t.Cleanup(manager.StopAll)

			agent, err := manager.Spawn(context.Background(), tc.name)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("subagent did not start")
			}
			if tc.stop {
				if err := manager.Stop(agent.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				agent.Wait()
			}
			agent.Wait()

			tool := &SubagentStatusTool{
				Supervisor: manager,
				Enabled:    func() bool { return true },
			}
			res, err := tool.Execute(context.Background(), json.RawMessage(`{"agent_id":"`+agent.ID+`"}`), nil)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Agent struct {
					ID string `json:"agent_id"`
				} `json:"agent"`
			}
			if err := json.Unmarshal([]byte(textResult(res.Content)), &got); err != nil {
				t.Fatal(err)
			}
			if got.Agent.ID != agent.ID {
				t.Fatalf("agent id = %q, want %q", got.Agent.ID, agent.ID)
			}
		})
	}
}
