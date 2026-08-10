// stubchild is a minimal subagent-worker stand-in used by the runner
// end-to-end test. It speaks the daemon protocol the real zut
// binary will implement next:
//
//   - parses --subagent-worker <path>, --session <path>, --cwd <path>,
//     and an optional positional task,
//   - opens a unix-socket listener at the inbox path so the
//     supervisor's Inbox can dial through,
//   - emits well-formed JSONL events on stdout that the supervisor
//     mirrors into the durable event log,
//   - reads one line per supervisor message and echoes it back as
//     a "user_message" event followed by a fake "assistant_message"
//     so the runner sees the dialogue happen.
//
// The runner test compiles this binary into a tempdir, points
// subagent's execRunner at it via Command, and asserts the events
// flow through correctly without needing the full zut model
// machinery.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
)

func main() {
	var (
		inboxPath       string
		sessionPath     string
		cwd             string
		webSearchPolicy string
	)
	flag.StringVar(&inboxPath, "subagent-worker", "", "inbox socket path")
	flag.StringVar(&sessionPath, "session", "", "session file path")
	flag.StringVar(&cwd, "cwd", "", "working directory")
	// Accepted only to keep the fixture aligned with the real child argv;
	// the stub does not construct a tool registry.
	flag.StringVar(&webSearchPolicy, "web-search-policy", "", "web search capability policy")
	flag.Parse()

	if inboxPath == "" {
		fmt.Fprintln(os.Stderr, "stubchild: --subagent-worker required")
		os.Exit(2)
	}

	emit := newEmitter()

	// Open the inbox listener BEFORE doing any work so the supervisor
	// can dial through as soon as it sees the agent_ready event. If we
	// processed the initial task first, the parent's send call
	// would race the stub's net.Listen and trip ErrNotReady.
	ln, err := net.Listen("unix", inboxPath)
	if err != nil {
		emit("error", map[string]any{"message": err.Error()})
		os.Exit(1)
	}
	defer os.Remove(inboxPath)

	emit("agent_ready", map[string]any{
		"inbox":   inboxPath,
		"session": sessionPath,
		"cwd":     cwd,
	})

	// Initial task lives in the positional. Process it as the first
	// user turn so the supervisor's "initial task" path is exercised.
	initialTurnOpen := false
	if task := flag.Arg(0); task != "" {
		if os.Getenv("ZUT_STUB_BLOCK_INITIAL") == "1" {
			initialTurnOpen = true
			emit("turn_start", map[string]any{"step": 0})
			emit("user_message", map[string]any{
				"content": []any{
					map[string]any{"type": "text", "text": task},
				},
			})
			emit("message.delta", map[string]any{"delta": "partial answer"})
		} else {
			runTurn(emit, task, 0)
		}
	}

	turn := 1
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		br := bufio.NewReader(c)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				msg := trimNL(line)
				command, parseErr := subagents.ParseCommand(msg)
				if parseErr == nil {
					switch command.Type {
					case subagents.CommandAgentShutdown:
						if initialTurnOpen {
							emit("turn_end", map[string]any{"stop": "cancelled"})
							initialTurnOpen = false
						}
						emit("agent_stopped", map[string]any{"reason": "shutdown"})
						_ = c.Close()
						return
					case subagents.CommandTurnCancel:
						if initialTurnOpen {
							initialTurnOpen = false
						}
						emit("turn_end", map[string]any{"stop": "cancelled"})
					case subagents.CommandTurnStart:
						var payload subagents.TurnStartPayload
						if command.DecodePayload(&payload) == nil {
							runTurn(emit, payload.Prompt, turn)
							turn++
						}
					}
				}
			}

			if err != nil {
				_ = c.Close()
				break
			}
		}
	}
}

// runTurn fakes one model round-trip: turn_start, an echoed
// user_message, an assistant_message that echoes back, and a
// turn_end. Enough event variety that applyEventToSink has
// something to interpret.
func runTurn(emit emitter, text string, step int) {
	emit("turn_start", map[string]any{"step": step})
	emit("user_message", map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": text},
		},
	})
	emit("assistant_message", map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": "echo: " + text},
		},
	})
	emit("turn_end", map[string]any{"step": step, "stop": "end"})
}

type emitter = func(string, map[string]any)

func newEmitter() emitter {
	var mu sync.Mutex
	enc := json.NewEncoder(os.Stdout)
	versioned := os.Getenv("ZUT_STUB_PROTOCOL") == "1"
	return func(typ string, data map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		if data == nil {
			data = map[string]any{}
		}
		if versioned {
			envelope := subagents.NewEventEnvelope(typ, "stub-agent", "", data)
			_ = enc.Encode(envelope)
			return
		}
		data["type"] = typ
		data["time"] = time.Now().Format(time.RFC3339Nano)
		_ = enc.Encode(data)
	}
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
