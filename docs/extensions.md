# zut extensions

zut can be extended with custom slash commands by running an external
program as a subprocess and exchanging newline-delimited JSON over
its stdin/stdout. Extensions can be written in **any language** that
can read and write JSON lines from stdio — Go, TypeScript, Python,
Rust, shell with `jq`, anything.

Six phases shipped so far:

- **Phase 1**: slash commands, chat notifications, and host alerts.
- **Phase 2**: tools the LLM can call.
- **Phase 3**: lifecycle event subscriptions + tool-call interception
  for guardrail extensions.
- **Phase 4**: interactive extension-owned panels rendered inside zut.
- **Phase 5**: branch-aware session lifecycle events and opaque extension
  state persisted with session files.
- **Phase 6**: hidden per-turn context, persistent status/widgets, and
  extension-bundled skills.
- **Theme-only extensions**: ship `theme.json` without launching a
  subprocess. See [themes.md](themes.md).

## Quick start

The simplest extension is a script that prints a hello frame, reads
commands, and prints responses. Here's the whole thing in **Python**,
no SDK required:

```python
#!/usr/bin/env python3
# $ZUT_HOME/extensions/hello-py/hello.py
import json, sys

def emit(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()

emit({"type":"hello","protocol_version":2,"name":"hello-py","version":"1.0.0","capabilities":["commands"]})

for line in sys.stdin:
    msg = json.loads(line)
    if msg["type"] == "hello_ack":
        if msg.get("protocol_version") != 2:
            raise SystemExit("unsupported extension protocol version")
        emit({"type":"register_command","name":"hellopy","description":"say hi (python)"})
        emit({"type":"ready"})
    elif msg["type"] == "command_invoked":
        emit({"type":"command_response","id":msg["id"],"action":"prompt",
              "prompt": "Greet me very briefly in one sentence."})
    elif msg["type"] == "shutdown":
        emit({"type":"shutdown_ack"})
        break
```

Drop it in a directory with this `extension.json`:

```json
{
  "name": "hello-py",
  "version": "1.0.0",
  "exec": "./hello.py",
  "language": "python",
  "enabled": true
}
```

`exec` is required for protocol extensions. If an extension only ships
`theme.json` or `themes/theme.json`, no `exec` is required and zut does
not spawn a subprocess.

`chmod +x hello.py`, install:

```bash
zut ext install ./hello-py
```

Restart `zut`, type `/hellopy`, the agent greets you. Done.

## Built-in extensions

**zut ships with no extensions installed by default.** A fresh `zut install` (or `go install`) gives you a clean agent. Extensions are entirely opt-in: you install (or `--ext` for one run) only the ones you want.

The `examples/extensions/` directory in the repo is reference code, not a default install set. To use any of those:

```bash
# install a Go example and explicitly build it in the staged install
zut ext install --build=go path/to/zut/examples/extensions/hello

# or build it in the source tree for a one-run development load
cd path/to/zut/examples/extensions/hello
go build -o hello .
zut --ext .
```

`zut ext install` never builds source code implicitly. For local installs it
validates `extension.json` and any executable path relative to the extension
directory, then stages the copy so a missing or ignored runtime artifact fails
with an explicit error instead of becoming a broken installed extension. For a
local Go extension, opt into the fixed builder explicitly with
`zut ext install --build=go <path>`. The builder runs `go build -trimpath` from
the source directory, writes the manifest-declared relative executable into the
staging directory, and validates it before installation. It may resolve Go
modules and access the network according to the user's Go environment. Bare
launchers such as `go`, `node`, and `npx` remain resolved from `PATH` when zut
starts; they cannot be used as the output of `--build=go`.

Nothing is auto-installed and nothing reaches out to the network without your explicit action.

## Layout & discovery

zut scans two directories on startup, in this order:

1. **Project-local**: `./.zut/extensions/<name>/extension.json`
2. **Global**: `$ZUT_HOME/extensions/<name>/extension.json`

