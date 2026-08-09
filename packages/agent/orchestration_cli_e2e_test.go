package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var (
	orchestrationBuildOnce   sync.Once
	orchestrationBuildDir    string
	orchestrationBuildBinary string
	orchestrationBuildOutput []byte
	orchestrationBuildErr    error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if orchestrationBuildDir != "" {
		_ = os.RemoveAll(orchestrationBuildDir)
	}
	os.Exit(code)
}

func TestOrchestratedCLITwoDelegationWavesAcrossHeadlessModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal and subprocess semantics differ on Windows")
	}
	if testing.Short() {
		t.Skip("skip CLI orchestration end-to-end test in -short mode")
	}
	binary := buildOrchestrationCLI(t)
	for _, mode := range []string{"print", "stream", "json"} {
		t.Run(mode, func(t *testing.T) {
			server := newTwoWaveProvider(t)
			defer server.Close()
			home := t.TempDir()
			args := []string{"--orchestrate", "--provider", "openai", "--model", "gpt-5", "--base-url", server.URL(), "--api-key", "test-key"}
			switch mode {
			case "print":
				args = append(args, "--print")
			case "stream":
				args = append(args, "--stream")
			case "json":
				args = append(args, "--json")
			}
			args = append(args, "delegate two waves")
			cmd := exec.Command(binary, args...)
			cmd.Env = orchestrationCLIEnv(binary, home)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			output := append(append([]byte(nil), stdout.Bytes()...), stderr.Bytes()...)
			if err != nil {
				t.Fatalf("zut %s: %v\n%s", mode, err, output)
			}
			if !bytes.Contains(output, []byte("final synthesis")) {
				t.Fatalf("%s output omitted final synthesis: %s", mode, output)
			}
			if server.ParentCalls() != 5 || server.WorkerCalls() < 2 {
				t.Fatalf("%s calls = parent %d, worker %d; want two delegation waves and two workers", mode, server.ParentCalls(), server.WorkerCalls())
			}
			if mode == "json" {
				events := assertJSONL(t, stdout.Bytes())
				assertJSONOrchestrationWaves(t, events)
			}
			if mode == "stream" && !bytes.Contains(stdout.Bytes(), []byte("wave one synthesis")) {
				t.Fatalf("stream output omitted first parent turn: %s", output)
			}
			if mode == "stream" && bytes.Contains(stdout.Bytes(), []byte("[auto-subagents update]")) {
				t.Fatalf("stream stdout leaked internal completion prompt: %s", stdout.Bytes())
			}
			if mode == "print" && bytes.Contains(stdout.Bytes(), []byte("wave one synthesis")) {
				t.Fatalf("print output leaked intermediate parent turn: %s", output)
			}
		})
	}
}

func TestOrchestratedCLICancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal and subprocess semantics differ on Windows")
	}
	if testing.Short() {
		t.Skip("skip CLI orchestration end-to-end test in -short mode")
	}
	binary := buildOrchestrationCLI(t)
	server, workerReady, releaseWorker := newCancellationProvider()
	defer server.Close()
	defer close(releaseWorker)

	cmd := exec.Command(binary, "--orchestrate", "--json", "--provider", "openai", "--model", "gpt-5", "--base-url", server.URL, "--api-key", "test-key", "cancel")
	cmd.Env = orchestrationCLIEnv(binary, t.TempDir())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForWorker := time.NewTimer(10 * time.Second)
	select {
	case <-workerReady:
		if !waitForWorker.Stop() {
			<-waitForWorker.C
		}
	case <-waitForWorker.C:
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("worker did not reach the blocking provider request")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send cancellation: %v", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case err := <-wait:
		if err == nil {
			t.Fatal("canceled orchestration unexpectedly succeeded")
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("canceled orchestration did not exit")
	}

	events := assertJSONL(t, stdout.Bytes())
	errorCount := 0
	for _, event := range events {
		if event["type"] == "error" {
			errorCount++
		}
	}
	if errorCount != 1 {
		t.Fatalf("cancellation error objects = %d, want one; stdout=%s stderr=%s", errorCount, stdout.Bytes(), stderr.Bytes())
	}
}

func TestOrchestratedCLIParentFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal and subprocess semantics differ on Windows")
	}
	if testing.Short() {
		t.Skip("skip CLI orchestration end-to-end test in -short mode")
	}
	binary := buildOrchestrationCLI(t)

	t.Run("failure-jsonl", func(t *testing.T) {
		server := newFailingProvider()
		defer server.Close()
		cmd := exec.Command(binary, "--orchestrate", "--json", "--provider", "openai", "--model", "gpt-5", "--base-url", server.URL, "--api-key", "test-key", "fail")
		cmd.Env = orchestrationCLIEnv(binary, t.TempDir())
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err == nil {
			t.Fatal("failing orchestration unexpectedly succeeded")
		}
		events := assertJSONL(t, stdout.Bytes())
		errorCount := 0
		for _, event := range events {
			if event["type"] == "error" {
				errorCount++
			}
		}
		if errorCount != 1 {
			output := append(append([]byte(nil), stdout.Bytes()...), stderr.Bytes()...)
			t.Fatalf("error objects = %d, want one: %s", errorCount, output)
		}
	})
}

type twoWaveProvider struct {
	t           *testing.T
	server      *httptest.Server
	mu          sync.Mutex
	parentCalls int
	workerCalls int
}

func newTwoWaveProvider(t *testing.T) *twoWaveProvider {
	p := &twoWaveProvider{t: t}
	p.server = httptest.NewServer(http.HandlerFunc(p.serveHTTP))
	return p
}

func (p *twoWaveProvider) URL() string { return p.server.URL }
func (p *twoWaveProvider) Close()      { p.server.Close() }

func (p *twoWaveProvider) ParentCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.parentCalls
}

func (p *twoWaveProvider) WorkerCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.workerCalls
}

func (p *twoWaveProvider) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.t.Errorf("read provider request: %v", err)
		return
	}
	p.mu.Lock()
	isParent := requestIsParent(body)
	if isParent {
		p.parentCalls++
		call := p.parentCalls
		p.mu.Unlock()
		switch call {
		case 1:
			writeToolCall(w, "wave-one", "wave one")
		case 2:
			writeText(w, "wave one synthesis")
		case 3:
			writeToolCall(w, "wave-two", "wave two")
		default:
			writeText(w, "final synthesis")
		}
		return
	}
	p.workerCalls++
	worker := p.workerCalls
	p.mu.Unlock()
	writeText(w, fmt.Sprintf("worker result %d", worker))
}

func newCancellationProvider() (*httptest.Server, <-chan struct{}, chan<- struct{}) {
	workerReady := make(chan struct{})
	releaseWorker := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		parent := requests == 1
		mu.Unlock()
		if parent {
			writeToolCall(w, "cancel-worker", "long running worker")
			return
		}
		once.Do(func() { close(workerReady) })
		select {
		case <-r.Context().Done():
		case <-releaseWorker:
		}
	}))
	return server, workerReady, releaseWorker
}

func newFailingProvider() *httptest.Server {
	var mu sync.Mutex
	parentCalls := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if requestIsParent(body) {
			mu.Lock()
			parentCalls++
			call := parentCalls
			mu.Unlock()
			if call == 1 {
				writeToolCall(w, "failure-worker", "failure wave")
				return
			}
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":{"message":"synthetic provider failure","type":"server_error"}}`)
			return
		}
		writeText(w, "worker completed before parent failure")
	}))
}

func requestIsParent(body []byte) bool {
	var request struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &request) != nil {
		return false
	}
	for _, message := range request.Messages {
		if message.Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(message.Content, &text) == nil {
			if text == "delegate two waves" || text == "fail" || text == "cancel" {
				return true
			}
		}
		if bytes.Contains(message.Content, []byte(`delegate two waves`)) || bytes.Contains(message.Content, []byte(`"text":"fail"`)) || bytes.Contains(message.Content, []byte(`"text":"cancel"`)) {
			return true
		}
	}
	return false
}

