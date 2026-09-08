package provider

import "testing"

func TestVertexReplaysOwnReasoning(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_API_KEY", "synthetic-key")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "test-project")
	client := NewVertex("", "").(*renamedClient)
	inner := client.inner.(*geminiClient)
	for _, origin := range []string{"google-vertex", "google"} {
		t.Run(origin, func(t *testing.T) {
			wire, _, err := inner.buildRequest(Request{Model: "gemini-2.5-pro", Messages: []Message{WithMessageOrigin(Message{Role: RoleAssistant, Content: []Content{ReasoningBlock{Summary: "summary", Encrypted: "opaque"}, TextBlock{Text: "answer", ThoughtSignature: "signature"}}}, origin, "gemini-2.5-pro")}})
			if err != nil {
				t.Fatal(err)
			}
			parts := wire.Contents[0].Parts
			if origin == "google-vertex" {
				if len(parts) != 2 || parts[0].ThoughtSignature != "opaque" || parts[1].ThoughtSignature != "signature" {
					t.Fatalf("own metadata lost: %+v", parts)
				}
			} else if len(parts) != 1 || parts[0].ThoughtSignature != "" {
				t.Fatalf("foreign metadata replayed: %+v", parts)
			}
		})
	}
}
