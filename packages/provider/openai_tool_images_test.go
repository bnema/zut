package provider

import "testing"

func TestOpenAICompatToolResultImagesFollowTextToolMessages(t *testing.T) {
	c := NewOpenAICompat("commandcode", "token", "https://example.test/v1", "").(*openaiClient)

	wire, err := c.buildRequest(Request{
		Model: "vision-model",
		Messages: []Message{
			{Role: RoleUser, Content: []Content{TextBlock{Text: "read both images"}}},
			{Role: RoleAssistant, Content: []Content{
				ToolCallBlock{ID: "call-1", Name: "read"},
				ToolCallBlock{ID: "call-2", Name: "read"},
			}},
			{Role: RoleTool, Content: []Content{
				ToolResultBlock{CallID: "call-1", Content: []Content{
					ImageBlock{MimeType: "image/png", Data: []byte("first")},
				}},
			}},
			{Role: RoleTool, Content: []Content{
				ToolResultBlock{CallID: "call-2", Content: []Content{
					TextBlock{Text: "second image"},
					ImageBlock{MimeType: "image/jpeg", Data: []byte("second")},
				}},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	wantRoles := []string{"user", "assistant", "tool", "tool", "user"}
	if len(wire.Messages) != len(wantRoles) {
		t.Fatalf("message count = %d, want %d: %+v", len(wire.Messages), len(wantRoles), wire.Messages)
	}
	for i, want := range wantRoles {
		if got := wire.Messages[i].Role; got != want {
			t.Fatalf("message %d role = %q, want %q", i, got, want)
		}
	}
	if got, ok := wire.Messages[2].Content.(string); !ok || got != "(see attached image)" {
		t.Fatalf("first tool content = %#v, want text placeholder", wire.Messages[2].Content)
	}
	if got, ok := wire.Messages[3].Content.(string); !ok || got != "second image" {
		t.Fatalf("second tool content = %#v, want text", wire.Messages[3].Content)
	}

	parts, ok := wire.Messages[4].Content.([]interface{})
	if !ok {
		t.Fatalf("image message content type = %T, want []interface{}", wire.Messages[4].Content)
	}
	if len(parts) != 3 {
		t.Fatalf("image message parts = %d, want prefix and two images", len(parts))
	}
	if text, ok := parts[0].(oaiContentText); !ok || text.Text != "Tool output included the following image content:" {
		t.Fatalf("image message prefix = %#v", parts[0])
	}
	for i, part := range parts[1:] {
		image, ok := part.(oaiContentImage)
		if !ok || image.Type != "image_url" {
			t.Fatalf("image part %d = %#v", i, part)
		}
	}
}

func TestOpenAICompatDoesNotDuplicatePersistedToolImageMirror(t *testing.T) {
	c := NewOpenAICompat("custom", "token", "https://example.test/v1", "").(*openaiClient)
	image := ImageBlock{MimeType: "image/png", Data: []byte("image")}

	wire, err := c.buildRequest(Request{
		Model: "vision-model",
		Messages: []Message{
			{Role: RoleAssistant, Content: []Content{
				ToolCallBlock{ID: "call-1", Name: "read"},
			}},
			{Role: RoleTool, Content: []Content{
				ToolResultBlock{CallID: "call-1", Content: []Content{image}},
			}},
			{Role: RoleUser, Content: []Content{
				TextBlock{Text: "Tool output included the following image content:"},
				image,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	wantRoles := []string{"assistant", "tool", "user"}
	if len(wire.Messages) != len(wantRoles) {
		t.Fatalf("message count = %d, want %d: %+v", len(wire.Messages), len(wantRoles), wire.Messages)
	}
	for i, want := range wantRoles {
		if got := wire.Messages[i].Role; got != want {
			t.Fatalf("message %d role = %q, want %q", i, got, want)
		}
	}
}

func TestOpenAICompatToolResultErrorText(t *testing.T) {
	c := NewOpenAICompat("custom", "token", "https://example.test/v1", "").(*openaiClient)

	wire, err := c.buildRequest(Request{
		Model: "vision-model",
		Messages: []Message{
			{Role: RoleAssistant, Content: []Content{
				ToolCallBlock{ID: "call-1", Name: "read"},
			}},
			{Role: RoleTool, Content: []Content{
				ToolResultBlock{CallID: "call-1", IsError: true, Content: []Content{
					TextBlock{Text: "boom"},
				}},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(wire.Messages) != 2 {
		t.Fatalf("message count = %d, want 2: %+v", len(wire.Messages), len(wire.Messages))
	}
	if got, ok := wire.Messages[1].Content.(string); !ok || got != "boom [error]" {
		t.Fatalf("tool content = %#v, want error text", wire.Messages[1].Content)
	}
}

func TestOpenAICompatDeepSeekDropsToolImages(t *testing.T) {
	c := NewOpenAICompat("deepseek", "token", "https://example.test/v1", "").(*openaiClient)

	wire, err := c.buildRequest(Request{
		Model: "deepseek-chat",
		Messages: []Message{
			{Role: RoleAssistant, Content: []Content{
				ToolCallBlock{ID: "call-1", Name: "read"},
			}},
			{Role: RoleTool, Content: []Content{
				ToolResultBlock{CallID: "call-1", Content: []Content{
					TextBlock{Text: "caption"},
					ImageBlock{MimeType: "image/png", Data: []byte("image")},
				}},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(wire.Messages) != 2 {
		t.Fatalf("message count = %d, want 2 (no image mirror): %+v", len(wire.Messages), len(wire.Messages))
	}
	if got, ok := wire.Messages[1].Content.(string); !ok || got != "caption" {
		t.Fatalf("tool content = %#v, want text only", wire.Messages[1].Content)
	}
}

func TestOpenAICompatOpenCodeGoDeepSeekDropsToolImages(t *testing.T) {
	c := NewOpenAICompat(ProviderOpenCodeGo, "token", "https://example.test/v1", "").(*openaiClient)

	wire, err := c.buildRequest(Request{
		Model: "deepseek/deepseek-chat",
		Messages: []Message{
			{Role: RoleAssistant, Content: []Content{
				ToolCallBlock{ID: "call-1", Name: "read"},
			}},
			{Role: RoleTool, Content: []Content{
				ToolResultBlock{CallID: "call-1", Content: []Content{
					TextBlock{Text: "caption"},
					ImageBlock{MimeType: "image/png", Data: []byte("image")},
				}},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(wire.Messages) != 2 {
		t.Fatalf("message count = %d, want 2 (no image mirror): %+v", len(wire.Messages), len(wire.Messages))
	}
	if got, ok := wire.Messages[1].Content.(string); !ok || got != "caption" {
		t.Fatalf("tool content = %#v, want text only", wire.Messages[1].Content)
	}
}

func TestOpenAICompatUserImagesPassThrough(t *testing.T) {
	c := NewOpenAICompat("custom", "token", "https://example.test/v1", "").(*openaiClient)
	image := ImageBlock{MimeType: "image/png", Data: []byte("image")}

	wire, err := c.buildRequest(Request{
		Model: "vision-model",
		Messages: []Message{
			{Role: RoleUser, Content: []Content{
				TextBlock{Text: "look"},
				image,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(wire.Messages) != 1 {
		t.Fatalf("message count = %d, want 1: %+v", len(wire.Messages), len(wire.Messages))
	}
	parts, ok := wire.Messages[0].Content.([]interface{})
	if !ok || len(parts) != 2 {
		t.Fatalf("user content = %#v, want text+image", wire.Messages[0].Content)
	}
	if _, ok := parts[1].(oaiContentImage); !ok {
		t.Fatalf("second part = %#v, want image", parts[1])
	}
}
