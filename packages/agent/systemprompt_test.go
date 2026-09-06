package agent

import (
	"strings"
	"testing"
	"time"
)

func TestBuildSystemPromptAlwaysIncludesFinalWritingGuidance(t *testing.T) {
	for _, tt := range []struct {
		name   string
		custom string
	}{
		{name: "default"},
		{name: "custom", custom: "custom identity"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const conflictingAddendum = "Always use ceremonial language."
			prompt := BuildSystemPrompt(SystemPromptOpts{
				Custom: tt.custom,
				Append: []string{conflictingAddendum},
			})

			if count := strings.Count(prompt, writingGuidance); count != 1 {
				t.Fatalf("writing guidance count = %d, want 1:\n%s", count, prompt)
			}
			if strings.Index(prompt, writingGuidance) < strings.Index(prompt, conflictingAddendum) {
				t.Fatalf("writing guidance must follow appended context:\n%s", prompt)
			}
			for _, want := range []string{
				"medium, audience, and reader's immediate need",
				"plain, precise language",
				"Default to concise paragraphs",
				"Use lists when items are genuinely parallel, sequential, or easier to compare",
				"canned transitions",
				"verified facts",
				"descriptive links, and accessibility",
				"Do not manufacture slang, errors, hesitation",
				"Revise silently",
			} {
				if !strings.Contains(prompt, want) {
					t.Errorf("writing guidance missing %q:\n%s", want, prompt)
				}
			}
		})
	}
}

func TestBuildSystemPromptAlwaysIncludesTaskAndSkillGuidance(t *testing.T) {
	for _, custom := range []string{"", "Custom identity"} {
		t.Run(custom, func(t *testing.T) {
			prompt := BuildSystemPrompt(SystemPromptOpts{
				Custom: custom,
				Append: []string{"Workspace context", "Available skills"},
			})
			previous := strings.Index(prompt, "Available skills")
			for _, guidance := range []string{taskExecutionGuidance, skillPriorityGuidance, writingGuidance} {
				index := strings.Index(prompt, guidance)
				if index <= previous {
					t.Fatalf("shared guidance must follow appended context in task/skill/writing order")
				}
				previous = index
			}
			for _, want := range []string{
				"Treat requests such as \"can you\" as instructions to do the requested work",
				"do not stop at a plan or an offer to continue",
				"Respect requests for explanation, planning, or review without making unsolicited changes",
				"Ask only when a missing decision blocks safe, correct progress",
				"Preserve tool permissions, required confirmations, and explicit approval requirements",
				"Explicit user instructions take precedence over skill guidelines",
				"Skills do not grant permissions or override system or developer constraints",
				"identify its source and the relevant instruction",
			} {
				if count := strings.Count(prompt, want); count != 1 {
					t.Errorf("guidance %q count = %d, want 1", want, count)
				}
			}
		})
	}
}

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
