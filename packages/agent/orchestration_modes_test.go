package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestRunOrchestratedStreamUsesStreamRenderer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	server := orchestratedModeTestServer("stream answer")
	defer server.Close()

	output, runErr := captureTestStdout(t, func() error {
		return runOrchestratedStreamMode(context.Background(), Args{
			Mode:        ModeStream,
			Orchestrate: true,
			Provider:    "openai",
			Model:       "gpt-5",
			BaseURL:     server.URL,
			APIKey:      "test-key",
			Prompt:      "say hello",
		}, "test")
	})
	if runErr != nil {
		t.Fatalf("runOrchestratedStreamMode error: %v", runErr)
	}
	if got := output; got != "stream answer\n" {
		t.Fatalf("stdout = %q, want one rendered answer newline", got)
	}
}

func TestRunOrchestratedJSONRetainsParentEventsAsJSONL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	server := orchestratedModeTestServer("json answer")
	defer server.Close()

	output, runErr := captureTestStdout(t, func() error {
		return runOrchestratedJSONMode(context.Background(), Args{
			Mode:        ModeJSON,
			Orchestrate: true,
			Provider:    "openai",
			Model:       "gpt-5",
			BaseURL:     server.URL,
			APIKey:      "test-key",
			Prompt:      "say hello",
		}, "test")
	})
	if runErr != nil {
		t.Fatalf("runOrchestratedJSONMode error: %v", runErr)
	}

	seen := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("invalid JSONL line %q: %v", scanner.Text(), err)
		}
		if typ, ok := event["type"].(string); ok {
			seen[typ] = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"user_message", "assistant_message", "turn_end"} {
		if !seen[typ] {
			t.Fatalf("JSONL omitted %s event: %s", typ, output)
		}
	}
}

func captureTestStdout(t *testing.T, run func() error) (string, error) {
	t.Helper()
	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		return "", err
	}
	os.Stdout = writer
	t.Cleanup(func() {
		os.Stdout = oldStdout
		_ = writer.Close()
		_ = reader.Close()
	})
	readDone := make(chan struct {
		output []byte
		err    error
	}, 1)
	go func() {
		output, readErr := io.ReadAll(reader)
		readDone <- struct {
			output []byte
			err    error
		}{output: output, err: readErr}
	}()

	runErr := run()
	_ = writer.Close()
	os.Stdout = oldStdout
	result := <-readDone
	_ = reader.Close()
	return string(result.output), errors.Join(runErr, result.err)
}

func orchestratedModeTestServer(answer string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\""+answer+"\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
}
