package agent

import (
	"strings"
	"testing"
	"time"
)

func TestBuildSystemPromptAddsCompactionHandoffToCustomPrompt(t *testing.T) {
	prompt := BuildSystemPrompt(SystemPromptOpts{Custom: "custom identity"})
	if !strings.Contains(prompt, "custom identity") {
		t.Fatalf("custom prompt missing:\n%s", prompt)
	}
	if !strings.Contains(prompt, compactedSummaryHandoffInstruction) {
		t.Fatalf("custom prompt missing compaction handoff:\n%s", prompt)
	}
	if count := strings.Count(prompt, compactedSummaryHandoffInstruction); count != 1 {
		t.Fatalf("compaction handoff count = %d, want 1:\n%s", count, prompt)
	}
	for _, want := range []string{"most recent unresolved user request", "newer user request", "without waiting for the user to type \"continue\""} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("compaction handoff missing %q:\n%s", want, prompt)
		}
	}
}

func TestBuildSystemPromptCustomOmitsBuiltInDocs(t *testing.T) {
	got := BuildSystemPrompt(SystemPromptOpts{
		CWD:        "/workspace",
		Custom:     "Custom instructions",
		Append:     []string{"Additional context"},
		Now:        time.Date(2026, time.August, 6, 0, 0, 0, 0, time.UTC),
		ZutDocsDir: "/zut/docs",
	})

	if strings.Contains(got, "Zut's own docs") || strings.Contains(got, "/zut/docs") {
		t.Fatalf("custom prompt includes built-in docs guidance:\n%s", got)
	}
	for _, want := range []string{
		"Custom instructions",
		"Additional context",
		"Current date: 2026-08-06",
		"Current working directory: /workspace",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("custom prompt missing %q:\n%s", want, got)
		}
	}
}

func TestBuildSystemPromptDefaultIncludesBuiltInDocs(t *testing.T) {
	got := BuildSystemPrompt(SystemPromptOpts{
		CWD:        "/workspace",
		Now:        time.Date(2026, time.August, 6, 0, 0, 0, 0, time.UTC),
		ZutDocsDir: "/zut/docs",
	})

	if !strings.Contains(got, "Zut's own docs are installed under /zut/docs") {
		t.Fatalf("default prompt missing built-in docs guidance:\n%s", got)
	}
}
