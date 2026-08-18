package provider

import "testing"

func TestSystemWithDeveloperContextPromotesOnlyDeveloperText(t *testing.T) {
	system, messages := systemWithDeveloperContext(Request{
		System: "stable instructions",
		Messages: []Message{
			{Role: RoleDeveloper, Content: []Content{TextBlock{Text: "dynamic host context"}}},
			{Role: RoleUser, Content: []Content{TextBlock{Text: "user task"}}},
		},
	})
	if system != "stable instructions\n\ndynamic host context" {
		t.Fatalf("system = %q", system)
	}
	if len(messages) != 1 || messages[0].Role != RoleUser {
		t.Fatalf("messages = %#v, want only the user task", messages)
	}
}

func TestSystemWithDeveloperContextKeepsLegacySystemContext(t *testing.T) {
	system, messages := systemWithDeveloperContext(Request{
		System:        "stable instructions",
		SystemContext: "legacy host context",
		Messages: []Message{
			{Role: RoleDeveloper, Content: []Content{TextBlock{Text: "dynamic host context"}}},
		},
	})
	if system != "stable instructions\n\nlegacy host context\n\ndynamic host context" {
		t.Fatalf("system = %q", system)
	}
	if len(messages) != 0 {
		t.Fatalf("messages = %#v, want no developer message", messages)
	}
}
