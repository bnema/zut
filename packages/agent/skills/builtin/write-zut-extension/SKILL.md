---
name: write-zut-extension
description: Help the user create a new zut extension (slash command, LLM tool, or guard) in any language.
---

# Writing a zut extension

Use this skill when the user asks for help building a zut extension —
a new slash command, a new tool the LLM can call, an audit hook, or
a permission gate. Skim this whole skill first, then collaborate
with the user on the specific extension they want.

## What an extension is

A zut extension is **an external executable** that zut launches as a
subprocess and talks to over its stdin/stdout in newline-delimited
JSON. It can be written in any language that can read/write JSON
lines from stdio: Go, TypeScript (via tsx), Python, Rust, shell with
jq, anything. Crash isolation is automatic; one bad extension never
takes down zut.

Three things an extension can do (any combination):

1. **Slash commands** — register `/foo` so the user can run it from
   the input. The handler returns a "prompt" (submitted to the
   agent), an "insert" (text dropped into the editor), a "display"
   (one-shot styled note in the chat), or a "noop".

2. **Tools** — register tools the LLM itself calls. Schema is
   JSON Schema; zut routes the model's `tool_call` to the
   extension's `tool_result`. Same lifecycle as built-in tools
   (read/write/edit/bash/create_worktree/grep/lsp/web_search/skill).

3. **Lifecycle hooks** — subscribe to events
   (session_start, turn_start, tool_call, turn_end,
   assistant_message) for telemetry / audit / custom UI, or
   intercept tool calls before execution to refuse dangerous
   patterns.

## On-disk layout

Each extension lives in its own directory:

```
~/Library/Application Support/zut/extensions/<name>/
├── extension.json    # manifest (required)
└── <executable>      # whatever exec points at
```

Or project-local: `<project>/.zut/extensions/<name>/`. Project-local
wins on name conflict.

For ad-hoc use during development, skip the install step entirely
and run `zut --ext PATH` (repeatable: `-e PATH -e PATH`).

### Manifest

```json
{
  "name": "weather",
  "version": "1.0.0",
  "exec": "./weather",
  "args": [],
  "language": "go",
  "description": "current weather lookups for any city",
  "enabled": true
}
```

Field rules:
- `name` (required, unique) — id zut uses internally; matches the
  hello frame. Slash commands & tools live in the same name space
  as built-ins; conflicts with active built-ins are ignored. The
  native `web_search` and `grep` names are reserved even when those
  built-ins are disabled, so an extension cannot claim or replace them.
- `exec` (required) — the executable path. Resolution:
  - absolute: as-is
  - starts with `./` or `../`: relative to the manifest's directory
  - bare name (no separator): looked up via `$PATH` (e.g. `node`,
    `python3`, `npx`, `tsx`)
- `args` — extra argv passed to `exec` (e.g. `["index.js"]`)
- `language` — informational only (`"go"`, `"typescript"`,
  `"python"`, etc.)
- `enabled` — defaults to true; set false to keep installed but skip

## Wire format

Newline-delimited JSON in both directions. Top-level `type` is the
discriminator. Optional `id` correlates command/tool requests with
their responses.

### Required handshake

The very first frame the extension sends is `hello`:

```json
{"type":"hello","protocol_version":2,"name":"weather","version":"1.0.0",
 "capabilities":["commands","tools"]}
```

Capabilities are advisory; current values are `commands`, `tools`,
`events`, `alerts`, and `panels`. Send all that apply. `protocol_version`
is the major wire version; it is currently `2` and must match the host.

zut replies with `hello_ack`:

```json
{"type":"hello_ack","protocol_version":2,"zut_version":"0.0.x",
 "provider":"anthropic","model":"claude-opus-4-7","cwd":"/path/to/project"}
```

### Registration (after hello_ack)

The canonical startup order is `hello`, wait for `hello_ack`, send
registration frames in any order, then send a single `ready` sentinel
so zut can finalize the agent's tool registry:

```json
{"type":"register_command","name":"weather","description":"current weather"}
{"type":"register_tool","name":"weather","description":"Get current weather for a city.",
 "schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}
{"type":"subscribe","events":["tool_call"],"intercept":["tool_call"]}
{"type":"ready"}
```

If you don't send `ready`, zut's idle watchdog auto-treats you as
ready after 250ms of no frames, but always send it explicitly when
you can. Newer extensions on faster hosts shave that 250ms off.

Use `hello_ack.cwd` for the user's project directory. The extension
process itself runs from the extension directory, so do not use
`os.Getwd()` or `process.cwd()` when you need the project path.

### Runtime frames

**zut → extension:**

```json
{"type":"command_invoked","id":"abc","name":"weather","args":"berlin"}
{"type":"tool_call","id":"def","name":"weather","args":{"city":"Berlin"}}
{"type":"event","event":"turn_start","step":1}
{"type":"event_intercept","id":"ghi","event":"tool_call",
 "tool_name":"bash","tool_args":{"command":"rm -rf /tmp/foo","timeout":30}}
{"type":"shutdown"}
```