func writeToolCall(w http.ResponseWriter, id, task string) {
	w.Header().Set("content-type", "text/event-stream")
	arguments, _ := json.Marshal(map[string]string{"task": task})
	chunk := map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": id, "type": "function", "function": map[string]string{"name": "subagent_spawn", "arguments": string(arguments)}}}}, "finish_reason": "tool_calls"}},
		"usage":   map[string]int{"prompt_tokens": 8, "completion_tokens": 2},
	}
	data, _ := json.Marshal(chunk)
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
}

func writeText(w http.ResponseWriter, text string) {
	w.Header().Set("content-type", "text/event-stream")
	chunk := map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": text}, "finish_reason": "stop"}},
		"usage":   map[string]int{"prompt_tokens": 8, "completion_tokens": 2},
	}
	data, _ := json.Marshal(chunk)
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
}

func assertJSONL(t *testing.T, output []byte) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "zut:") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid JSONL line %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

func assertJSONOrchestrationWaves(t *testing.T, events []map[string]any) {
	t.Helper()
	var userMessages []string
	seenTypes := make(map[string]bool)
	for _, event := range events {
		if typ, ok := event["type"].(string); ok {
			seenTypes[typ] = true
		}
		if event["type"] != "user_message" {
			continue
		}
		content, ok := event["content"].([]any)
		if !ok {
			t.Fatalf("user_message content = %T, want JSON array", event["content"])
		}
		var text strings.Builder
		for _, block := range content {
			object, ok := block.(map[string]any)
			if !ok || object["type"] != "text" {
				continue
			}
			if value, ok := object["text"].(string); ok {
				text.WriteString(value)
			}
		}
		userMessages = append(userMessages, text.String())
	}
	if len(userMessages) < 3 {
		t.Fatalf("user messages = %d, want initial prompt and two parent completion turns", len(userMessages))
	}
	completionMessages := 0
	for _, message := range userMessages {
		if strings.HasPrefix(message, "[auto-subagents update]") {
			completionMessages++
		}
	}
	if completionMessages != 2 {
		t.Fatalf("completion user messages = %d, want two: %#v", completionMessages, userMessages)
	}
	for _, typ := range []string{"assistant_message", "tool_call", "tool_execution_started", "tool_result", "usage", "turn_end"} {
		if !seenTypes[typ] {
			t.Fatalf("orchestrated JSONL omitted %s event; seen types: %v", typ, seenTypes)
		}
	}
	for _, event := range events {
		if event["type"] == "error" {
			t.Fatalf("successful two-wave orchestration emitted an error event: %#v", event)
		}
	}
}

func orchestrationCLIEnv(binary, home string) []string {
	// Restrict PATH so spawned workers resolve the same freshly built test
	// binary instead of any installed zut executable on the host.
	return append(os.Environ(), "ZUT_HOME="+home, "PATH="+filepath.Dir(binary))
}

func buildOrchestrationCLI(t *testing.T) string {
	t.Helper()
	orchestrationBuildOnce.Do(func() {
		orchestrationBuildDir, orchestrationBuildErr = os.MkdirTemp("", "zut-orchestration-cli-")
		if orchestrationBuildErr != nil {
			return
		}
		orchestrationBuildBinary = filepath.Join(orchestrationBuildDir, "zut")
		root := filepath.Join("..", "..")
		cmd := exec.Command("go", "build", "-o", orchestrationBuildBinary, "./cmd/zut")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		orchestrationBuildOutput, orchestrationBuildErr = cmd.CombinedOutput()
	})
	if orchestrationBuildErr != nil {
		t.Fatalf("build zut: %v\n%s", orchestrationBuildErr, orchestrationBuildOutput)
	}
	return orchestrationBuildBinary
}
