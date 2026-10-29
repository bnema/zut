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
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// Headless parity goldens live in testdata/headless_parity/<scenario>.<mode>.
// They were recorded from the in-process headless modes (before the daemon
// client cutover) by running, in that tree:
//
//	ZUT_PARITY_RECORD=1 go test ./packages/agent -run TestHeadlessParityRecord
//
// Normalisation applied to both recorded and replayed stdout (see
// normaliseParityOutput): message "time" values, the scenario cwd, and
// resident subagent IDs. Everything else must match byte for byte.
const parityGoldenDir = "testdata/headless_parity"

type parityScenario struct {
	name   string
	prompt string
	extra  []string
	// handler answers one provider request; call counts parent requests.
	handler func(p *parityProvider, w http.ResponseWriter, body []byte)
	modes   []string
	// rpcInput is the stdin script for the rpc mode.
	rpcInput string
}

type parityProvider struct {
	mu      sync.Mutex
	parent  int
	workers int
}

func (p *parityProvider) nextParent() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.parent++
	return p.parent
}

func (p *parityProvider) nextWorker() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.workers++
	return p.workers
}

func parityToolCall(w http.ResponseWriter, id, name string, args any) {
	w.Header().Set("content-type", "text/event-stream")
	raw, _ := json.Marshal(args)
	chunk := map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": id, "type": "function", "function": map[string]string{"name": name, "arguments": string(raw)}}}}, "finish_reason": "tool_calls"}},
		"usage":   map[string]int{"prompt_tokens": 8, "completion_tokens": 2},
	}
	data, _ := json.Marshal(chunk)
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
}

func parityScenarios() []parityScenario {
	return []parityScenario{
		{
			name:   "text",
			prompt: "say hello",
			handler: func(p *parityProvider, w http.ResponseWriter, _ []byte) {
				p.nextParent()
				writeText(w, "hello from parity")
			},
			modes:    []string{"print", "stream", "json", "rpc"},
			rpcInput: `{"id":"1","type":"hello"}` + "\n" + `{"id":"2","type":"ping"}` + "\n" + `{"id":"3","type":"prompt","message":"say hello"}` + "\n",
		},
		{
			name:   "tool",
			prompt: "run a command",
			handler: func(p *parityProvider, w http.ResponseWriter, _ []byte) {
				if p.nextParent() == 1 {
					parityToolCall(w, "call-parity", "bash", map[string]any{"command": "echo parity-tool", "timeout": 10})
					return
				}
				writeText(w, "tool finished")
			},
			modes:    []string{"print", "stream", "json", "rpc"},
			rpcInput: `{"id":"1","type":"prompt","message":"run a command"}` + "\n",
		},
		{
			name:   "orchestrate",
			prompt: "delegate two waves",
			extra:  []string{"--orchestrate"},
			handler: func(p *parityProvider, w http.ResponseWriter, body []byte) {
				if !requestIsParent(body) {
					writeText(w, fmt.Sprintf("worker result %d", p.nextWorker()))
					return
				}
				switch p.nextParent() {
				case 1:
					writeToolCall(w, "wave-one", "wave one")
				case 2:
					writeText(w, "wave one synthesis")
				case 3:
					writeToolCall(w, "wave-two", "wave two")
				default:
					writeText(w, "final synthesis")
				}
			},
			modes: []string{"print", "stream", "json"},
		},
	}
}

func (s parityScenario) server(t *testing.T) *httptest.Server {
	t.Helper()
	p := &parityProvider{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Daemon sessions generate a title from the first prompt with a
		// separate request (core.GenerateSessionTitle); answer it without
		// consuming the scripted transcript replies.
		if bytes.Contains(body, []byte("You create concise titles")) {
			writeText(w, "parity title")
			return
		}
		s.handler(p, w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

// args returns the CLI args for mode (no binary name).
func (s parityScenario) args(mode, baseURL, cwd string) []string {
	out := append([]string{}, s.extra...)
	switch mode {
	case "print":
		out = append(out, "--print")
	case "stream":
		out = append(out, "--stream")
	case "json":
		out = append(out, "--json")
	case "rpc":
		out = append(out, "--rpc")
	}
	out = append(out, "--provider", "openai", "--model", "gpt-5", "--base-url", baseURL, "--api-key", "test-key",
		"--no-ext", "--no-mcp", "--no-lsp", "--no-context-files", "--cwd", cwd)
	if mode != "rpc" {
		out = append(out, s.prompt)
	}
	return out
}

var (
	parityTimePattern = regexp.MustCompile(`"time":"[^"]*"`)
	// Resident child IDs are random UUIDs.
	parityResidentPattern = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
)

func normaliseParityOutput(out, cwd string) string {
	out = parityTimePattern.ReplaceAllString(out, `"time":"<TIME>"`)
	if cwd != "" {
		out = strings.ReplaceAll(out, cwd, "<CWD>")
		if resolved, err := filepath.EvalSymlinks(cwd); err == nil && resolved != cwd {
			out = strings.ReplaceAll(out, resolved, "<CWD>")
		}
	}
	return parityResidentPattern.ReplaceAllString(out, "<RESIDENT>")
}

func parityGoldenPath(scenario, mode string) string {
	return filepath.Join(parityGoldenDir, scenario+"."+mode+".golden")
}

// TestHeadlessParityRecord records goldens from the built binary. It only
// runs with ZUT_PARITY_RECORD=1 and was run on the pre-cutover tree.
func TestHeadlessParityRecord(t *testing.T) {
	if os.Getenv("ZUT_PARITY_RECORD") != "1" {
		t.Skip("set ZUT_PARITY_RECORD=1 to record headless parity goldens")
	}
	if runtime.GOOS == "windows" {
		t.Skip("recording uses POSIX subprocess semantics")
	}
	binary := buildOrchestrationCLI(t)
	if err := os.MkdirAll(parityGoldenDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, sc := range parityScenarios() {
		for _, mode := range sc.modes {
			t.Run(sc.name+"/"+mode, func(t *testing.T) {
				server := sc.server(t)
				cwd := t.TempDir()
				home := t.TempDir()
				cmd := exec.Command(binary, sc.args(mode, server.URL, cwd)...)
				cmd.Env = append(orchestrationCLIEnv(t, binary, home), "HOME="+t.TempDir())
				if mode == "rpc" {
					cmd.Stdin = strings.NewReader(sc.rpcInput)
				}
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				if err := cmd.Run(); err != nil {
					t.Fatalf("zut %s %s: %v\n%s", sc.name, mode, err, stderr.Bytes())
				}
				got := normaliseParityOutput(stdout.String(), cwd)
				if err := os.WriteFile(parityGoldenPath(sc.name, mode), []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func readParityGolden(t *testing.T, scenario, mode string) string {
	t.Helper()
	raw, err := os.ReadFile(parityGoldenPath(scenario, mode))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	return string(raw)
}
