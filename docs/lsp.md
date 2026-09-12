# LSP and linter support

zut exposes a built-in `lsp` tool for code intelligence and diagnostics. It
communicates with stdio language servers through JSON-RPC and can also run
configured command-line linters without a shell.

## Configuration

zut loads JSON configuration from the following layers, in increasing
precedence:

- `$ZUT_HOME/lsp.json`
- `<cwd>/.pi/lsp.json`
- `<cwd>/.omp/lsp.json`
- `<cwd>/lsp.json`, `<cwd>/.lsp.json`, and `<cwd>/.zut/lsp.json`

A file may put providers below `servers`, `lspServers`, or `linters`, use a
flat provider map, or use the `providers` array:

```json
{
  "servers": {
    "gopls": {
      "command": "gopls",
      "fileTypes": [".go"],
      "rootMarkers": ["go.mod", "go.work"]
    },
    "eslint": {
      "kind": "cli",
      "command": "eslint",
      "args": ["--format", "json"],
      "parser": "eslint-json",
      "mode": "files",
      "fileTypes": [".js", ".jsx", ".ts", ".tsx"],
      "rootMarkers": ["eslint.config.js", ".eslintrc"]
    }
  }
}
```

The `providers` array accepts built-in names and objects with `id`, `kind`,
`command`, `args`, selectors, root markers, and nested `lsp`/`cli` options.
Unknown provider metadata is ignored. Set `autoDetect` to `false` to
keep only providers named or defined by the configuration file.

Server overrides merge onto zut's built-in definitions. Built-ins cover common
Go, Rust, TypeScript/JavaScript, Python, C/C++, YAML, JSON, and shell servers,
plus ESLint, golangci-lint, and Ruff CLI providers. A provider is selected
when it is enabled and its file type/root markers match the workspace. Command
lookup prefers project-local bin directories and then `PATH`; missing commands
are reported by `status` or the request result. Set `disabled` to `true` to
turn off a provider.

Useful server fields are:

| Field | Meaning |
| --- | --- |
| `kind` | `lsp` (default) or `cli` |
| `command`, `args` | Executable and arguments |
| `fileTypes` | Extensions, basenames, or language IDs handled by the provider |
| `languageId` | Optional LSP language ID sent on `textDocument/didOpen`; inferred when omitted |
| `rootMarkers` | Files identifying a workspace root |
| `isLinter` | LSP provider used for diagnostics, not navigation/refactors |
| `parser` | CLI output parser: `eslint-json`, `golangci-lint-json`, `ruff-json`, `sarif`, or generic |
| `mode` | CLI provider scope: `files` or `workspace` |
| `settings` | Values returned for `workspace/configuration` requests |
| `initializationOptions` | Values sent during LSP initialization |
| `env` | Extra environment variables for the child process |

CLI providers may use `{files}` as an argument or inside an argument. It is
expanded to project-relative paths touched by the diagnostics request; an
embedded placeholder produces one argument per path. CLI output is capped
before parsing.

## Tool actions

The model calls `lsp` with an `action` and optional arguments such as `path`,
`line`, `column`, `server`, or `query`. Every action has an overall time budget:
`timeout_ms` defaults to 30,000 milliseconds and is capped at 120,000 milliseconds.

- `diagnostics` — merge LSP and CLI diagnostics for a file, glob, or the whole workspace. `run_cli:false` skips command-line linters.
- `definition`, `type_definition`, `implementation`, `references`, `hover` — query a language server at a 1-based line and column.
- `symbols` — return document symbols, or workspace symbols when `query` is supplied and `path` is omitted or `*`.
- `rename` — request a `WorkspaceEdit`; applies it by default, or previews it with `apply:false`.
- `code_actions` — list actions, or apply returned edits with `apply:true`.
- `status` — show configured providers, running state, and diagnostic counts without starting servers.
- `reload` — stop cached LSP processes and reload configuration.
- `capabilities` — inspect initialize capabilities for one or all servers.
- `request` — send a raw JSON-RPC method with JSON `params`.

`--no-lsp` disables the tool for one run. `--tools lsp` selects it when an
explicit tool list is used.

## Diagnostics and context limits

Diagnostics are deduplicated across providers using path, severity, code,
start position, and normalized message. Results are ordered by severity and
location. Repeated
identical messages are represented as one occurrence group with example paths
or lines. Cascades beneath missing-module errors are grouped as `SECONDARY`
items so the root import problem remains visible without repeating a large
error fan-out.

The model-facing summary has a hard diagnostic cap (50 by default, with a
smaller eight-item summary used after writes and edits). Messages are
whitespace-compacted and clipped before formatting. Slow write-time checks are
bounded to a short two-second budget; a successful file write or edit is never
turned into a failed mutation because an LSP or linter is unavailable.

Write and edit diagnostics are enabled by default when LSP is enabled. Applied
LSP rename, code-action, and AST rewrite edits also run the same bounded checks
for up to three modified files within one shared two-second budget. The persisted configuration keys are
`lsp_diagnostics_on_write` and `lsp_diagnostics_on_edit` when an embedding
needs to override the native file-tool defaults.
Repeated post-write diagnostics are tracked per file by severity/code/message
identity, so a line shift does not replay the same error. Clearing a file's
diagnostics allows a later recurrence to be surfaced again.

## Settings

`/settings` provides two independent switches, both enabled by default:

- **lsp in main session** (`lsp_enabled`) controls the main agent and applies
  immediately to the live tool registry.
- **lsp in sub-agents** (`subagent_lsp_enabled`) controls newly spawned subagent
  child processes.

The sub-agent setting does not grant access to a child whose profile or
explicit tool list omits `lsp`. Both settings can also be controlled through
`$ZUT_HOME/config.json`.

## Safety and lifecycle

Language servers and linters run as local child processes. Project and
global LSP configuration is executable local configuration: review configured
commands, arguments, environment variables, and workspace-local binaries before
using it. zut does not add a separate trust prompt or sandbox around those child
processes; it does avoid shell expansion and keeps returned workspace edits
inside the session workspace. The `lsp` tool never executes configured commands
through a shell. Workspace edits returned by
rename and code actions are restricted to the session workspace, reject
resource operations, validate UTF-16 LSP positions, and write through a
temporary file before replacement. Server-initiated `workspace/applyEdit`
requests use the same checks.

LSP clients are cached per workspace and provider for the lifetime of the zut
process. `reload` and process shutdown terminate cached children. Server
stderr is kept out of model-facing tool results; diagnostics and protocol
errors are returned through the established tool result channel.
