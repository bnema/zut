package tools_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/sdk"
)

func TestEnvIsolationSDKBoundary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell syntax")
	}
	t.Setenv("ZUT_SDK_ENV_TEST", "sdk-captured")
	out := runSDKBash(t, t.TempDir(), `printf '%s' "$ZUT_SDK_ENV_TEST"`)
	if !strings.Contains(out, "sdk-captured") {
		t.Fatalf("SDK bash tool did not receive captured environment: %q", out)
	}
}

func TestCWDIsolationSDKDefaultCWD(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell syntax")
	}
	for _, cwd := range []string{"", "."} {
		t.Run("cwd="+cwd, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Chdir(dir)
			out := runSDKBash(t, cwd, "pwd -P")
			if !strings.Contains(out, "\n"+dir+"\n") {
				t.Fatalf("SDK cwd %q ran in %q, want %q", cwd, out, dir)
			}
		})
	}
}

// runSDKBash drives one bash tool call through the public SDK against a
// synthetic OpenAI-compatible server and returns the tool output text.
func runSDKBash(t *testing.T, cwd, command string) string {
	t.Helper()
	call, err := json.Marshal(map[string]any{"command": command, "timeout": 5})
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "finish_reason": "tool_calls", "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "bash-1", "type": "function", "function": map[string]any{"name": "bash", "arguments": string(call)}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			io.WriteString(w, "data: "+string(chunk)+"\n\ndata: [DONE]\n\n")
		} else {
			io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}
	}))
	defer server.Close()
	rt, err := sdk.New(sdk.Config{Provider: "openai", Model: "gpt-4o", APIKey: "synthetic-test-key", BaseURL: server.URL + "/v1", CWD: cwd, Tools: []string{"bash"}})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events, err := rt.Prompt(ctx, "run the bash tool", nil)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	seen := false
	for event := range events {
		if event.Type == "error" {
			t.Fatal(event.Error)
		}
		if event.Type == "tool_result" {
			if event.IsError {
				t.Fatalf("tool failed: %+v", event)
			}
			seen = true
			for _, block := range event.Result {
				out.WriteString(block.Text)
			}
		}
	}
	if !seen {
		t.Fatal("no tool_result event")
	}
	return out.String()
}
