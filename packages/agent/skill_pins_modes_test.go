package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestHeadlessModesPreloadSkillPinsAndPersistOnce(t *testing.T) {
	modes := []struct {
		name string
		run  func(context.Context, Args, string) error
	}{
		{"print", runPrintMode}, {"stream", runStreamMode}, {"json", runJSONMode},
	}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			home, cwd := t.TempDir(), t.TempDir()
			t.Setenv("ZUT_HOME", home)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("ZUT_AGENT_SKILLS", "")
			t.Setenv("ZUT_AGENT_PROFILES", "")
			dir := filepath.Join(cwd, ".zut", "skills", "review")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: review\ndescription: Review things.\n---\nPINNED_INSTRUCTIONS\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := toggleSkillPin(cwd, "review", true); err != nil {
				t.Fatal(err)
			}
			requests := make(chan string, 3)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Messages []struct {
						Role    string          `json:"role"`
						Content json.RawMessage `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				for index := len(request.Messages) - 1; index >= 0; index-- {
					if request.Messages[index].Role == "user" {
						requests <- string(request.Messages[index].Content)
						break
					}
				}
				w.Header().Set("content-type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			stdin, err := os.Open(os.DevNull)
			if err != nil {
				t.Fatal(err)
			}
			oldStdin := os.Stdin
			os.Stdin = stdin
			t.Cleanup(func() { os.Stdin = oldStdin; _ = stdin.Close() })
			args := Args{CWD: cwd, Provider: "openai", Model: "gpt-5", APIKey: "test-key", BaseURL: server.URL, Prompt: "first prompt", NoExt: true, WithSkills: true}
			if _, err := captureTestStdout(t, func() error { return mode.run(context.Background(), args, "test") }); err != nil {
				t.Fatal(err)
			}
			select {
			case prompt := <-requests:
				if !strings.Contains(prompt, "PINNED_INSTRUCTIONS") {
					t.Fatalf("provider did not receive pinned body: %s", prompt)
				}
			default:
				t.Fatal("no provider request")
			}
			path := core.LatestSession(home, cwd)
			if path == "" {
				t.Fatal("no persisted session")
			}
			args.Continue, args.Prompt = true, "second prompt"
			if _, err := captureTestStdout(t, func() error { return mode.run(context.Background(), args, "test") }); err != nil {
				t.Fatal(err)
			}
			select {
			case prompt := <-requests:
				if strings.Contains(prompt, "PINNED_INSTRUCTIONS") {
					t.Fatalf("resumed prompt reloaded pins: %s", prompt)
				}
			default:
				t.Fatal("no resumed provider request")
			}
			session, messages, err := core.OpenSession(path)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			count := 0
			for _, message := range messages {
				if message.Role != provider.RoleUser {
					continue
				}
				for _, content := range message.Content {
					if text, ok := content.(provider.TextBlock); ok {
						count += strings.Count(text.Text, "PINNED_INSTRUCTIONS")
					}
				}
			}
			if count != 1 {
				t.Fatalf("persisted pinned bodies=%d, want 1", count)
			}
		})
	}
}
