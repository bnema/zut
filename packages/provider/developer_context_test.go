package provider

import "testing"

func TestSystemWithDeveloperContextSeparatesStableAndDynamicText(t *testing.T) {
	stable, dynamic, messages := systemWithDeveloperContext(Request{
		System:        "stable instructions",
		SystemContext: "legacy host context",
		Messages: []Message{
			{Role: RoleDeveloper, Content: []Content{TextBlock{Text: "dynamic host context"}}},
			{Role: RoleUser, Content: []Content{TextBlock{Text: "user task"}}},
		},
	})
	if stable != "stable instructions" {
		t.Fatalf("stable = %q", stable)
	}
	if dynamic != "legacy host context\n\ndynamic host context" {
		t.Fatalf("dynamic = %q", dynamic)
	}
	if len(messages) != 1 || messages[0].Role != RoleUser {
		t.Fatalf("messages = %#v, want only the user task", messages)
	}
}

func TestAnthropicDeveloperContextFollowsStableCacheBoundary(t *testing.T) {
	req, err := (&anthropicClient{}).buildRequest(Request{
		Model:  "claude-sonnet-4-5",
		System: "stable instructions",
		Messages: []Message{
			{Role: RoleDeveloper, Content: []Content{TextBlock{Text: "dynamic host context"}}},
			{Role: RoleUser, Content: []Content{TextBlock{Text: "user task"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(req.System) != 2 || req.System[0].Text != "stable instructions" || req.System[0].CacheControl == nil || req.System[1].Text != "dynamic host context" || req.System[1].CacheControl != nil {
		t.Fatalf("system blocks = %#v", req.System)
	}
}

func TestOpenAICompatUsesSystemRoleForDeepSeekDeveloperContext(t *testing.T) {
	tests := []struct {
		name   string
		client *openaiClient
		model  string
		want   string
	}{
		{name: "direct DeepSeek", client: NewDeepSeek("key", "").(*openaiClient), model: "deepseek-v4-flash", want: "system"},
		{name: "OpenCode Go DeepSeek", client: NewOpenAICompat(ProviderOpenCodeGo, "key", "https://example.test/v1", "").(*openaiClient), model: "deepseek/deepseek-v4.1-flash", want: "system"},
		{name: "OpenCode Go other model", client: NewOpenAICompat(ProviderOpenCodeGo, "key", "https://example.test/v1", "").(*openaiClient), model: "other/model", want: "developer"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req, err := test.client.buildRequest(Request{
				Model: test.model,
				Messages: []Message{
					{Role: RoleDeveloper, Content: []Content{TextBlock{Text: "dynamic host context"}}},
					{Role: RoleUser, Content: []Content{TextBlock{Text: "user task"}}},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(req.Messages) != 2 || req.Messages[0].Role != test.want || req.Messages[0].Content != "dynamic host context" {
				t.Fatalf("messages = %#v, want developer context as %q", req.Messages, test.want)
			}
		})
	}
}

func TestBedrockDeveloperContextFollowsStableCacheBoundary(t *testing.T) {
	req, err := (&bedrockClient{region: "us-east-1"}).buildRequest(Request{
		Model:  "anthropic.claude-sonnet-4-5-20250929-v1:0",
		System: "stable instructions",
		Messages: []Message{
			{Role: RoleDeveloper, Content: []Content{TextBlock{Text: "dynamic host context"}}},
			{Role: RoleUser, Content: []Content{TextBlock{Text: "user task"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(req.System) != 3 || req.System[0]["text"] != "stable instructions" || req.System[1]["cachePoint"] == nil || req.System[2]["text"] != "dynamic host context" {
		t.Fatalf("system blocks = %#v", req.System)
	}
}