A project-local extension with the same name wins over a global one.
When `XDG_STATE_HOME` is set on any platform, `$ZUT_HOME` defaults to
`$XDG_STATE_HOME/zut`. Otherwise it defaults to `~/Library/Application Support/zut/`
on macOS, `~/.local/state/zut` on Linux, or `%LOCALAPPDATA%\zut` on Windows.

Because each extension owns its own directory, the recommended place
for extension state is inside that directory itself (for example
`todos.json`, `settings.json`, or an auth/cache file used only by that
extension). The host also passes this path back in `hello_ack` as
`extension_dir` / `data_dir` so runtime code does not need to guess it.

Each extension owns its own subdirectory. The `extension.json`
manifest tells zut how to launch it:

```json
{
  "name": "weather",
  "version": "1.0.0",
  "exec": "./weather",
  "args": ["--mode", "daemon"],
  "language": "go",
  "description": "current weather for any city",
  "skills": ["skills"],
  "enabled": true
}
```

| field | meaning |
|---|---|
| `name` | required. how zut identifies the extension; must match what's sent in the `hello` frame. |
| `version` | optional. shown in `zut ext list`. |
| `exec` | required. path to the executable (relative to the manifest). |
| `args` | optional. extra argv passed to `exec`. |
| `language` | optional. informational only (`go`, `python`, `typescript`, ...). |
| `description` | optional. shown in `zut ext list`. |
| `skills` | optional. relative directories containing bundled `<name>/SKILL.md` files or a direct `SKILL.md`. Paths must remain inside the extension directory. |
| `enabled` | optional, defaults to `true`. set to `false` to disable without removing. |

## Lifecycle

1. **Discovery**: zut reads every `extension.json` in the search dirs.
2. **Spawn**: enabled extensions are launched as subprocesses. stderr
   redirects to `$ZUT_HOME/logs/ext-<name>.log` (one file per
   extension, append-mode).
3. **Hello handshake**: the extension's first stdout frame must be
   `hello` with `protocol_version: 2`; zut rejects a mismatched major
   and replies with `hello_ack` containing the same protocol version,
   the active provider/model/cwd, and the extension's own data directory
   so it can persist files beside its manifest.
