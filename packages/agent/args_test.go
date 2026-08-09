package agent

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/google/uuid"
)

func TestParseArgsOrchestrate(t *testing.T) {
	for _, mode := range []string{"--print", "--stream", "--json"} {
		args, err := ParseArgs([]string{mode, "--orchestrate", "task"})
		if err != nil {
			t.Fatalf("ParseArgs(%q): %v", mode, err)
		}
		if !args.Orchestrate {
			t.Fatalf("ParseArgs(%q): Orchestrate = false", mode)
		}
	}
}

func TestValidateOrchestrationArgs(t *testing.T) {
	valid := Args{Mode: ModePrint, Orchestrate: true}
	if err := validateOrchestrationArgs(valid); err != nil {
		t.Fatalf("valid orchestration args: %v", err)
	}
	for _, tc := range []struct {
		name string
		args Args
		want string
	}{
		{name: "interactive", args: Args{Mode: ModeInteractive, Orchestrate: true}, want: "print, stream, or JSON"},
		{name: "rpc", args: Args{Mode: ModeRPC, Orchestrate: true}, want: "RPC"},
		{name: "worker", args: Args{Mode: ModeSubagentWorker, Orchestrate: true}, want: "subagent-worker"},
		{name: "profile", args: Args{Mode: ModePrint, Orchestrate: true, Subagent: "reviewer"}, want: "--subagent profiles"},
		{name: "stats", args: Args{Mode: ModePrint, Orchestrate: true, StatsPath: "stats.json"}, want: "--stats"},
		{name: "packaged", args: Args{Mode: ModePrint, Orchestrate: true, PermissionSet: &tools.PermissionSet{}}, want: "packaged"},
		{name: "packaged agent name", args: Args{Mode: ModePrint, Orchestrate: true, AgentName: "bundled"}, want: "packaged"},
		{name: "packaged agent data dir", args: Args{Mode: ModePrint, Orchestrate: true, AgentDataDir: "/tmp/agent-data"}, want: "packaged"},
		{name: "missing spawn", args: Args{Mode: ModePrint, Orchestrate: true, ToolsSet: true, Tools: []string{"read"}}, want: "subagent_spawn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOrchestrationArgs(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestInvalidOrchestrationRejectsBeforeRuntimeCatalogSideEffects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)

	err := runWithArgsRaw([]string{"--orchestrate", "task"}, "test")
	if err == nil || !strings.Contains(err.Error(), "requires print, stream, or JSON") {
		t.Fatalf("run error = %v, want mode validation error", err)
	}
	entries, readErr := os.ReadDir(home)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid orchestration created runtime state: %v", entries)
	}
}

func TestParseArgsSubagentAndReasoning(t *testing.T) {
	args, err := ParseArgs([]string{"--subagent-worker", "/tmp/in.sock", "--subagent", "reviewer", "--reasoning", "high", "task"})
	if err != nil {
		t.Fatal(err)
	}
	if args.Mode != ModeSubagentWorker || args.Subagent != "reviewer" || args.Reasoning != "high" || args.Prompt != "task" {
		t.Fatalf("parsed args = mode=%q subagent=%q reasoning=%q prompt=%q", args.Mode, args.Subagent, args.Reasoning, args.Prompt)
	}
}

func TestParseArgsSubagentTurnTimeout(t *testing.T) {
	args, err := ParseArgs([]string{"--subagent-worker", "/tmp/in.sock", "--subagent-turn-timeout", "15m"})
	if err != nil {
		t.Fatal(err)
	}
	if args.SubagentTurnTimeout != 15*time.Minute {
		t.Fatalf("turn timeout = %s, want %s", args.SubagentTurnTimeout, 15*time.Minute)
	}

	for _, value := range []string{"0s", "-1s", "invalid"} {
		if _, err := ParseArgs([]string{"--subagent-worker", "/tmp/in.sock", "--subagent-turn-timeout", value}); err == nil {
			t.Errorf("timeout %q unexpectedly accepted", value)
		}
	}
}

func TestParseArgsNoLSP(t *testing.T) {
	args, err := ParseArgs([]string{"--no-lsp"})
	if err != nil {
		t.Fatal(err)
	}
	if !args.NoLSP {
		t.Fatal("--no-lsp did not disable LSP")
	}
}

func TestParseArgsResumeOptionalSessionID(t *testing.T) {
	id := uuid.New()

	picker, err := ParseArgs([]string{"--resume"})
	if err != nil {
		t.Fatal(err)
	}
	if !picker.Resume || picker.ResumeSessionID != "" || picker.Prompt != "" {
		t.Fatalf("no-argument resume = %#v, want picker behavior", picker)
	}

	args, err := ParseArgs([]string{"--resume", id.String(), "continue this"})
	if err != nil {
		t.Fatal(err)
	}
	if !args.Resume || args.ResumeSessionID != id.String() || args.Prompt != "continue this" {
		t.Fatalf("resume UUID args = %#v", args)
	}

	prompt, err := ParseArgs([]string{"--resume", "not-a-session-id"})
	if err != nil {
		t.Fatal(err)
	}
	if prompt.ResumeSessionID != "" || prompt.Prompt != "not-a-session-id" {
		t.Fatalf("non-UUID resume argument = %#v, want existing prompt behavior", prompt)
	}
}

func TestParseArgsAllowsLeadingDashPromptAfterTerminator(t *testing.T) {
	args, err := ParseArgs([]string{"--subagent-worker", "/tmp/in.sock", "--", "--inspect", "auth flow"})
	if err != nil {
		t.Fatal(err)
	}
	if args.Prompt != "--inspect auth flow" {
		t.Fatalf("prompt = %q, want leading-dash task preserved", args.Prompt)
	}
}

func TestParseArgsTemperatureAllowsZero(t *testing.T) {
	args, err := ParseArgs([]string{"--temperature", "0"})
	if err != nil {
		t.Fatalf("ParseArgs returned %v", err)
	}
	if args.Temperature == nil || *args.Temperature != 0 {
		t.Fatalf("Temperature = %v; want 0", args.Temperature)
	}
}

func TestParseArgsTemperatureRejectsOutOfRange(t *testing.T) {
	if _, err := ParseArgs([]string{"--temperature", "2.1"}); err == nil {
		t.Fatal("ParseArgs accepted out-of-range temperature")
	}
}

func TestParseArgsYes(t *testing.T) {
	for _, flag := range []string{"-y", "--yes"} {
		args, err := ParseArgs([]string{flag, "--print", "hi"})
		if err != nil {
			t.Fatalf("ParseArgs(%q): %v", flag, err)
		}
		if !args.Yes {
			t.Fatalf("ParseArgs(%q): Yes = false", flag)
		}
		if args.Mode != ModePrint || args.Prompt != "hi" {
			t.Fatalf("ParseArgs(%q): Mode=%q Prompt=%q", flag, args.Mode, args.Prompt)
		}
	}
}

func TestParseArgsStatsRequiresPrintMode(t *testing.T) {
	args, err := ParseArgs([]string{"-p", "--stats", "stats.json", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if args.StatsPath != "stats.json" || args.Mode != ModePrint {
		t.Fatalf("StatsPath=%q Mode=%q", args.StatsPath, args.Mode)
	}

	if _, err := ParseArgs([]string{"--stats", "stats.json", "hi"}); err == nil {
		t.Fatal("ParseArgs accepted --stats without print mode")
	}
}

func TestParseArgsStream(t *testing.T) {
	args, err := ParseArgs([]string{"--stream", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if args.Mode != ModeStream || args.Prompt != "hi" {
		t.Fatalf("Mode=%q Prompt=%q", args.Mode, args.Prompt)
	}
}

func TestParseArgsWebSearchPolicyIsWorkerOnlyRegardlessOfOrder(t *testing.T) {
	for _, argv := range [][]string{
		{"--web-search-policy", "allow"},
		{"--web-search-policy", "allow", "--subagent-worker", "/tmp/in.sock", "--print"},
		{"--subagent-worker", "/tmp/in.sock", "--web-search-policy", "deny", "--json"},
	} {
		if _, err := ParseArgs(argv); err == nil {
			t.Fatalf("ordinary CLI mode accepted internal policy from %q", argv)
		}
	}

	for _, tc := range []struct {
		name   string
		argv   []string
		policy subagents.WebSearchPolicy
	}{
		{
			name:   "policy before worker",
			argv:   []string{"--web-search-policy", "allow", "--subagent-worker", "/tmp/in.sock", "--tools", "web_search"},
			policy: subagents.WebSearchAllow,
		},
		{
			name:   "policy after worker",
			argv:   []string{"--print", "--subagent-worker", "/tmp/in.sock", "--web-search-policy", "deny"},
			policy: subagents.WebSearchDeny,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker, err := ParseArgs(tc.argv)
			if err != nil {
				t.Fatal(err)
			}
			if worker.Mode != ModeSubagentWorker || worker.WebSearchPolicy != tc.policy {
				t.Fatalf("worker policy args = %#v", worker)
			}
		})
	}
}

func TestParseArgsTracksExplicitToolListProvenance(t *testing.T) {
	absent, err := ParseArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if absent.ToolsSet || len(absent.Tools) != 0 {
		t.Fatalf("absent --tools = %#v", absent)
	}

	empty, err := ParseArgs([]string{"--tools", ""})
	if err != nil {
		t.Fatal(err)
	}
	if !empty.ToolsSet || len(empty.Tools) != 0 {
		t.Fatalf("empty --tools = %#v", empty)
	}

	selected, err := ParseArgs([]string{"--tools", "read, web_search"})
	if err != nil {
		t.Fatal(err)
	}
	if !selected.ToolsSet || len(selected.Tools) != 2 || selected.Tools[1] != "web_search" {
		t.Fatalf("selected --tools = %#v", selected)
	}
}
