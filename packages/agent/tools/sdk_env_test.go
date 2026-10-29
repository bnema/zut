package tools_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"bash-1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"printf '%s' \\\"$ZUT_SDK_ENV_TEST\\\"\",\"timeout\":5}"}}]},"finish_reason":"tool_calls"}]}`+"\n\ndata: [DONE]\n\n")
		} else {
			io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}
	}))
	defer server.Close()
	rt, err := sdk.New(sdk.Config{Provider: "openai", Model: "gpt-4o", APIKey: "synthetic-test-key", BaseURL: server.URL + "/v1", CWD: t.TempDir(), Tools: []string{"bash"}})
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
	found := false
	for event := range events {
		if event.Type == "error" {
			t.Fatal(event.Error)
		}
		if event.Type == "tool_result" {
			if event.IsError {
				t.Fatalf("tool failed: %+v", event)
			}
			for _, block := range event.Result {
				if strings.Contains(block.Text, "sdk-captured") {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("SDK bash tool did not receive captured environment")
	}
}