**extension → zut (replies + spontaneous notifications):**

```json
{"type":"command_response","id":"abc","action":"prompt",
 "prompt":"Show today's weather for Berlin in one line."}
{"type":"tool_result","id":"def","content":[{"type":"text","text":"Berlin: 16°C, fog"}]}
{"type":"event_intercept_response","id":"ghi","block":true,
 "reason":"refused: command matches the danger pattern \"rm -rf\""}
{"type":"notify","level":"info","message":"refreshed cache"}
{"type":"alert","kind":"bell","reason":"question_ready"}
{"type":"clear_notes"}
{"type":"shutdown_ack"}
```

`notify` notes are one-shot: they clear when the user sends their next
prompt (and on `esc` / `/clear`). Send `clear_notes` to retract every
note this extension pushed earlier (e.g. a transient approval prompt)
without waiting for the next turn; other extensions' notes are kept.

`alert` is a host-owned, fire-and-forget request. The first supported
kind is `bell`; `reason` is semantic metadata. The interactive host
applies the user's shared terminal-alert setting and non-interactive
hosts may ignore it. Extensions must never write BEL or ANSI bytes to
stdout.

`command_response.action` values:
- `"prompt"` — submit `prompt` as a fresh user message
- `"insert"` — drop `insert` into the editor at the cursor
- `"display"` — append `display` to chat as a one-shot note (no
  model call, not in transcript)
- `"noop"` — handled internally; zut doesn't change the UI

`tool_result.content[]` blocks: `{"type":"text","text":"..."}` or
`{"type":"image","mime_type":"image/png","data":"<base64>"}`.

Per-tool timeout: 60s. Per-intercept timeout: 5s. Missing the
intercept timeout is treated as "allow" so an unresponsive guard
never stalls the agent.

## Important rules

- **stdout is reserved for the protocol.** Anything you print to
  stdout that isn't a JSON frame breaks the wire. The first stdout
  frame must be `hello`; do not send `notify`, `alert`, logs, or
  registration frames before that handshake starts. Use stderr for
  logs / debug output (zut captures stderr to `$ZUT_HOME/logs/ext-<name>.log`).
- **One JSON object per line.** No multi-line JSON. Always end
  every frame with `\n`.
- **Flush after writing.** Most stdout writes are line-buffered when
  piped, which is fine, but explicitly flushing avoids surprise
  buffering on slow handlers.
- Extension processes inherit the user's permissions. A bad
  extension can do anything the user can.

## Recommended layout per language

### Go (use the built-in SDK at packages/agent/ext)

```go
package main

import (
    "encoding/json"
    "github.com/bnema/zut/packages/agent/ext"
)

func main() {
    e := ext.New("weather", "1.0.0")

    e.Command("weather", "current weather for a city",
        func(args string) ext.Response {
            return ext.Prompt("Tell me the weather for " + args)
        })

    e.Tool("weather", "Get current weather for a city.",
        json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
        func(args json.RawMessage) ext.ToolResult {
            var in struct{ City string `json:"city"` }
            if err := json.Unmarshal(args, &in); err != nil {
                return ext.TextErrorResult("invalid args")
            }
            return ext.TextResult(in.City + ": sunny, 21°C (fake)")
        })

    // Optional: register project-specific commands after hello_ack.
    e.OnHello(func(host ext.HostInfo) {
        if host.CWD != "" {
            e.Command("cwd", "show the current project directory", func(args string) ext.Response {
                return ext.Display(host.CWD)
            })
        }
    })

    if err := e.Run(); err != nil {
        e.Logf("fatal: %v", err)
    }
}
```

Build: `go build -o weather .`

`OnHello` is optional. Use it when configuration or registrations need
host metadata such as `HostInfo.CWD`, `Provider`, `Model`, `ZutVersion`,
`ExtensionDir`, or `DataDir`. The SDK sends `hello`, waits for
`hello_ack`, runs `OnHello`, announces registrations, then sends `ready`.

`extension.json`:
```json
{"name":"weather","version":"1.0.0","exec":"./weather","language":"go","enabled":true}
```

After the hello handshake completes inside `Run`, call `e.Alert` from a
command, tool, or panel handler when the user needs to be drawn back to zut:

```go
e.Alert(ext.AlertRequest{Kind: ext.AlertKindBell, Reason: "question_ready"})
```

### TypeScript (no SDK; handles the protocol directly)

Run via `tsx`, which executes `.ts` files without a build step.

```json
{"name":"scratchpad","version":"1.0.0","exec":"tsx","args":["index.ts"],"language":"typescript","enabled":true}
```

