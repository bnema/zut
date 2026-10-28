# MCP servers

zut includes a native [Model Context Protocol](https://modelcontextprotocol.io)
client. Configured servers start once per zut process, and their tools appear
to the model as ordinary tools named `mcp__<server>__<tool>`.

## Configuration

Servers are read from two JSON files. A project entry replaces a global entry
with the same name:

| File | Scope |
| --- | --- |
| `$ZUT_HOME/mcp.json` | Global |
| `<cwd>/.zut/mcp.json` | Project |

> **Executable configuration.** zut starts every configured stdio command and
> contacts every HTTP endpoint when it launches, before any model turn. There
> is no trust prompt, `--no-yolo` does not gate startup, and servers are not
> sandboxed. Review a repository's `.zut/mcp.json` before running zut in it, or
> pass `--no-mcp`.

The format matches Claude Desktop and Claude Code:

```json
{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "."]
    },
    "docs": {
      "type": "http",
      "url": "https://example.com/mcp",
      "bearerTokenEnvVar": "DOCS_MCP_TOKEN"
    }
  }
}
```

| Field | Transport | Meaning |
| --- | --- | --- |
| `type` (or `transport`) | both | `stdio` (default with `command`) or `http` (default with `url`; `streamable-http` is an alias) |
| `command`, `args` | stdio | Executable and arguments |
| `env` | stdio | Extra environment variables |
| `envVars` | stdio | Host variables to pass through |
| `cwd` | stdio | Working directory; relative paths resolve against the zut cwd |
| `url` | http | Streamable HTTP endpoint |
| `headers` | http | Static HTTP headers |
| `envHeaders` | http | Header name → host variable name |
| `bearerTokenEnvVar` | http | Sends `Authorization: Bearer <value>` |
| `enabled` | both | `false` skips the server |
| `required` | both | Abort startup when this server fails |
| `enabledTools`, `disabledTools` | both | Allow-list, then deny-list of raw MCP tool names |
| `startupTimeoutSec` | both | Start + initialize + `tools/list` budget (default 30) |
| `toolTimeoutSec` | both | One `tools/call` budget (default 300) |

`${VAR}` and `${VAR:-default}` are expanded in `command`, `args`, `cwd`, `env`
values, `url`, and `headers` values. A reference to an unset variable without
a default disables only that server; the error names the field and variable,
never the value.

Stdio servers receive a minimal environment: common variables such as `PATH`,
`HOME`, `LANG`, `TMPDIR`, and proxy/CA settings, plus `envVars` and `env`.
Provider API keys and other host variables are not inherited.

The legacy HTTP+SSE transport is not supported; use the server's streamable
HTTP endpoint or a stdio command.

### Authentication

zut has no built-in OAuth sign-in. OAuth servers return 401, and zut reports
when the server advertises an OAuth requirement.

- Use an access token through `bearerTokenEnvVar`, `envHeaders`, or `headers`.
- Or connect through a stdio bridge such as `mcp-remote`:

  ```sh
  zut mcp add notion --startup-timeout 180 -- npx -y mcp-remote https://mcp.notion.com/mcp
  zut mcp status notion
  ```

  The sign-in URL is printed live on stderr as `mcp[notion]: …`. Open it
  manually if no browser launches. The restricted child environment omits
  `DISPLAY` and `WAYLAND_DISPLAY`; use `--env-var` to pass them through if
  needed. Later starts reuse mcp-remote's cached token.

## Managing servers

```sh
zut mcp add fs -- npx -y @modelcontextprotocol/server-filesystem .
zut mcp add docs --url https://example.com/mcp --bearer-token-env-var DOCS_MCP_TOKEN
zut mcp add local --project --env KEY='${LOCAL_KEY}' -- ./bin/server
zut mcp list            # name, transport, target
zut mcp get docs        # entry with env and header values hidden
zut mcp status [NAME]   # connect, then list each tool name
zut mcp remove docs [--project]
```

`zut mcp add` writes the global file unless `--project` is given. Files are
written with `0600` permissions because entries may hold secrets.

In the interactive TUI, `/mcp` shows connection state, tool counts, and
startup errors.

## Runtime behavior

- Servers connect in parallel at startup. A failure is reported (stderr in
  headless modes, the startup panel in the TUI) and zut continues without that
  server unless it is `required`. A server that failed at startup exposes no
  tools for the rest of the run; fix it (`zut mcp status NAME`) and restart
  zut. A server that started and later dropped (a crashed stdio process or an
  expired HTTP session) reconnects on the next call to one of its tools; the
  failed call itself is not retried.
- zut negotiates protocol `2025-06-18` and accepts `2025-03-26` and
  `2024-11-05`. It answers `ping` and `roots/list` (the zut cwd) and follows
  `tools/list` pagination.
- `notifications/tools/list_changed` refreshes the server's tool list. New
  definitions reach the model the next time zut rebuilds its tool registry
  (for example after `/reload-ext` or a model switch).
- Tool names are sanitized to `[A-Za-z0-9_-]` and limited to 64 characters.
  Names that collide or overflow receive a stable 8-character hash suffix.
- Input schemas are normalized to an object root without top-level
  `oneOf`/`anyOf`/`allOf`, which some providers reject.
- Results map text and image blocks directly. Embedded text resources are
  inlined, resource links and audio are described, and `structuredContent` is
  used when no content blocks are returned. Text is capped at 50 KiB per result.
  `isError` results are reported to the model as tool errors.
- When a server offers resources, the `mcp_list_resources` and
  `mcp_read_resource` tools are added.
- Server-provided `instructions` are added to the system prompt, marked as
  server-provided. Treat MCP output as untrusted data.
- Cancelling a turn sends a best-effort `notifications/cancelled`. Exiting zut
  closes HTTP sessions and stops stdio servers with their process group
  (Unix) or job object (Windows). A descendant that starts its own session
  (`setsid`) on Unix is not stopped.
- Errors and status output strip terminal control sequences, URL credentials
  and query strings, and configured `env`, header, and bearer-token values.
  Live stderr from `zut mcp status` keeps the query of OAuth authorization-request
  URLs. Secrets passed through `args` are not redacted.

## Tool selection and confirmation

- `--no-mcp` skips MCP for one run. `--no-tools` also disables it.
- With `--tools`, add `mcp` for every MCP tool, or list exact names such as
  `mcp__docs__search`.
- Packaged agents (`zut run`), bot runs, and SDK runtimes never start MCP
  servers.
- `--no-yolo` confirmation covers MCP tools like any other tool.
- Tool annotations such as `readOnlyHint` are hints from the server, not a
  security boundary.

## Orchestrator mode and subagents

All agents in one zut process share the same MCP connections.

- **Headless `--orchestrate`**: the manager-only orchestrator holds only MCP
  tools annotated `readOnlyHint: true` plus the resource tools, for research,
  still limited by any `--tools` selection. Its workers receive every MCP tool,
  so actions that change external state are delegated.
- **Resident subagents** inherit MCP tools like other child-safe tools.
  `subagents.allowed_tools` and profile `tools:` lists accept `mcp` to grant
  every MCP tool, or exact `mcp__<server>__<tool>` names.
- A resumed child whose MCP server is no longer configured continues without
  that tool.
