package modes

import (
	"slices"
	"testing"

	"github.com/bnema/zut/packages/agent/skills"
)

func TestSlashSuggesterHidesUnjailUntilJailed(t *testing.T) {
	s := newSlashSuggester()

	if got := commandNames(s.matches("/unj")); contains(got, "/unjail") {
		t.Fatalf("/unjail should be hidden while not jailed, got %v", got)
	}
	if got := commandNames(s.matches("/ja")); !contains(got, "/jail") {
		t.Fatalf("/jail should be visible while not jailed, got %v", got)
	}

	s.SetJailed(true)
	if got := commandNames(s.matches("/unj")); !contains(got, "/unjail") {
		t.Fatalf("/unjail should be visible while jailed, got %v", got)
	}
	if got := commandNames(s.matches("/ja")); contains(got, "/jail") {
		t.Fatalf("/jail should be hidden while jailed, got %v", got)
	}
}

func TestSlashSuggesterShowsLlamaOnlyWhenConfigured(t *testing.T) {
	s := newSlashSuggester()
	if got := commandNames(s.matches("/llama")); contains(got, "/llama") {
		t.Fatalf("/llama visible without login: %v", got)
	}
	s.SetLlamaConfigured(true)
	if got := commandNames(s.matches("/llama")); !contains(got, "/llama") {
		t.Fatalf("/llama missing with login: %v", got)
	}
}

func TestSlashSuggesterHasSubagents(t *testing.T) {
	s := newSlashSuggester()
	if got := commandNames(s.matches("/su")); !contains(got, "/subagents") {
		t.Fatalf("/subagents missing from suggestions, got %v", got)
	}
}

func TestSlashSuggesterHasFastModeToggle(t *testing.T) {
	s := newSlashSuggester()
	if got := commandNames(s.matches("/fa")); !contains(got, "/fast") {
		t.Fatalf("/fast missing from suggestions, got %v", got)
	}
	if !isKnownSlashCommand("/FAST") {
		t.Fatal("/FAST was not recognized as a built-in command")
	}
}

func TestSlashSuggesterHasOrchestratorToggle(t *testing.T) {
	s := newSlashSuggester()
	if got := commandNames(s.matches("/orc")); !contains(got, "/orchestrator") {
		t.Fatalf("/orchestrator missing from suggestions, got %v", got)
	}
	if !isKnownSlashCommand("/ORCHESTRATOR") {
		t.Fatal("/ORCHESTRATOR was not recognized as a built-in command")
	}
}

func TestSlashCommandsAreCaseInsensitive(t *testing.T) {
	s := newSlashSuggester()
	if got := commandNames(s.matches("/EX")); !contains(got, "/exit") {
		t.Fatalf("/EX did not suggest /exit: %v", got)
	}
	if !isKnownSlashCommand("/Exit") {
		t.Fatal("/Exit was not recognized as a built-in command")
	}
	if !isKnownSlashCommand("/Fork") {
		t.Fatal("/Fork was not recognized as a built-in command")
	}
	if !slashCancelsTurn("/CLEAR") {
		t.Fatal("/CLEAR did not retain /clear cancellation semantics")
	}
	if !slashCommandCancelsTurn("/fork") || !slashCommandCancelsTurn("/session fork") {
		t.Fatal("fork commands did not retain idle-transition cancellation semantics")
	}
	if slashCommandCancelsTurn("/session tree") {
		t.Fatal("/session tree should use its fail-closed gate instead of cancelling a turn")
	}
}

func TestSlashSuggesterBuiltinsShadowExtensionsCaseInsensitively(t *testing.T) {
	s := newSlashSuggester()
	s.SetExtra([]slashCommand{{Name: "/EXIT", Desc: "extension exit"}})
	matches := commandNames(s.matches("/exit"))
	if len(matches) != 1 || matches[0] != "/exit" {
		t.Fatalf("matches = %v, want only built-in /exit", matches)
	}
}

func TestSlashSuggesterCompletesVisibleSkills(t *testing.T) {
	s := newSlashSuggester()
	s.SetSkills([]*skills.Skill{
		{Name: "test-fix", Description: "Fix failing tests."},
		{Name: "ai-development-workflow", Description: "Guide AI development."},
		{Name: "manual-only", Description: "Explicit invocation only.", DisableModelInvocation: true},
		{Name: "builtin", Description: "Internal.", Builtin: true},
	})

	if got, want := commandNames(s.matches("/skill:")), []string{
		"/skill:ai-development-workflow",
		"/skill:manual-only",
		"/skill:test-fix",
	}; !slices.Equal(got, want) {
		t.Fatalf("matches = %v, want %v", got, want)
	}
	s.lastMatches = s.matches("/skill:")
	if got := s.Selection("/skill:"); got != "/skill:ai-development-workflow" {
		t.Fatalf("initial selection = %q", got)
	}
	s.Down()
	if got := s.Selection("/skill:"); got != "/skill:manual-only" {
		t.Fatalf("selection after down = %q", got)
	}
	if got := commandNames(s.matches("/SKILL:ai-dev")); !slices.Equal(got, []string{"/skill:ai-development-workflow"}) {
		t.Fatalf("filtered matches = %v", got)
	}
	if got := s.Selection("/skill:ai-dev"); got != "/skill:ai-development-workflow" {
		t.Fatalf("selection = %q", got)
	}
}

func TestSlashSuggesterRefreshesSkillsOncePerInputSession(t *testing.T) {
	s := newSlashSuggester()
	if s.SkillInputStarted("/skill") {
		t.Fatal("refresh started before colon")
	}
	if !s.SkillInputStarted("/skill:") {
		t.Fatal("first /skill: did not request refresh")
	}
	if s.SkillInputStarted("/skill:review") {
		t.Fatal("same input session requested another refresh")
	}
	if s.SkillInputStarted("") {
		t.Fatal("leaving skill input requested refresh")
	}
	if !s.SkillInputStarted("/SKILL:") {
		t.Fatal("new case-insensitive input session did not request refresh")
	}
}

func commandNames(cmds []slashCommand) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		if !c.Header {
			out = append(out, c.Name)
		}
	}
	return out
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