```typescript
// index.ts (excerpt; see examples/extensions/scratchpad/index.ts for the full version)
import { createInterface } from "node:readline";
import { stderr, stdin, stdout } from "node:process";

function send(o: object) { stdout.write(JSON.stringify(o) + "\n"); }
function log(s: string) { stderr.write(`[scratchpad] ${s}\n`); }

send({ type: "hello", protocol_version: 2, name: "scratchpad", version: "1.0.0",
       capabilities: ["commands", "tools"] });

const rl = createInterface({ input: stdin, crlfDelay: Infinity });
rl.on("line", (line) => {
  const f = JSON.parse(line);
  if (f.type === "hello_ack") {
    if (f.protocol_version !== 2) throw new Error("unsupported extension protocol version");
    // f.cwd is the user's project directory.
    send({ type: "register_command", name: "note", description: "append a note" });
    send({ type: "register_tool", name: "read_notes",
           description: "Read the user's scratchpad notes.",
           schema: { type: "object", properties: {} } });
    send({ type: "ready" });
  } else if (f.type === "command_invoked" && f.name === "note") {
    send({ type: "command_response", id: f.id, action: "display",
           display: `noted: ${f.args}` });
  } else if (f.type === "tool_call" && f.name === "read_notes") {
    send({ type: "tool_result", id: f.id,
           content: [{ type: "text", text: "(notes go here)" }] });
  } else if (f.type === "shutdown") {
    send({ type: "shutdown_ack" });
    rl.close();
  }
});
```

`tsx` install: `npm install -g tsx`. Without global tsx, fall back
to `"exec":"npx","args":["--yes","tsx","index.ts"]` (slower
startup; npx checks the registry every launch).

### Python

```json
{"name":"hello-py","version":"1.0.0","exec":"./hello.py","language":"python","enabled":true}
```

```python
#!/usr/bin/env python3
import json, sys

def emit(o): sys.stdout.write(json.dumps(o) + "\n"); sys.stdout.flush()

emit({"type": "hello", "protocol_version": 2, "name": "hello-py", "version": "1.0.0", "capabilities": ["commands"]})

for line in sys.stdin:
    msg = json.loads(line)
    if msg["type"] == "hello_ack":
        if msg.get("protocol_version") != 2:
            raise SystemExit("unsupported extension protocol version")
        # msg["cwd"] is the user's project directory.
        emit({"type": "register_command", "name": "hellopy", "description": "say hi (python)"})
        emit({"type": "ready"})
    elif msg["type"] == "command_invoked":
        emit({"type": "command_response", "id": msg["id"],
              "action": "prompt", "prompt": "Say hi briefly."})
    elif msg["type"] == "shutdown":
        emit({"type": "shutdown_ack"})
        break
```

`chmod +x hello.py`.

## Install / dev workflow

```bash
zut ext install ./weather       # copy into $ZUT_HOME/extensions/
zut ext install --build=go ./weather # explicitly build local Go source
zut --ext ./weather             # run from disk for one zut session (no install)
zut --ext .                     # cwd is the extension dir
zut ext list                    # show installed extensions
zut ext logs weather            # cat the extension's stderr
zut ext logs weather -f         # tail it
zut ext disable weather         # keep installed but skip on launch
zut ext enable weather
zut ext remove weather
```

For TS / Python extensions, no build step is needed — edit the source
in place and relaunch zut.

For Go, either run `zut ext install --build=go <path>` to explicitly build
the local source into the staged install, or run `go build -o <name> .`
first and then use `zut ext install <path>`. The default install never runs a
build step; a missing local executable is rejected with an explicit builder
hint instead of leaving a broken installed extension. The `--build=go` option
is intentionally limited to local source paths and does not apply to git URLs.

## Manual debug

The extension is just a process. Drive it directly with shell pipes
to see exactly what's happening on the wire:

```bash
{
  printf '%s\n' '{"type":"hello_ack","protocol_version":2,"zut_version":"x","provider":"a","model":"o","cwd":"/tmp"}'
  sleep 0.2
  printf '%s\n' '{"type":"command_invoked","id":"1","name":"weather","args":"Berlin"}'
  sleep 0.5
  printf '%s\n' '{"type":"shutdown"}'
} | ./weather
```

Compare what comes out of stdout to the expected wire format. If a
frame doesn't match what zut expects, it's discarded silently and
logged to `ext-<name>.log`.

## Process to follow with the user

1. Ask what the extension should DO. One sentence.
2. Pick the right capability:
   - "I want a slash command that triggers a prompt" → `command` only
   - "I want the model to be able to do X" → `tool`
   - "I want to gate / log every bash command" → `event` + `intercept`
3. Pick a language. Default to **Go via packages/agent/ext** for new
   extensions if the user has Go installed; **TypeScript via tsx**
   if they prefer JS-flavored ergonomics; **Python** for one-off
   scripts.
4. Write the extension dir (manifest + source).
5. For Go, use `zut ext install --build=go <path>` or build it manually.
   For TS / Python, mark the script executable.
6. Suggest `zut --ext <path>` for testing without committing to an install.
7. When happy, `zut ext install <path>` (or `--build=go` for local Go source).

Don't try to write a full SDK or framework on top of the protocol
unless the user asked for one — the wire format is small enough
that a 30-line raw script is the right answer for most extensions.