4. **Registration**: after receiving `hello_ack`, the extension sends
   `register_command`, `register_tool`, and subscription frames, then
   sends `ready`. First-come-first-served: a name already taken by a
   built-in or by a previously-loaded extension is silently shadowed
   (logged in the extension's own log file).
5. **Runtime**: after `ready`, zut dispatches `command_invoked` frames
   when the user runs a registered command; the extension responds
   with `command_response`. Extensions can also push `notify`, persistent
   status/widget, and structured `alert` frames during runtime. Extensions
   subscribed to session lifecycle events receive the active branch identity
   and their own restored opaque state. Panel-capable extensions may open an
   interactive panel, receive key events, and push redraws while the panel is
   focused.
6. **Shutdown**: when zut exits, it sends `shutdown` and waits up to
   2s for the extension to send `shutdown_ack`. Holdouts are
   SIGTERM'd, then SIGKILL'd.

A crashing extension does not bring down zut. The slash command it
owned simply stops working until the extension is fixed and zut is
restarted.

## Wire format

All frames are one JSON object per line. Top-level `type` is the
discriminator. Optional `id` correlates request frames with their
responses. The canonical startup order is `hello`, `hello_ack`,
registration frames, then `ready`. Do not send `notify`, `alert`, logs,
or any other stdout frame before `hello`.

### Extension → host

#### `hello` (required, first frame)

```json
{"type":"hello","protocol_version":2,"name":"weather","version":"1.0.0",
 "capabilities":["commands","tools","alerts","panels"]}
```

#### `register_command`

```json
{"type":"register_command","name":"weather",
 "description":"current weather for a city"}
```

Command names are matched case-insensitively. zut sends `command_invoked.name` using the canonical spelling registered here. Registrations that differ only by case conflict, and the first registration remains active.

#### `register_tool`

Registers a tool the LLM can call. `schema` is a JSON Schema object
describing the tool's args (the same shape Anthropic and OpenAI accept).

```json
{"type":"register_tool","name":"weather",
 "description":"Get the current weather for a city.",
 "schema":{
   "type":"object",
   "properties":{"city":{"type":"string"}},
   "required":["city"]
 }}
```

Tool names live in the same namespace as built-in tools (`read`,
`write`, `edit`, `bash`, `create_worktree`, `grep`, `lsp`, `web_search`,
`web_open`, `web_find`, `web_click`, `skill`). Conflicts with active built-ins
are silently shadowed by the built-in. Every native public-web capability name
is reserved even when its setting disables the capability, so an extension
cannot claim or replace it. The `grep` name is
reserved even when `Args.NoTools`/SDK `Config.NoTools` or an explicit
`Args.Tools`/SDK `Config.Tools` allowlist disables the native search tool, so
an extension cannot claim or replace it.

Set `"deferred": true` to register a tool without advertising its definition initially. A loader tool can activate registered deferred tools by returning their names in `activate_tools`:

```json
{"type":"tool_result","id":"...",
 "content":[{"type":"text","text":"Enabled weather lookup"}],
 "activate_tools":["weather"]}
```

On Kimi K3's OpenAI-compatible routes, zut places newly activated schemas at the tool-result position using Kimi's native deferred-tool format. Other models receive the complete active tool list on the next request. Unknown names are ignored. The Go extension SDK exposes `DeferredTool` and `ToolResult.ActivateTools` for the same protocol.

#### `ready`

Sentinel telling zut "all initial registrations are flushed". Send it
right after your last `register_*` frame so the host can build the
agent's tool registry without racing the registration window.

```json
{"type":"ready"}
```

#### `tool_result`

Reply to a `tool_call` from the host. `content[]` is a list of
message blocks; each block is `{"type":"text","text":"..."}` or
`{"type":"image","mime_type":"image/png","data":"<base64>"}`. Set
`is_error: true` to mark the call as failed. `activate_tools` can name
registered deferred tools that become available after this result.

```json
{"type":"tool_result","id":"...",
 "content":[{"type":"text","text":"Berlin: 16°C, fog"}],
 "details":{"state":{"version":1}}}
```

`details` is opaque extension metadata. Zut excludes it from provider
requests and persists it as the latest extension state for the active
session branch. Keep it valid JSON and reasonably small; the host caps
persisted snapshots at 256 KiB. The next `session_opened`,
`session_switched`, `session_forked`, or `session_compacted` event returns
the extension's own snapshot in `state`.

#### `subscribe`

Declares which lifecycle events the extension wants to observe and
which it wants to intercept. Send once after `hello`, before `ready`.

```json
{"type":"subscribe",
 "events":["session_start","turn_start","tool_call","tool_confirmation_requested","turn_end","assistant_message"],
 "intercept":["tool_call","turn_start","assistant_message"]}
```

Recognised event names include `session_start`, `session_opened`,
`session_switched`, `session_forked`, `session_compacted`, `turn_start`,
`tool_call`, `tool_confirmation_requested`, `assistant_message`, and
`turn_end`.

Session events contain `session` with the current branch ID, parent ID,
path, cwd, and fork point. They also contain `state`, but only for the
receiving extension. Zut never exposes another extension's state.

`tool_confirmation_requested` fires only when zut is about to wait for
interactive approval. Calls running in yolo mode, calls covered by a
remembered approval, and calls blocked before confirmation do not emit it.
The event includes `tool_id`, `tool_name`, and the short `tool_preview`
shown in the confirmation dialog.

Interceptable events:

- `tool_call`: block the call (model sees `reason` as the tool
  error) or rewrite args via `modified_args`.
- `turn_start`: block the turn before the model is called. Useful
  for rate-limiting and business-hour gates. `reason` is shown to
  the user as a status line. No rewrite supported.
- `assistant_message`: suppress the message via `block`, or rewrite
  the user-visible text via `replace_text`. The model's original
  text stays in the transcript so the model sees what it actually
  said on subsequent turns.

#### `event_intercept_response`

Reply to an `event_intercept` from the host. All fields default to
"allow, pass through unmodified".

| field | meaning |
|---|---|
| `block` | `true` refuses the action. For `tool_call`, `reason` is shown to the model; for `turn_start` / `assistant_message`, `reason` is shown to the user. |
| `reason` | refusal text (on block) or pass-through note. |
| `modified_args` | for `tool_call`: rewritten JSON args the tool will actually see. Must be a valid JSON object. Ignored when `block` is true. Rewrites replace the complete argument object; for `bash`, preserve the required positive integer `timeout` when changing `command`. |
| `replace_text` | for `assistant_message`: replaces the user-visible text. The model's original output still lives in the transcript. Ignored when `block` is true. |
| `context` | for `turn_start`: bounded hidden context sent only in the upcoming provider request. It is not a user message and is not persisted in the transcript. |

Missing the response within 5s is treated as "allow" (i.e. an
unresponsive extension never stalls the agent). When multiple
extensions subscribe to the same event, they're consulted serially;
the first `block` wins and rewrites (args / text) chain: each
subsequent interceptor sees the previous one's output.

```json
{"type":"event_intercept_response","id":"...",
 "block":true,"reason":"refused: matches danger pattern \"rm -rf\""}

{"type":"event_intercept_response","id":"...",
 "modified_args":{"command":"echo GUARDED: ls","timeout":30}}

{"type":"event_intercept_response","id":"...",
 "replace_text":"[redacted]"}

{"type":"event_intercept_response","id":"...",
 "context":"Current phase: parse files\nRemaining: validate inputs"}
```

#### `command_response` (reply to `command_invoked`)

```json
{"type":"command_response","id":"...","action":"prompt",
 "prompt":"Show today's weather for Berlin in one line."}
```

`action` is one of:

- `"prompt"` — submits `prompt` as a fresh user message; the agent
  runs a turn against it.
- `"insert"` — inserts `insert` into the editor at the cursor without
  submitting.
- `"display"` — appends `display` to the chat as a one-shot styled
  note. No model call, nothing written to the transcript.
- `"open_panel"` — opens an extension-owned interactive panel inside
  zut. The panel content lives in `open_panel`.
- `"noop"` — the extension handled it itself (e.g. it pushed
  `notify` frames or kicked off background work). zut doesn't change
  the UI in response.

Example:

```json
{"type":"command_response","id":"...","action":"open_panel",
 "open_panel":{
   "id":"todos-main",
   "title":"Todos",
   "lines":["□ ship panel api","✓ persist state"],
   "footer":"↑/↓ navigate - a add - x complete - esc close"
 }}
```

If `error` is non-empty, zut renders it as a red status line
regardless of `action`.

#### `submit` (one-way, any time)

Submits text as a user prompt in the interactive host. If the agent is
idle, zut starts a turn immediately. If a turn is already running, zut
queues the prompt behind it using the same queue path as typed input.
Empty or whitespace-only text is ignored.

```json
{"type":"submit","text":"Summarize the selected workspace."}
```

In print / JSON / RPC modes this frame is ignored because there is no
interactive editor or prompt queue.

#### `panel_render` (one-way, while a panel is open)

Pushes a fresh frame for an already-open panel.

```json
{"type":"panel_render","panel_id":"todos-main",
 "title":"Todos",
 "lines":["□ ship panel api","✓ persist state"],
 "footer":"↑/↓ navigate - a add - x complete - esc close"}
```

#### `panel_close`

Closes a previously-open panel.

```json
{"type":"panel_close","panel_id":"todos-main"}
```

#### `status` (one-way, persistent)

Sets or replaces one status item owned by the extension. Sending an empty
`text` clears the item.

```json
{"type":"status","key":"progress","level":"success",
 "text":"2/4 tasks checked"}
```

#### `widget` (one-way, persistent)

Sets or replaces a compact widget. `position` is a host-defined placement
hint with these interactive-zut values:

- `"above_input"` keeps the widget in the existing persistent chrome above
  the editor.
- `"right_bar"` keeps the widget in a display-only side rail beside the
  transcript when the terminal is wide enough.

The host owns right-bar layout: it orders widgets, bounds width and height,
and truncates content. Narrow terminals and `Ctrl+B` use a bounded
`above_input` fallback. Empty or unknown positions keep the historical
`above_input` behavior. Use `open_panel` for interaction or navigation.

```json
{"type":"widget","id":"plan","position":"right_bar",
 "title":"Tasked phases","lines":["Current phase: parse","[ ] validate inputs"]}
```

#### `widget_clear` (one-way)

```json
{"type":"widget_clear","id":"plan"}
```

#### `notify` (one-way, any time)

```json
{"type":"notify","level":"info",
 "message":"refreshed cache (12 entries)"}
```

`level` is one of `info`, `success`, `warn`, `error`. The note shows
up below the transcript with the extension's name in brackets. Notes
are one-shot: they clear automatically when the user sends their next
prompt (and on `esc` / `/clear`).

#### `alert` (one-way, any time)

Requests a host-owned structured alert. The first supported kind is
`bell`, which asks the interactive terminal to emit one terminal bell.
`reason` is semantic metadata and is not rendered as terminal text.
The shared terminal-alert setting in `/settings` applies to extension
alerts too; disabled or non-interactive hosts may ignore the request.

```json
{"type":"alert","kind":"bell","reason":"question_ready"}
```

The Go SDK sends the same frame with:

```go
e.Alert(ext.AlertRequest{Kind: ext.AlertKindBell, Reason: "question_ready"})
```

Extensions must not write BEL, ANSI, or other terminal bytes to stdout;
stdout is reserved for protocol frames. In `--mode rpc`, this becomes an
`ext_alert` JSON event for the RPC client to interpret.

#### `clear_notes` (one-way, any time)

Removes every note this extension previously pushed via `notify` /
`display`. Use it for transient status lines (e.g. an approval prompt)
so they do not stack up; notes from other extensions are untouched.

```json
{"type":"clear_notes"}
```

In `--mode rpc`, this surfaces to the host as an `ext_clear_notes`
event (alongside `ext_notify` / `ext_display`).

#### `shutdown_ack`

Sent in response to `shutdown`. Extension should exit promptly after.

### Host → extension

#### `hello_ack`

```json
{"type":"hello_ack","protocol_version":2,
 "zut_version":"0.0.7","provider":"anthropic",
 "model":"claude-opus-4-7","cwd":"/path/to/zut",
 "extension_dir":"/path/to/zut/.zut/extensions/todos",
 "data_dir":"/path/to/zut/.zut/extensions/todos",
 "session":{"id":"branch-1","parent_id":"root-1",
             "path":".../session.jsonl","cwd":"/path/to/zut",
             "fork_point":4}}
```

Sent immediately after a matching `hello`. The protocol version is a
major and must match the version sent by the extension. Wait for this frame
before sending registrations if they depend on host metadata. The extension can use
these fields to decide which commands to register (e.g. only register
a Python tool on macOS, only register a model-specific shortcut for
opus, etc.). `cwd` is the user's project directory; the extension
process itself runs from the extension directory, so do not use
`os.Getwd()` or `process.cwd()` when you need the project path.
`extension_dir` / `data_dir` are where the extension should persist
its own state (for example `todos.json`, cached metadata, or auth
tokens scoped to that extension).

#### `command_invoked`

```json
{"type":"command_invoked","id":"...",
 "name":"weather","args":"berlin"}
```

`args` is everything the user typed after the command name, trimmed.

#### `tool_call`

Sent when the LLM invokes a tool the extension registered. `args` is
the parsed JSON object the model produced; the extension is
responsible for validating/coercing it.

```json
{"type":"tool_call","id":"...","name":"weather",
 "args":{"city":"Berlin"}}
```

Reply with `tool_result` within the host's tool timeout (default 60s).
Missing the timeout surfaces an error to the model and the call is
marked as failed.

#### `event`

Lifecycle notification for events the extension subscribed to via
`subscribe`. One-way — no response expected.

```json
{"type":"event","event":"session_opened",
 "session":{"id":"branch-1","parent_id":"root-1","fork_point":4},
 "state":{"version":1}}
{"type":"event","event":"turn_start","step":1}
{"type":"event","event":"tool_call",
 "tool_id":"...","tool_name":"read","tool_args":{"path":"foo.go"}}
{"type":"event","event":"tool_confirmation_requested",
 "tool_id":"...","tool_name":"read","tool_preview":"foo.go"}
{"type":"event","event":"turn_end","stop":"end_turn"}
```

#### `event_intercept`

Sent when zut wants to give the extension a chance to block, modify,
or annotate a lifecycle event before it happens. Reply with
`event_intercept_response` within 5s; missing the deadline is
treated as "allow".

Payload fields depend on the event:

```json
// tool_call: includes the tool id, name, and parsed args
{"type":"event_intercept","id":"...","event":"tool_call",
 "tool_id":"...","tool_name":"bash",
 "tool_args":{"command":"rm -rf /tmp/foo","timeout":30}}

// turn_start: includes the step number
{"type":"event_intercept","id":"...","event":"turn_start",
 "step":3}

// assistant_message: includes the assembled text
{"type":"event_intercept","id":"...","event":"assistant_message",
 "text":"here is your api key: sk-ant-..."}
```

#### `panel_key`

Sent while an extension-owned panel is focused. `key` is a normalized
name (`up`, `down`, `left`, `right`, `enter`, `esc`, `tab`, `pageup`,
`pagedown`, `home`, `end`, `backspace`, `delete`, `rune`). For
`key:"rune"`, `text` carries the typed character.

```json
{"type":"panel_key","panel_id":"todos-main","key":"down"}
{"type":"panel_key","panel_id":"todos-main","key":"rune","text":"x"}
```

#### `panel_close`

Sent when the user closes the focused panel from zut (for example with
Esc or Ctrl+C). The extension should treat this as the panel lifetime
ending and stop sending `panel_render` updates for that `panel_id`.

```json
{"type":"panel_close","panel_id":"todos-main"}
```

#### `shutdown`

Sent during graceful zut exit (or `/reload-ext` once that lands).
Reply with `shutdown_ack` and then exit.

## Managing extensions from the CLI

```
zut ext list                              list installed extensions and their state
zut ext doctor                            diagnose load, registration, and conflict issues
zut ext install <path|git-url>             copy / clone into $ZUT_HOME/extensions/
zut ext install --build=go <local-path>   build local Go source, then install
zut ext remove <name>                     delete an extension directory
zut ext enable <name>           re-enable a disabled extension
zut ext disable <name>          disable without removing
zut ext logs <name> [-f]        cat / tail the extension's stderr
```

`zut ext doctor` runs the same discovery path as zut startup, but reports
what happened instead of changing the fail-soft runtime behavior. It shows
manifest errors, disabled or shadowed extensions, subprocess load errors,
ready/auto-ready status, registered commands/tools, registration conflicts,
warnings, and each extension's stderr log path.

`zut ext install <path>` does a recursive copy; `<git-url>` does a
shallow clone. Both validate that the destination contains an
`extension.json` and roll back if not. No build runs unless the local install
is invoked with the explicit `--build=go` option. That option currently supports
only local Go source paths; clone remote source locally before building it.

## Loading an extension for one run

For iteration on a working copy, skip the install + reload cycle
and load straight from disk for one zut session:

```
zut --ext ./my-extension        # short form: -e ./my-extension
zut --ext ./a -e ./b            # repeatable
```

`--ext` paths take precedence over installed extensions of the same
name, so you can shadow an installed copy with a work-in-progress
version without uninstalling first. Nothing is copied or persisted;
the extension dies with zut like any other subprocess.

## SDKs

Writing the wire protocol by hand is fine for one-off scripts, but
for anything bigger the SDKs handle the boilerplate.

### Go — `packages/agent/ext`

```go
package main

import (
    "encoding/json"
    "github.com/bnema/zut/packages/agent/ext"
)

func main() {
    e := ext.New("hello", "1.0.0")

    // Slash command
    e.Command("hello", "say hi", func(args string) ext.Response {
        return ext.Prompt("Greet me in one short sentence.")
    })

    // LLM-callable tool
    e.Tool("weather", "Current weather for a city.",
        json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
        func(args json.RawMessage) ext.ToolResult {
            var in struct{ City string `json:"city"` }
            json.Unmarshal(args, &in)
            return ext.TextResult(in.City + ": sunny")
        })

    // Optional: register project-specific commands after hello_ack.
    e.OnHello(func(host ext.HostInfo) {
        if host.CWD != "" {
            e.Command("cwd", "show the current project directory", func(args string) ext.Response {
                return ext.Display(host.CWD)
            })
        }
    })

    e.Run()
}
```

Build with `go build -o hello .`, drop the binary + an `extension.json`
into `$ZUT_HOME/extensions/hello/`.

`OnHello` is optional. Use it when configuration or registrations need
host metadata such as `HostInfo.CWD`, `Provider`, `Model`, `ZutVersion`,
`ExtensionDir`, or `DataDir`. The SDK sends `hello`, waits for
`hello_ack`, runs `OnHello`, announces registrations, then sends `ready`.

Session-aware extensions can subscribe to `session_opened`,
`session_switched`, `session_forked`, and `session_compacted`; `Event.Session`
identifies the active branch and `Event.State` contains that extension's
latest persisted snapshot. A tool can return opaque state with
`result.Details = ext.JSONDetails(value)`.

`TurnStartDecision.Context` supplies hidden bounded context for the next model
request. `SetStatus`, `SetWidget`, and their clear methods update persistent
interactive chrome without entering the transcript.

For Go extensions, use the SDK constants so the host can choose the responsive
layout without extension-specific terminal code:

```go
e.SetWidget("plan", ext.WidgetPositionRightBar, "Tasked phases", lines)
e.ClearWidget("plan")
```

The SDK has four interceptor hooks, all optional:

```go
// e is the *ext.Extension returned by ext.New(...).

// Refuse calls or rewrite args before they run.
e.InterceptToolCall(func(tool string, args json.RawMessage) (bool, string) {
    if tool == "bash" { /* inspect args, return false, reason */ }
    return true, ""
})

// Richer variant: returns ToolCallDecision so you can also rewrite
// args via ModifiedArgs.
e.InterceptToolCallX(func(tool string, args json.RawMessage) ext.ToolCallDecision {
    return ext.ToolCallDecision{
        ModifiedArgs: json.RawMessage(`{"command":"echo GUARDED","timeout":30}`),
    }
})

// Block the next turn before the model is called.
e.InterceptTurnStart(func(step int) ext.TurnStartDecision {
    if time.Now().Hour() < 9 { return ext.TurnStartDecision{Block: true, Reason: "outside business hours"} }
    return ext.TurnStartDecision{}
})

// Scrub or rewrite the assistant's final text before the user sees it.
e.InterceptAssistantMessage(func(text string) ext.AssistantMessageDecision {
    return ext.AssistantMessageDecision{
        ReplaceText: strings.ReplaceAll(text, "SECRET", "[redacted]"),
    }
})
```

See:
- `examples/extensions/hello/` — slash commands
- `examples/extensions/clock/` — slash commands in plain Node, no SDK
- `examples/extensions/weather/` — LLM-callable tool
- `examples/extensions/guard/` — event subscriptions + tool-call
  interception (refuses dangerous bash patterns)
- `examples/extensions/todo/` — interactive persistent panel + tool
- `examples/extensions/tasked-phases/` — spec, phased checklist, persistent tool, and `/phases` panel
- `examples/extensions/scratchpad/` — source-run TypeScript commands + tool

The `tasked-phases` example bundles its companion skill under
`examples/extensions/tasked-phases/skills/` and declares it in
`extension.json`. Installing the extension therefore installs the workflow
skill automatically. The standalone copy at `examples/skills/tasked-phases/`
remains useful for project-local skill installation. Its state is restored per
session branch when zut session persistence is enabled; the extension keeps a
project-file fallback for hosts that do not persist sessions.

### Hot reload

Type `/reload-ext` in the TUI to tear down every running extension
subprocess, re-read the manifests from disk, and respawn the set.
The agent's tool registry is rebuilt automatically, so freshly-
registered extension tools become callable without restarting zut.
Useful while developing an extension: edit, save, `/reload-ext`,
done. Explicit `--ext` paths are remembered and reloaded alongside
discovered extensions. The temporary reload status reports each load
error in red, including its message, and dismisses itself after five
seconds. Extension failures from startup and `/reload-ext` remain in the
scrolling chat until a successful reload or `/clear`, without being sent to
the model or saved in the session transcript. Subprocess startup and
handshake errors include the exact stderr log path for the failed extension.
If a subprocess exits before sending `hello`, the error also reports its exit
status or terminating signal.

### TypeScript / Python

These SDKs aren't in the main repo yet; the wire format is small
enough that a `~30 line` raw script gets you started in either
language. See the [Quick start](#quick-start) Python example for the
shape. SDK packages will land in follow-up commits.

## Security

Extensions run with **the user's full filesystem and network
permissions**. Treat installing an extension the same as installing
any other binary on your machine.

`zut ext install <git-url>` clones from any URL you give it. There's
no sandbox in v1; if you need isolation, install only extensions you
trust or run zut under your platform's sandboxing tool (`bwrap` /
`sandbox-exec` / AppContainer).

## Roadmap

Phase 1 (shipped):
- [x] subprocess lifecycle + hello handshake
- [x] `register_command` + `command_invoked`
- [x] `notify` + `clear_notes` + structured terminal alerts
- [x] `zut ext` CLI

Phase 2 (shipped):
- [x] `register_tool` + `tool_call` + `tool_result`
- [x] `ready` sentinel for safe agent-registry build timing
- [x] tool result attribution surfaces extension name in details

Phase 3 (shipped):
- [x] event subscriptions (`session_start`, `session_opened`,
      `session_switched`, `session_forked`, `session_compacted`, `turn_start`,
      `turn_end`, `tool_call`, `assistant_message`)
- [x] tool-call interception (block before execution)

Phase 4 (shipped):
- [x] interception for `turn_start` and `assistant_message` (in
      addition to `tool_call`)
- [x] modify tool args mid-flight via `modified_args`
- [x] rewrite user-visible assistant text via `replace_text`
- [x] `/reload-ext` slash command (hot-reload without restarting zut)

Phase 5 (shipped):
- [x] session/branch identity in `hello_ack` and lifecycle events
- [x] session open/switch/fork/compaction notifications
- [x] opaque extension state persisted with session branches

Phase 6 (shipped):
- [x] hidden bounded context returned from `turn_start` interception
- [x] persistent extension status and widget frames
- [x] manifest-declared bundled skill discovery with safe path validation

Future (no firm timeline):
- [ ] TypeScript and Python SDK packages (currently the wire format
      is stable enough to hand-roll, see the Python quick-start)
- [ ] HTTP / WebSocket transport variants (today: subprocess stdio)
- [ ] per-extension permission scopes (today: full user privileges)
