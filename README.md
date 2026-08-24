# zut

> [!NOTE]
> **Fork notice:** `zut` is an independent fork of [zot](https://github.com/patriceckhart/zot) adapted for my own workflow but feel free to use it too.

## What is it?

Yet another coding agent harness, lightweight and written (vibe-slopped) in go.

- one static binary.
- built-in providers for Anthropic, OpenAI/Codex/Responses, Kimi, DeepSeek, Google Gemini/Vertex, GitHub Copilot, Bedrock, Azure OpenAI, OpenRouter, Groq, Cerebras, xAI, Together, Hugging Face, Mistral, Moonshot, Z.AI, Xiaomi, MiniMax, Fireworks, Vercel AI Gateway, OpenCode, Cloudflare AI, and Ollama/local models.
- thirteen core built-in tools (read, write, edit, bash, create_worktree, grep, lsp, web_search, web_open, web_find, web_click, update_goal, and schedule; `schedule` is interactive-only); the conditional `skill` tool is available when skills are enabled. See [docs/web-search.md](docs/web-search.md) for public-web egress and availability boundaries.
- three run modes (interactive tui, print, json).
- built-in telegram bot.
- extensions in any language via subprocess + json-rpc. None installed by default; opt in with `zut ext install` or `zut --ext`. See [docs/extensions.md](docs/extensions.md).
- user and extension themes via JSON; see [docs/themes.md](docs/themes.md).
- standing instructions via `AGENTS.md` files (global and per-project); see [Persistent instructions](#persistent-instructions-agentsmd).
- reusable instructions via `SKILL.md` files; see [docs/skills.md](docs/skills.md).
- named subagent profiles from `~/.agents/agents/*.md`; see [docs/subagents.md](docs/subagents.md).
- portable agents from local directories, `.zut` files, or temporary public GitHub downloads; see [docs/zutfiles.md](docs/zutfiles.md).

## Install

### One-liner (macOS, Linux)

```bash
curl -fsSL https://raw.githubusercontent.com/bnema/zut/main/install.sh | bash
```

Detects your OS and architecture, downloads the latest release from GitHub, verifies the SHA-256 against the release's `checksums.txt`, extracts the binary, and drops it in `/usr/local/bin`, `~/.local/bin`, or `~/bin`, whichever is writable first. Pass a version or prefix to pin:

```bash
curl -fsSL https://raw.githubusercontent.com/bnema/zut/main/install.sh | bash -s -- v0.1.0 ~/bin
```

### One-liner (Windows, PowerShell)

```powershell
iwr -useb https://raw.githubusercontent.com/bnema/zut/main/install.ps1 | iex
```

Drops `zut.exe` into `$HOME\bin` and adds it to the user PATH if missing. Open a fresh terminal afterwards.

### go install

```bash
go install github.com/bnema/zut/cmd/zut@latest
```

The installed binary reports the tagged module version and supports `zut update`.

### From source

```bash
git clone https://github.com/bnema/zut
cd zut
make help         # list the important developer commands
make build        # produces ./bin/zut (0.0.0-dev by default)
make install      # install the current checkout with go install
make go-install   # install the latest published module version
```

`make go-install GO_INSTALL_VERSION=v0.1.0` installs a specific published
version. `make install` is different: it installs the current checkout as a
development build.

### Prebuilt binaries

Every release on the [releases page](https://github.com/bnema/zut/releases) ships archives for Linux, macOS, and Windows on amd64 and arm64 (except windows/arm64), plus a `checksums.txt` file. Download, verify, `chmod +x`, and drop on your `$PATH`.

## Authenticate

The easiest way is to just run `zut` and type `/login`. The TUI opens even without credentials and walks you through a browser-based login flow.

### Credential lookup order

1. `--api-key` flag
2. provider-specific env var (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `KIMI_API_KEY`, `MOONSHOT_API_KEY`, `DEEPSEEK_API_KEY`, `GEMINI_API_KEY`, `GOOGLE_API_KEY`, `GROQ_API_KEY`, `OPENROUTER_API_KEY`, `MISTRAL_API_KEY`, `XAI_API_KEY`, `CEREBRAS_API_KEY`, `TOGETHER_API_KEY`, `HF_TOKEN`, `ZAI_API_KEY`, `XIAOMI_API_KEY`, `MINIMAX_API_KEY`, `FIREWORKS_API_KEY`, `AI_GATEWAY_API_KEY`, `COPILOT_GITHUB_TOKEN`, `GITHUB_COPILOT_TOKEN`, and others for provider-specific backends)
3. `$ZUT_HOME/auth.json` (API key or OAuth token; mode 0600)

`$ZUT_HOME` defaults to:
- All platforms: `$XDG_STATE_HOME/zut` when `XDG_STATE_HOME` is set
- macOS fallback: `~/Library/Application Support/zut`
- Linux fallback: `~/.local/state/zut`
- Windows fallback: `%LOCALAPPDATA%\zut`

### API keys from commands

To keep an API key in a password manager instead of `auth.json`, configure an `api_key_command` for the provider:

```json
{
  "anthropic": {
    "api_key_command": {
      "program": "op",
      "args": ["read", "op://Work/Anthropic/credential"],
      "timeout_ms": 120000
    }
  }
}
```

For a provider added through `models.json`, put the same credential object under `additional_api_key_creds` using its provider ID. `program` is executed directly, without a shell, so each argument must be a separate `args` entry. `timeout_ms` is optional and defaults to 120 seconds.

zut runs the command only when that provider is selected, not while checking login status or refreshing model catalogs in the background. Successful output is cached in memory for the rest of the zut process and is never written to disk. The command must print one non-empty line to stdout; zut removes trailing CR/LF characters, limits output to 64 KiB, and does not include command output in errors. Saving a normal key through `/login` replaces the command configuration, and `/logout` removes it.

Treat `auth.json` as executable configuration: anyone who can modify it can cause zut to run a program under your user account. zut does not interpret `!` prefixes or execute command strings through a shell.

### `/login` flow

Run `zut` and type `/login`. Pick one of two methods:

- **API key**: a small local web server starts on `127.0.0.1:<free-port>`, your browser opens a form, you pick a provider from the full API-key provider list, paste the key, and zut saves it to `auth.json` if accepted. Providers with a lightweight model-list endpoint are probed before saving; provider backends that need extra project/account env vars are saved directly.
- **Subscription**: use your Claude Pro/Max, ChatGPT Plus/Pro, Kimi Code, SuperGrok/X Premium, or GitHub Copilot subscription. DeepSeek and Google Gemini do **not** have a subscription login path. For those, use the API-key flow.
  - Anthropic and OpenAI pin the browser callback to fixed provider-specific ports (`localhost:53692` for Anthropic, `localhost:1455` for OpenAI) because those are the only ports their auth servers will redirect to.
  - Anthropic uses the Claude Code OAuth flow. Messages go to `api.anthropic.com` with a bearer token and the Claude Code identity headers.
  - OpenAI uses the Codex CLI OAuth flow. Messages go to `chatgpt.com/backend-api/codex/responses` with the `chatgpt-account-id` extracted from the returned id_token.
  - Kimi uses the Kimi Code device-code OAuth flow. zut opens the verification URL, polls until you approve it in the browser, then sends messages to `api.kimi.com/coding/v1` with the Kimi Code identity headers.
  - xAI uses a device-code OAuth flow. zut opens a prefilled authorization URL, polls for approval, and uses the resulting token with the xAI API.
  - GitHub Copilot uses GitHub's device-code login flow. zut stores the GitHub access token and exchanges it for short-lived Copilot inference tokens on demand.

> **Note on subscription login.** The OAuth client IDs used are the ones published in Anthropic's Claude Code CLI, OpenAI's Codex CLI, Kimi Code CLI, xAI's device flow, and GitHub Copilot's device-code flow. Reusing them from a third-party tool may be against their terms of service and may be revoked at any time. Use it at your own risk; the API-key flow is the safe default.

### Token refresh

OAuth access tokens are short-lived (Anthropic ~8h, OpenAI ~30d; Kimi, xAI, and GitHub Copilot also use refresh/exchange flows). zut refreshes or exchanges them automatically:

- At every credential lookup, zut checks the stored `expiry` and, if past it (with a 60s safety margin), hits the provider's `oauth/token` endpoint with the stored `refresh_token`, persists the new `access_token`, `refresh_token`, and `expiry` back to `auth.json`, and hands the fresh token to the client.
- The telegram bridge additionally refreshes once per turn so a bot that runs for days keeps working without manual intervention.
- If the refresh itself fails (the `refresh_token` was revoked, or the account was logged out everywhere), the error bubbles up to the caller: the TUI shows it in the status line, the bot replies with it in your DM. Run `/login` to get a fresh token pair.

All data lives under `$ZUT_HOME`:

```
$ZUT_HOME/
├── config.json         # last-used provider/model/theme and persistent settings
├── auth.json           # api keys and oauth tokens (mode 0600)
├── sessions/           # jsonl transcripts, one dir per cwd
├── models-cache.json   # live /v1/models discovery cache (6h ttl)
├── AGENTS.md           # optional: global instructions appended to the prompt
├── SYSTEM.md           # optional: replaces the built-in identity; addenda remain
├── skills/             # optional: user SKILL.md files
├── themes/             # optional: user theme JSON files
├── extensions/         # installed extensions, one dir per extension
└── logs/               # app log files
```

Drop a `SYSTEM.md` in `$ZUT_HOME` to replace the built-in identity and docs guidance for every run. Existing append addenda, including `AGENTS.md`, skills, and enabled Ponytail coding mode, remain layered on top. `--system-prompt` wins per invocation unless a selected replace-mode subagent profile supplies the child's identity. Delete the file to revert to the default identity.

Ponytail coding mode is enabled by default, including when an existing `config.json` does not contain a `ponytail_enabled` field. When disabled, its compact guidance is omitted from resolved system prompts; when enabled, the guidance tells the model to apply itself only to engineering work. The setting persists its explicit on/off choice in `$ZUT_HOME/config.json` and can be changed from `/settings`. It is advisory guidance only: it does not change tool permissions, confirmations, jail behavior, or sandboxing.

A selected subagent profile with `systemPromptMode: replace` supplies the child's base identity even when `--system-prompt` was provided for the parent run. Global append addenda, including enabled Ponytail guidance, still follow their normal inclusion rules.

### HTTP proxy

To route zut-managed HTTP and HTTPS requests through one proxy, add `http_proxy` to `$ZUT_HOME/config.json`:

```json
{
  "http_proxy": "http://127.0.0.1:7890"
}
```

The setting is applied at startup to both HTTP and HTTPS traffic. Existing `HTTP_PROXY`, `HTTPS_PROXY`, `http_proxy`, and `https_proxy` environment variables take precedence for their corresponding protocol. `NO_PROXY` and `no_proxy` continue to control bypasses. Restart zut after changing the config file. If the URL contains proxy credentials, prefer protected environment variables because `config.json` is not a credential store.

## Persistent instructions (AGENTS.md)

Use `AGENTS.md` to give zut standing instructions that layer **on top of** the default system prompt, without replacing it. This is the friendliest way to shape behavior (for example, taming local models that jump straight to code edits) because it adds guidance rather than taking over the base identity the way `SYSTEM.md` does.

zut discovers `AGENTS.md` files automatically at startup and loads them in this order:

1. `$ZUT_HOME/AGENTS.md` (global, machine-wide instructions that apply to every project).
2. Every `AGENTS.md` from the filesystem root down to the current working directory. More specific (deeper) files may override earlier ones. This includes `~/AGENTS.md` when the working directory is inside your home directory.

All discovered files are appended to the prompt in that order, so a global baseline can be refined per project. To list the active Zutfile agent, loaded context paths, extensions, and user-installed skills above the interactive transcript, enable **show loaded resources at startup** in `/settings`. The agent section appears only for `zut run`, the setting is off by default, built-in skills are omitted, and the displayed lists are not sent to the model or stored in the session transcript.

For a general, non-project-specific instruction set, put your rules in `$ZUT_HOME/AGENTS.md`, for example:

```markdown
Treat questions and discussions as requests for explanation. Do not edit files or run tools unless explicitly asked to make a change. Ask before modifying code.
```

`AGENTS.md` vs the other mechanisms:

| Mechanism | Scope | Effect |
|---|---|---|
| `$ZUT_HOME/AGENTS.md` | global, every run | appended to the default prompt |
| `./AGENTS.md` (and parent dirs) | project | appended to the default prompt |
| `$ZUT_HOME/SYSTEM.md` | global, every run | replaces the built-in identity and docs guidance; append addenda remain |
| `--append-system-prompt <text>` | single run | appends for one invocation (repeatable) |
| `--system-prompt <text>` | single run | replaces the built-in identity and docs guidance for one invocation; append addenda remain |

> **Note:** zut does not read a `CLAUDE.md` instruction file. The only Claude-compatible thing it picks up is skills under `.claude/skills/`. If you are migrating from Claude Code, move that content into `AGENTS.md` (global or per-project) and zut will use it.

## Changelog on update

The first time you launch a newer zut binary, the TUI shows the GitHub release notes once in a dismissible overlay. Press any key to close. The version is recorded in `config.json`'s `last_changelog_shown` so the same release notes never reappear. Fresh installs don't see a changelog (no upgrade has happened yet). Development builds (`0.0.0-dev`) skip both the update and changelog requests and print `dev version detected; skipping update check`. For release builds, the fetch is best-effort: a network failure or a missing release page silently skips, with another attempt on the next launch.

## Usage

```bash
zut                              # interactive tui
zut "fix the failing test"       # tui, pre-filled prompt
zut -p "list all go files"       # print final text, exit
cat README.md | zut "summarize this text" # pipe input and infer print mode
zut -p --stats stats.json "task" # print final text and write generation stats
zut --json "refactor main.go"    # newline-delimited json events, exit
zut -p --orchestrate --provider openai --model gpt-5 "implement and synthesize"
zut --continue                   # resume the most recent session for this cwd
zut --resume                     # pick a session to resume
zut --resume <UUID>              # resume a persisted session by UUID
zut --list-models                # show supported models
zut --help
```

Print-mode stats contain `provider`, `model`, `prompt_tokens`, `reasoning_tokens`, `generated_output_tokens`, and `elapsed_ms`. Counts cover all model turns triggered by the prompt, including tool loops. Prompt tokens include cache reads and writes. `reasoning_tokens` is `null` when the provider does not report a separate count; in that case `generated_output_tokens` is the provider's total output count and may include reasoning. Elapsed time covers the agent run, not startup and credential resolution. The file is written only after a successful run.

## Flags

| Flag | Description |
|---|---|
| `--provider <id>` | Pick the provider (for example `anthropic`, `openai`, `openai-codex`, `kimi`, `google`, `github-copilot`, `groq`, `openrouter`, `amazon-bedrock`, `ollama`; see Providers). |
| `--model <id>` | Pick the model (see `--list-models`). |
| `--api-key <key>` | Override the API key. |
| `--base-url <url>` | Override the provider base URL (tests, self-hosted). |
| `--insecure` | Skip TLS certificate verification for the explicit `--base-url` endpoint or a `baseUrl` defined for a user model in `models.json` (self-signed local/internal inference servers). Built-in providers, auth, and model discovery keep normal TLS verification. |
| `--system-prompt <text>` | Replace the default system prompt for this run (also overrides `$ZUT_HOME/SYSTEM.md`). |
| `--append-system-prompt <text>` | Append text to the system prompt (repeatable). |
| `--reasoning off\|minimum\|low\|medium\|high\|xhigh\|max` | Set the reasoning level on supported models (default: off). `max` is a separate opt-in tier above `xhigh`. |
| `--stats <path>` | With `-p`/`--print`, write generation statistics as JSON; incompatible with `--orchestrate`. |
| `--orchestrate` | In print, stream, or JSON mode, enable bounded headless delegation and final synthesis. It is not implicit. |
| `-c`, `--continue` | Resume the latest session for this cwd. |
| `-r`, `--resume [UUID]` | Pick a session to resume, or resume a persisted session by UUID. |
| `--session <path>` | Resume a specific session file. |
| `--no-session` | Don't read or write session files. |
| `--cwd <path>` | Use `<path>` as the working directory. |
| `--no-tools` | Disable all tools. |
| `--no-lsp` | Disable the built-in LSP/linter tool for this run. |
| `--tools <csv>` | Only enable the listed tools. Include `lsp` to select it explicitly. An explicit list must include `web_search` to retain or opt into the complete public-web capability (`web_search`, `web_open`, `web_find`, and `web_click`) for that invocation; explicitly including it overrides the persisted web-search setting. |
| `--max-steps <n>` | Cap agent loop iterations (default: unlimited). |
| `-e`, `--ext <path>` | Load an extension from `<path>` for this run (repeatable; wins against installed extensions of the same name). |
| `--no-ext` | Skip extension discovery for this run. `--ext` still works on top, so `--no-ext --ext ./x` runs only `x`. |
| `--no-skill` | Disable all skills, including built-ins. No `skill` tool is registered and the system prompt has no skill manifest. |
| `--no-context-files`, `-nc` | Disable discovery and loading of global and project `AGENTS.md` files. |
| `--no-yolo` | Confirm every tool call before it runs (interactive TUI only). A dialog shows the tool name and a one-line preview of its args, while edit calls show the proposed diff in the tool panel, with four choices: yes, yes-always-this-tool-this-session, yes-always-this-session, no. Press `/` while the dialog is focused to run a slash command first; closing its input or child dialog returns to the pending confirmation. Ignored with a stderr warning in print / json / rpc modes, where tools still run freely so scripts and automation keep working. |

## Tools

- `read`: read text files, or inline images (PNG, JPEG, GIF, WebP).
- `write`: create or overwrite files, making parent directories as needed.
- `edit`: one or more exact-match replacements in an existing file.
- `grep`: search a file or directory for a regular expression using ripgrep when available, with a portable grep fallback. Search output is bounded and follows the active filesystem read scope; it never invokes a shell. The fallback's regular-expression syntax and ignore/hidden-file behavior can differ from ripgrep.
- `bash`: run a command in the session cwd with merged stdout/stderr. Every call must include a positive integer `timeout` in seconds; calls without it or with `timeout <= 0` are rejected, and the deadline is always enforced. On Unix, zut uses `/bin/bash -c` when available, then `bash -c` from `PATH`, and falls back to POSIX `/bin/sh -c` when Bash is unavailable. On Windows, it uses `cmd /C`. macOS ships Bash 3.2 by default, so newer Bash features may be unavailable.
- `create_worktree`: create a new branch from the current `HEAD` and check it out persistently. A repository with no configured root and no `.worktrees` directory first returns bootstrap guidance without making changes; the agent asks whether to use the repository default or an external root, then retries with `bootstrap_root`. The choice is saved privately in local Git config. Choosing `.worktrees` creates `<repository-root>/.worktrees/<branch>` and adds `/.worktrees/` to the root `.gitignore`; an absolute external root leaves tracked repository files and the root `.gitignore` unchanged. The tool never copies uncommitted or ignored files, and it refuses an existing branch or worktree path.
- `lsp`: query configured language servers and linters for diagnostics, definitions, references, hover information, symbols, renames, code actions, capabilities, and raw protocol requests. LSP servers use stdio JSON-RPC; project `lsp.json` files can add or override servers and CLI linters. Diagnostics are bounded, sorted, deduplicated by path/severity/code/start position, and repeated issues are grouped before they reach the model. See [docs/lsp.md](docs/lsp.md).
- `web_search`: search DuckDuckGo HTML and return bounded source titles, URLs, snippets, and opaque references. `web_open`, `web_find`, and `web_click` respectively open a referenced page, find literal text in an opened page, and follow a displayed link. The four tools are enabled together by default for normal CLI sessions and unavailable to bot, default SDK, and packaged-agent runs. See [docs/web-search.md](docs/web-search.md).
- `update_goal`: let the main agent start its own mission when none is active, set the next bounded goal within an active mission, or mark the active goal `done` or `blocked`. Pausing, resuming, and clearing missions remain user-controlled through `/goal`.
- `schedule`: add, list, or cancel recurring five-field cron prompts for the current interactive session. A task captures the machine's current timezone when it is created. While this zut process remains open, a due task waits for an active target turn to finish, or resumes an inactive target session in the background; its output is recorded in that target transcript. Resumed background tasks also show a completion notification in the current TUI. Scheduling is in-memory only: quitting zut cancels pending tasks, and schedules do not survive restart or reboot. Print, stream, JSON, and RPC modes do not expose this tool.

When the sandbox is on (see `/jail`), filesystem tools, `grep`, and LSP workspace edits refuse paths outside the session cwd. `create_worktree` also requires its repository root, configured worktree destination, optional `.gitignore`, and Git metadata to remain inside the jail. Jail does not sandbox web-search network egress.

## Modes

- **Interactive** (default): chat TUI with streaming output, spinner, cost meter, slash commands. Independent views such as `/settings`, `/subagents`, pickers, and confirmations open in a focused floating pane while the live main view remains visible and dimmed behind it. On terminals narrower than 80 columns, the pane becomes a full-width bottom drawer.
- **Print**: `zut -p "prompt"` runs the agent to completion and writes only the final assistant text to stdout.
- **Stream**: `zut --stream "prompt"` runs without the TUI and writes assistant text to stdout as it arrives. Tool activity goes to stderr.
- **Piped input**: stdin implies print mode when no explicit mode is selected. Print, stream, and JSON modes prepend piped input to the positional prompt, separated by a newline. For example, `cat README.md | zut "summarize this text"`.
- **JSON**: `zut --json "prompt"` emits one JSON object per agent event to stdout, newline-delimited. The schema is documented in [docs/rpc.md](docs/rpc.md).
- **RPC**: `zut rpc` runs as a long-lived child process; commands in on stdin, events and responses out on stdout, both as NDJSON. Designed for embedding zut in third-party apps written in any language. See [docs/rpc.md](docs/rpc.md) for the wire schema and `examples/rpc/{python,node,shell,go}` for working clients.

### Headless orchestration

`--orchestrate` explicitly enables bounded resident delegation for `-p`/`--print`, `--stream`, or `--json`; ordinary headless runs never delegate implicitly. This is a strict manager-only mode: the parent divides work into non-overlapping child scopes, coordinates active workers, and yields rather than implementing or repeating delegated work. The parent needs `subagent_spawn`: it is available with the default tool set, or must be named in `--tools subagent_spawn` (and is disabled by `--no-tools`). A packaged agent's `PermissionSet` does not expose `subagent_spawn`, so packaged agents cannot use orchestration. Interactive and RPC modes do not support this flag, and `--stats` cannot be combined with it.

Resident children inherit the parent CLI provider, model, and reasoning selection unless a selected profile or spawn explicitly overrides it. They do not inherit the parent's strict orchestrator role. Profile metadata from [docs/subagents.md](docs/subagents.md) is provided to the parent through `[subagents_list]` before selection; only the selected profile body and child-specific provider/model/reasoning configuration are loaded or applied afterward. Headless orchestration waits for every accepted child before its completion wave, while `required:true` additionally creates a durable obligation that survives terminal failure or interruption. Print mode writes only the final synthesis; stream mode renders every parent turn to stdout while tool diagnostics remain on stderr; JSON mode retains every parent event as parseable JSONL, including completion updates as existing `user_message` events. Child output is evidence for the next parent turn, not a host log.

The resident manager enforces global fair concurrency (six turns by default), optional queue timeout, required-result gating, and cancellation. A headless invocation permits at most 32 completion follow-up waves. Cancellation stops the parent and all resident children through the normal shutdown path. Children cannot spawn descendants. See [docs/subagents.md](docs/subagents.md) for the profile manifest and lifecycle.

When an initial print, stream, or JSON request exceeds the provider context window, zut compacts the existing transcript and continues the already-appended prompt once. Print still writes only the recovered final text, stream keeps assistant text on stdout and tool diagnostics on stderr, and JSON suppresses the recoverable first terminal error so stdout remains JSONL for the successful turn. A compaction failure or a second context-window error remains terminal; this is not a general autonomous-follow-up policy. The same one-shot recovery applies to resident child turns. It does not apply to RPC, SDK, bot, or Telegram requests.

## zutfile agents

A zutfile packages an agent's instructions, skills, requirements, and enforced tool permissions as a shareable agent. Run one from a local directory, a packed `.zut` artifact, a short name, or directly from a public GitHub repository:

```bash
zut run ./my-agent
zut run ./my-agent.zut
zut run frkr/zot-archify
zut run acme/agents/code-reviewer --cwd /path/to/project
```

Single-part names resolve only to a matching local directory or `.zut` archive. Anyone can publish an agent as a public GitHub repository and run it as `owner/repository`, or publish a collection and run an agent directory as `owner/repository/agent`. zut has no built-in owner, official collection, configured registry, or allowlist. For GitHub sources, zut downloads the repository archive into a temporary directory, validates and runs the selected agent, then removes the downloaded source when the command exits. Agent data, consent receipts, and sessions still persist under `$ZUT_HOME`. See [docs/zutfiles.md](docs/zutfiles.md) for authoring, permissions, packaging, and current limitations.

## Embedding

Two ways to drive zut from another program:

- **Go in-process**: import `github.com/bnema/zut/packages/agent/sdk`. One `Runtime` per project; `Prompt(ctx, text, images)` returns a channel of `Event`. `sdk.Config.MaxSteps` is unlimited when zero; embedders preserving the former SDK default should set it to `50` explicitly. Small example in `examples/sdk/`.
- **Any language, out-of-process**: spawn `zut rpc` as a subprocess and exchange newline-delimited JSON over its stdin/stdout. Wire format and event schema in [docs/rpc.md](docs/rpc.md). Reference clients live under `examples/rpc/`.

Both interfaces share the same event schema, so transcripts captured by one can be replayed through the other.

## Slash commands

Slash command names are case-insensitive in the TUI and messaging backends; arguments keep their original case. Type `/` in the TUI to open the autocomplete popup. Available commands:

| Command | Description |
|---|---|
| `/help` | Show key bindings and commands. |
| `/info` | Show the current session ID, full transcript-file path, working directory, provider, and model. |
| `/login` | Log in via API key or subscription (opens a dialog). |
| `/logout [provider]` | Clear credentials for any logged-in provider, or all when omitted. `/logout openai-codex` clears ChatGPT/Codex subscription auth while preserving a public OpenAI API key; `/logout kimi` also disables fallback to the official Kimi Code CLI token until you log in to Kimi through zut again. |
| `/model` | Pick a model from a list (or `/model <id>` to set directly). |
| `/reasoning` | Set the reasoning level for subsequent model calls. |
| `/fast` | Toggle fast mode for subsequent model calls. |
| `/orchestrator` | Toggle proactive subagent delegation. When disabled, subagent tools remain available for explicit delegation. |
| `/goal <objective>` | Start an autonomous interactive-session mission. Use `/goal` for status, `/goal history` for prior goals, and `/goal pause`, `resume`, or `clear` for control. |
| `/llama` | Connect to the configured llama.cpp router, load, unload, or remove cached models, and search/download GGUF models from Hugging Face with live progress. Shown after llama.cpp login is configured. |
| `/sessions` | Resume a previous session. The picker opens in this-directory scope; press `Tab` for all sessions in the active namespace, or `/` to search user/assistant text. It omits branches created only for tree navigation. |
| `/fork` | Pick a previous user message and fork the current session after that turn. The selected turn becomes branch context and no provider turn starts during the checkout. |
| `/session` | Four ops on the current session: `export` to a portable `.zutsession` file, `import` one back in, `fork` from a past user message into a new branch, `tree` to switch between branches. Opens a picker without an argument; direct forms: `/session export [path]`, `/session import <path>`, `/session fork`, `/session tree`. Default export destination is `~/Downloads`. `Esc` `Esc` from an idle, empty editor is a shortcut for `/session tree` when the terminal supplies two parsed bare Escape events. |
| `/jump` | Scroll the chat to a previous turn (or `/jump <text>` to filter). |
| `/btw` | Side chat with full context that doesn't add to the main thread. |
| `/subagents` | Spawn, inspect, and chat with resident background subagents. Each has a durable journal, structured results, and optional Git worktree isolation. Named profiles can be selected with `agent`; see [docs/subagents.md](docs/subagents.md). |
| `/skills` | List discovered skills (SKILL.md files) and preview their bodies. |
| `/compact` | Summarize the transcript into one message to free up context. |
| `/study` | Run the canned prompt "Read and understand everything in the current directory." so the agent has full project context before you start asking targeted questions. Pass a path — typed, drag-dropped, or selected via `@` — to target a specific file or directory instead: `/study [dir:packages/]`, `/study cmd/zut/main.go`. |
| `/jail` | Confine tools to the current directory. |
| `/unjail` | Allow tools to touch paths outside again. |
| `/reload-ext` | Hot-reload all extensions (re-read manifests, respawn subprocesses, rebuild tool registry). |
| `/telegram` | Connect, disconnect, or show status of the Telegram bridge (takes `connect` / `disconnect` / `status` as an optional argument; opens a picker without one). When connected, DMs from the paired user become prompts in the running session and the assistant's replies are mirrored back to Telegram. Alias: `/tg`. |
| `/settings` | Change persistent settings, including web search, inline images, terminal alerts, AI terminal titles, proactive delegation, Ponytail coding mode, fast mode, main/sub-agent LSP access, and the auto-compact threshold. `/fast` and `/orchestrator` are shortcuts for their corresponding toggles. Saved to `$ZUT_HOME/config.json`; setting changes apply without a restart, while AI title generation waits for the first real prompt. |
| `/clear` | Clear the chat transcript. |
| `/exit` | Exit zut. |

Extension-registered commands appear under a divider at the bottom of the popup, sorted by name.

### `/goal`

In interactive mode, the first user request establishes a durable user-owned mission. `/goal <objective>` explicitly starts or replaces its current user-owned goal and begins work immediately. While a goal is active, zut starts one leased hidden follow-up whenever the interactive thread becomes idle; queued user input always runs first. When no mission is active, the main agent can use `update_goal` to start one of its own; within an active mission, it can settle the current goal or set a bounded next goal. Subagents cannot start or update missions. The current state appears in the status bar and prior transitions are available through `/goal history`; both persist with the session.

Goals are unlimited by default. Zut records token use for each goal but only enforces a budget when it is explicitly configured in `config.json`:

```json
{
  "goals": {
    "max_token_budget": 1000000
  }
}
```

When set, `max_token_budget` is applied to newly created goals. A goal that reaches it becomes `budget_limited`; omitting the setting creates a goal with no token limit. This is separate from the continuation safety controller: a turn that ends without a concrete tool action receives one corrective continuation, then becomes `stalled` rather than repeatedly sending the same prompt. `esc` pauses an interrupted goal, and terminal provider errors block it instead of retrying indefinitely. Use `/goal` to inspect it, `/goal pause` or `/goal resume` to control execution, and `/goal clear` to remove it.

A branch that copies the complete transcript inherits the goal. Forking from an earlier turn clears it because the later objective did not necessarily exist at that point. Manual `/compact` leaves an active goal recorded but does not start a hidden follow-up; run `/goal resume` to continue it.

### Shell escape (`!command`)

Type `!` followed by a command to run it directly without going through the model. Everything after the `!` is passed to the same shell the `bash` tool uses (`/bin/bash -c` when available on Unix, then `bash -c` from `PATH`, with POSIX `/bin/sh -c` as a fallback; `cmd /C` on Windows), runs in the session working directory, and honors the `/jail` sandbox. Interactive shell escapes always use zut's built-in 600-second (10-minute) timeout. The command, merged output, and exit status are appended to the transcript as user context, so the model can use them on the next turn. Running the command does not itself start a model turn. A running `!command` shares the busy state with the agent: `esc` cancels it, and you cannot start one while a turn (or another shell escape) is in flight.

### `/sessions`

Shows previous sessions for the current working directory, newest first, with timestamp, model, message count, cost, and the session title. Press `Tab` to rotate between **this directory** and **all sessions** in the active normal or Zutfile-agent namespace; other agent namespaces never appear. Press `/` and type at least two characters to search contiguous text in an ephemeral in-memory corpus of persisted user and assistant messages. Tool calls, tool results, images, developer context, and compaction payloads are excluded. The picker displays each matching session's count and a highlighted first matching excerpt; changing a query reuses the corpus rather than reopening transcripts. Session entries and search corpus load in the background. Once an entry appears, `up`/`down` and `enter` can select it immediately; `esc` clears a search or cancels and closes the picker. Selecting a session from another working directory switches the live workspace to that session's recorded directory before the transcript resumes, so tools, instructions, sandbox state, and file suggestions do not retain the old directory. Sessions remember their stored provider and model: resuming rebuilds the live agent when necessary instead of silently using a different model; if a staged switch fails, the current session is left unchanged. Branches created for tree navigation remain available to `/session tree` but are hidden from this flat picker. `/sessions` is unavailable with `--no-session`. A fresh interactive session asks the active model for a concise title after the first real prompt and restores that title when resumed.

### `/session`

Four ops on the current session. `/session` alone opens a picker; each is also runnable directly.

- **`/session export [path]`**. Writes the running transcript to a portable `.zutsession` file. Default destination is `~/Downloads/<timestamp>-<session-id>-<prompt-slug>.zutsession`. Pass a path to override; a directory is fine (a dated name is built inside), a bare name gets `.zutsession` appended. The meta's cwd is stripped on the way out so the recipient doesn't see your filesystem layout.

  **What's included.** Only the main chat thread of the running session — messages, tool calls, tool results, compactions, and usage. **`/subagents` child journals are not included.** They remain machine-local managed state; a `.zutsession` is only a main-chat transcript. If you want the child conversation, copy it from the child-session view manually.
- **`/session import <path>`**. Copies a `.zutsession` file into `$ZUT_HOME/sessions/<cwd-hash>/` with a fresh id and the current cwd, then switches the running agent onto it. Imported sessions are first-class: they show up in `/sessions` and `/jump`, and in the current-family tree when they are part of that family. Drag-drop paths in the editor are accepted (zut strips the surrounding quotes automatically).
- **`/fork`** (also available as **`/session fork`**). Opens a turn picker (same shape as `/jump`). Pick any past user message; zut copies the transcript through that turn into a new branch and switches to it without starting a provider request. The parent session stays on disk. Use it to try a different question without polluting the original transcript, or to rewind after the agent went down the wrong path. Use a user-message row in `/session tree` when you want the prompt restored for editing, including image attachments.
- **`/session tree`**. Shows the current session's family: its root ancestor and descendants, not unrelated root sessions in the same cwd. The tree opens immediately while its history loads in the background; rows appear from recent to older content, and `esc` cancels the load or closes the tree. Use `up`/`down` and `enter` to choose an available row. The picker keeps pre-compaction history available as forkable user and assistant messages, while hiding tool calls, tool results, compaction checkpoints, shell/image helper rows, and other internal context. Message rows are indented at their branch points and the current endpoint is tagged `[current]`. Empty and detached branch points are represented by visible boundary rows. A child whose historical fork point no longer matches after compaction stays discoverable as a detached branch under the parent's current tail rather than disappearing. Parentless sessions are roots; orphaned children (whose parent file was deleted) remain roots.

  Selecting a user-message row creates a navigation branch before that message and restores the complete editable draft, including text blocks and image attachments; no provider request is made until you submit it. Selecting an assistant-message row checks out a safe message boundary, and selecting an empty or detached boundary checks out that branch point with an empty editor. Navigation branches are hidden from `/sessions` but remain in the tree.

  `/session tree` and the double-Escape shortcut use the same checks: a complete current session family must be readable, and no turn, shell escape, compaction, startup/session load, dialog, picker, suggestion, or queued message may be active. The shortcut is two parsed, unmodified bare `Esc` events within 500 ms from an idle editor with no visible text or pending image. Dialogs, confirmations, panels, pickers, and suggestions keep their existing Escape behavior; busy Escape still cancels, modified Escape is not part of the gesture, and any intervening key or timeout resets it. The shortcut depends on the terminal input path producing those parsed events; `/session tree` is the explicit equivalent when it is unavailable.

### `/jump`

Opens a turn picker for the current session, one row per user prompt, each showing the turn number, how many tools that turn invoked, and the first line of the prompt. `up`/`down` to pick, `enter` to jump, `esc` to cancel. Any printable rune while the picker is open extends a filter; backspace narrows it back. `/jump <text>` pre-applies the filter; if exactly one turn matches, zut jumps straight there without showing the picker.

Jumping is non-destructive. The transcript is untouched, the viewport just scrolls so the chosen turn is at the top. A muted line at the top of the chat reads `viewing turn N of M, pgdn to catch up`. Scroll back to the bottom with `pgdn` (or keep scrolling with the arrow keys) and the indicator goes away.

### `/btw`

Opens a side-chat overlay with the full main session as frozen context, so you can ask quick clarifying questions ("does asyncio.gather() catch exceptions?", "btw the bundle budget is 10MB", "what's the default fetch timeout?") without bloating the main thread.

Each question runs an isolated agent turn against `system + main transcript + side-chat history so far`. The side chat has the same tools and guards as the main chat, including per-tool confirmation when `--no-yolo` is active. Responses, tool calls, and tool results stay in the overlay. When you press `esc` to close, **nothing** has been added to the main session and subsequent main-thread turns don't re-read any of the side-chat exchanges, keeping the running context window lean.

```
/btw                              # open the overlay, type questions interactively
/btw does PUT replace the whole resource?
```

Inside the overlay: `enter` sends, `esc` cancels an in-flight call (or closes the overlay if idle), `ctrl+c` closes immediately. Side-chat exchanges never touch the transcript and aren't persisted to the session file.

### `/subagents`

Resident subagents run alongside the main session as independent in-process agent conversations. Each has its own stable session identity, provider client, tool registry, cancellation boundary, durable journal, and structured latest-turn result. Starting one never starts another `zut` process.

> **V0.x breaking change:** previous subprocess subagent state is unsupported and ignored. Outstanding jobs are never migrated or replayed; remove the old state directory when you no longer need it.

> **Choose the workspace deliberately.** Use `isolation:"worktree"` for parallel coding so children cannot edit the host checkout. Use shared mode for read-only review or explicitly coordinated work. Worktree isolation is an accident-prevention guardrail, not a security sandbox.

```text
/subagents                                      # open the dashboard
/subagents new <task>                           # spawn an agent (shared mode)
/subagents new --agent reviewer <task>          # use a named markdown profile
/subagents new --reasoning high <task>          # set child reasoning
/subagents logs <id>                            # open one agent's transcript
/subagents send <id> <text>                     # send a follow-up turn
/subagents result <id>                          # show the bounded final summary
/subagents resume <id> <text>                   # explicitly continue a child session
/subagents kill <id>                            # stop a live child
```

**Dashboard (`/subagents` with no arg)** — a bounded list of resident children, ordered by the latest state change. Rows lead with the profile or model, show `completed` rather than the internal success state, a relative update time, and a short ID. Use `up`/`down` then `enter` to open the selected child, or `esc` to close. With any resident child present, `down` from the main composer opens this picker without clearing a draft.

**Inside an agent's transcript** — the overlay uses the main transcript renderer, combining finalized journal history with an immutable live projection. PgUp loads an older bounded page; Up/Down preserves scrollback and PgDn follows new output. Type a follow-up and press `enter`; the draft stays until the manager durably accepts it. Press `esc` to close.

**Lifecycle** — acceptance and finalized transcript records live under managed subagent state as `transcript.jsonl`, with rebuildable `metadata.json`, bounded `result.json` (including up to 256 KiB of final visible assistant text), and optional worktree `patch.diff`. Completion updates carry that bounded final text; open the child transcript for complete history. Host shutdown stops all children. A next launch marks work that was queued or running as `interrupted`; it never replays it. Use an explicit new prompt to resume.

The fair global scheduler defaults to six concurrent turns, one active turn per child. `subagents.max_concurrent` overrides that limit. Queueing has no default deadline; a positive `subagents.queue_timeout` durably fails a prompt that waits too long for a slot. Required child work remains an obligation until an explicit successful follow-up.

Worktree isolation captures a patch and changed-file list without applying it. Shared children use the host checkout. The available logical references are `subagent://<id>`, `subagent://<id>/history`, `subagent://<id>/result`, and `subagent://<id>/patch`.

**Proactive delegation.** With `/settings` → **proactive delegation** on (or `/orchestrator`), the interactive primary remains the owner and implementer of the task. It delegates only concrete sidecars that can progress while the primary performs a preselected non-overlapping task; immediate blockers and tightly coupled work stay local. An active worker owns its scope until completion, so the primary does not repeat that investigation, review, testing, or implementation. If an explicit user request or workflow hands blocking work to a child, the primary yields for the host completion update instead. The canonical resident tools remain available for user-requested delegation when the setting is off, subject to launch-time permissions. Do not poll `subagent_status` or journal files solely to wait for completion.

### Named subagent profiles

zut discovers the common markdown/frontmatter profile layout from `~/.agents/agents/*.md` by default. Whenever `subagent_spawn` is available, the primary agent receives `[subagents_list]` metadata and should select the appropriately named profile for each worker when one matches; otherwise it can spawn a clearly described general worker without an `agent` field. If launch-time policy withholds `subagent_spawn`, profile metadata is omitted. An interactive primary with proactive delegation enabled continues locally, while an explicit headless `--orchestrate` run reports that its required delegation is unavailable instead of implementing directly. Profile bodies are applied only to the selected child. See [docs/subagents.md](docs/subagents.md) for discovery, supported fields, reasoning levels, and examples.

### `/settings`

Opens a dialog with every persistent setting. `up`/`down` to navigate, `enter` or `space` to change the selected row, `esc` to close (rows that open a sub-view, like model shortcuts, use `esc` to go back one level first). Changes are written to `$ZUT_HOME/config.json` without a restart; individual settings document whether they affect the current turn, the next model call, or a later session event. Current settings:

- **web search** — allow the normal CLI agent to search DuckDuckGo and explore only returned in-memory references. Enabled by default; disabling it removes `web_search`, `web_open`, `web_find`, and `web_click` from the current interactive session and future normal CLI runs when no invocation-level tool override applies. If the current command explicitly included `--tools web_search` (alone or in its CSV list), that higher-precedence opt-in keeps the capability enabled for this session and the settings row is read-only with an explanation; run zut without that explicit override to change the persisted default. The setting persists as `web_search_enabled`. Bot, default SDK, and packaged-agent runs remain unavailable regardless of this setting. See [docs/web-search.md](docs/web-search.md).
- **render images when supported** — draw screenshots / `read`-returned images inline using the terminal's image protocol, or fall back to a text placeholder. Auto-detected from `TERM_PROGRAM`; the toggle overrides the detection. The row is greyed out and forced off on terminals that don't speak any image protocol.
- **terminal alerts** — emit a terminal bell when the main session stops with work that may need attention, or when an extension raises a structured alert. Enabled by default; changes apply immediately and persist as `terminal_alerts_enabled`. Terminal emulators may render the bell audibly, visually, or not at all.
- **AI terminal titles** — after the first real prompt of a fresh interactive session, make one small hidden request to the active model and set the terminal title to `zut: <title>`. The title is limited to 40 Unicode characters, persisted with the session, restored on resume, and never added to the conversation. Enabled by default; disable it to avoid the extra model request. The toggle applies immediately, but title generation still waits for the first real prompt and never starts from startup context, resumed history, or slash commands. Persists as `terminal_title_enabled`.
- **proactive delegation** — let the interactive primary delegate concrete, bounded sidecars while it keeps ownership of the critical path. Before spawning, it identifies useful local work and gives the child a non-overlapping question, responsibility, package, or file set. Immediate blockers and tightly coupled work stay local; after spawning, the worker owns its scope until completion and the primary continues only its preselected independent task. If a user request or active workflow explicitly delegates blocking work, the primary yields rather than reproducing it. Off by default. When off, the permitted canonical subagent tools and profile manifest remain available for user-requested delegation or when an active skill workflow requires it. The tools accept named profiles, reasoning, per-spawn fast-mode overrides, and shared/worktree isolation where applicable; lifecycle and result references are persisted. Completion and required-work state are host-driven through `[auto-subagents update]` and `[required-subagents update]` messages; required work stays asynchronous, persists its outcome, and prevents the terminal parent response until a successful completion. If launch-time restrictions prevent spawning, the interactive primary continues locally. Explicit headless `--orchestrate` remains a separate strict manager-only mode and reports unavailable delegation rather than implementing directly. Do not poll or inspect dashboards, metadata, event logs, or files solely to wait. See `/subagents` and [docs/subagents.md](docs/subagents.md) for details.
- **Ponytail coding mode** — include compact engineering guidance in each resolved system prompt; the guidance tells the model to apply it to coding, debugging, and review work rather than ordinary conversation. It favors understanding the real flow, small validated changes, reuse, and preserving safety checks. Enabled by default; changes apply to the next model call and persist as `ponytail_enabled` in `$ZUT_HOME/config.json`. It is included by interactive, print, stream, JSON, RPC, subagent, bot, SDK, and Zutfile agents.
- **fast mode** — request the provider's fast tier where supported. It currently uses OpenAI's Fast service tier for OpenAI, OpenAI Responses, and OpenAI Codex models. Off by default; toggle it with `/fast` or `/settings`; changes apply on the next model call and persist as `fast_mode`. Other providers currently return an unsupported-provider error. Fast mode may cost more and depends on the selected model/account.
- **lsp in main session** — enable the built-in `lsp` tool and code diagnostics for the main agent. Enabled by default; changes persist as `lsp_enabled`.
- **lsp in sub-agents** — allow newly spawned background sub-agents to use the built-in `lsp` tool. Enabled by default; changes persist as `subagent_lsp_enabled` and apply when a child starts.
- **auto-compact threshold** — choose `off`, `70%`, `80%`, `85%` (default), `90%`, or `95%` of the model's advertised context window. The selected percentage controls automatic compaction before and after interactive turns and before sub-agent follow-up turns, and persists as `auto_compact_threshold`. Context accounting includes uncached input plus provider-reported cache reads and writes, so cached turns still trigger compaction. Structural unfinished tails and truncated output continue as before. After a successful after-turn threshold compaction, zut may make at most two hidden, bounded recovery continuations when the assistant's visible text appears to promise future work; clear completed text settles normally, and a newer queued user request wins. An interrupted interactive session retains an outstanding handoff when resumed, including after portable export and import. Manual `/compact` does not silently start work. `off` disables percentage-based triggers but keeps manual `/compact` and automatic recovery from context-window and payload-too-large responses. Truncated-output continuation remains a separate priority rule.
- **jail new sessions by default**: start every new agent with tools confined to its working directory. Off by default. The setting applies to interactive, print, JSON, RPC, and background-agent runs, persists as `jail_by_default`, and immediately updates the current interactive session. `/jail` and `/unjail` remain session-scoped overrides and do not change this default.
- **compact transcript rendering**: reduce visual chrome in the chat transcript. Tool calls render as a quiet header plus indented output instead of a bordered box, and sent messages render without padded background bubbles. Off by default. Changes apply immediately and persist to `config.json` as `compact_mode`.
- **show loaded resources at startup**: list the active Zutfile agent (for `zut run`), loaded `AGENTS.md` paths, extensions, and user-installed skills in compact sections above the transcript. Built-in skills are omitted. Off by default. Changes apply immediately and persist to `config.json` as `show_instructions_at_startup`.
- **TUI settings**: opens a sub-view for input layout in display order: status, working spinner, input, then subagent operations. **Input style** can be `plain` (default prompt line), `lines` (separator lines above and below the input), or `block` (a user-bubble-style input block). **Status position** places model, usage, and working-directory information above or below the input. **Working spinner position** places the busy spinner above or below the input. Changes apply immediately and persist to `config.json` as `tui_input_style`, `tui_status_position`, and `tui_working_position`.
- **reasoning level**: choose reasoning for supported models: off, minimum, low, medium, high, xhigh, or max. The `max` tier is opt-in and sent natively to GPT-5.6 and adaptive-thinking Claude models; unsupported backends clamp it to their highest accepted effort. The change is persisted to `config.json` and applied to the next model call. Use `/reasoning` to open this selector directly. The selector only shows distinct levels supported by the active model; models without reasoning support only offer `off`.
- **color theme** — `auto` follows the terminal foreground, background, ANSI palette, color depth, and supported live appearance changes. Choose fixed dark/light palettes or JSON overlays discovered under `$ZUT_HOME/themes` and loaded extensions. Active custom files reload safely; invalid edits retain the last valid appearance and persistent deletion resets to auto. See [docs/themes.md](docs/themes.md).
- **model shortcuts** — opens a sub-view with nine slots (`model 1` ... `model 9`). `enter` on a slot opens the same `/model` selector and binds the chosen provider/model to that slot; `backspace` clears a slot. Once assigned, press `Ctrl+1` ... `Ctrl+9` from the editor to switch the active model instantly (the same cross-provider swap `/model` performs, transcript and cost carried over). Assigning a shortcut does not change the current model. Shortcuts are skipped while a turn is running.

### `/skills`

Opens a picker listing every discovered SKILL.md file, built-ins hidden. Each row shows the skill name, source, and description. `enter` opens the body inline (scrollable with `up`/`down`/`pgup`/`pgdn`); `esc` goes back. Re-runs discovery each time it opens, so edits to a SKILL.md during a session are reflected immediately.

### `/compact`

Sends the current transcript through the model with a structured summarization prompt. The returned summary replaces the transcript as one synthetic user message, with the last few exchanges kept verbatim for continuity. The status bar's context meter resets. Manual `/compact` only summarizes; it does not silently start work afterward. Use it when the context meter creeps past ~80%.

zut also auto-compacts in the background after an interactive turn reaches the configured context threshold. Structural unfinished tails and output stopped at the model's token limit continue as before. After a successful after-turn threshold compaction, a visible assistant status that appears to promise future work may receive at most two hidden, bounded recovery continuations; clear completed text settles normally, and a newer queued user request supersedes that recovery. An interrupted interactive session resumes an outstanding hidden handoff without displaying it as a chat message. A new branch keeps that handoff only when it copies the complete effective transcript; `/fork` or `/session tree` selection at an earlier turn, and every branch from pre-compaction history, drops it and starts no hidden continuation. Manual `/compact` does not silently start work, and truncated-output continuation remains a separate priority rule. Choose `off`, `70%`, `80%`, `85%` (default), `90%`, or `95%` under `/settings` → **auto-compact threshold**. You'll see `condensing history, esc to cancel` above the status bar and an `(auto)` tag next to the context percentage; `esc` aborts it without touching the transcript. Turning the percentage trigger off does not disable automatic compaction and retry after a context-window or payload-too-large response.

To prevent tool-heavy runs from exhausting context, provider requests and compaction inputs retain up to 32 KiB from each tool result and 128 KiB across all tool-result text, preferring newer results. Omitted output is marked for the model but remains fully visible in the transcript and stored in the session.

### `/jail`

Enforces a sandbox rooted at the cwd shown in the status bar. `read`, `write`, `edit`, and `grep` resolve their target path (including through symlinks) and refuse anything outside the sandbox. `create_worktree` also requires its repository root, Git metadata, optional `.gitignore`, and configured worktree destination to be inside the sandbox. `bash` refuses obvious escape patterns (`sudo`, `rm -rf /`, leading `cd /`, `cd ..`, `cd ~`, `chmod -R`, `dd of=/`, and similar) and rejects shell arguments or redirections that point outside the sandbox. The status bar shows `jailed, ~/your/cwd` while active. Enable **jail new sessions by default** in `/settings` to persist this behavior across launches; `/unjail` then unlocks only the current session.

This is a guardrail against accidents, not a hard security boundary. If you need real isolation, run zut under docker or a proper sandbox.

## Sessions

Every interactive or print/json run (unless `--no-session`) writes a JSONL transcript under `$ZUT_HOME/sessions/<cwd-hash>/`. Resume any of them with `--continue`, `--resume`, `--session <path>`, or interactively via `/sessions` inside the TUI. Session metadata stores the provider and model used by that session; resume honors that pair, rebuilding the active agent when it differs and refusing the switch without discarding the current session if rebuilding fails. Usage checkpoints may include sanitized `retry_lifecycle` metadata—attempts, backoff delays, terminal state, and an allowlisted failure category—but never raw provider errors or response bodies. This metadata is ignored when rebuilding provider context. Empty sessions (the user exited without prompting) are deleted on close so the list stays tidy.

## Providers

zut's built-in provider catalog includes:

- **Subscription-capable**: Anthropic Claude Pro/Max (`anthropic`), OpenAI Codex / ChatGPT Plus/Pro (`openai-codex`), Kimi Code (`kimi`), SuperGrok/X Premium (`xai`), GitHub Copilot (`github-copilot`).
- **Direct API providers**: Anthropic, OpenAI Chat Completions, OpenAI Responses, DeepSeek, Google Gemini, Kimi/Moonshot, Moonshot CN, Groq, Cerebras, xAI, Together AI, Hugging Face Router, OpenRouter, Mistral, Z.AI, Xiaomi/MiMo token-plan regions, MiniMax global/CN, Fireworks, Vercel AI Gateway, OpenCode/OpenCode Go.
- **Cloud/platform providers**: Amazon Bedrock, Google Vertex AI, Azure OpenAI, Cloudflare Workers AI, Cloudflare AI Gateway.
- **Local/compatible**: Ollama, llama.cpp router mode, and OpenAI-compatible local endpoints via `--base-url`.

Use `/login` to store API keys or subscription credentials. `/model` only shows models from providers that are currently available from env vars, `auth.json`, Kimi CLI fallback, local Ollama, or a configured llama.cpp router.

## Models

`--list-models` or the `/model` picker shows the full catalog across all built-in providers. Three sources:

- **Catalog**: models baked into zut, covering Claude, GPT/Codex, Gemini/Gemma, Kimi/Moonshot, DeepSeek, Groq-hosted Llama/Gemma/Compound, OpenRouter-routed models, Bedrock model ids, Vertex model ids, Azure OpenAI deployments, Copilot models, and other provider-specific catalog entries.
- **Live**: IDs discovered from `GET /v1/models` using your stored API key (cached for 6h in `$ZUT_HOME/models-cache.json`, refreshed in the background on startup).
- **Speculative**: IDs that appear in the upstream generator but aren't live on the public API yet. They'll 404 today and start working the moment the provider ships them.

The context meter in the status line uses the model's advertised context window to show how much of it your last turn consumed. Tool output is still shown in full through the normal transcript rendering and retained in the session transcript. To keep long-running sessions usable, zut bounds the historical tool-result text included in provider-facing context; this projection affects what is sent to the model, not what is displayed or persisted, and it preserves the tool-call/result structure.

### Model fallback (rescue)

When a turn fails because of a recoverable provider error — expired token (`401`), permission denied (`403`), rate limit (`429`), provider outage (`502`/`503`/`504`), or a transient network failure — zut opens a **rescue** picker over the chat instead of just painting a red banner.

The picker is the same vertical list / fuzzy filter UI as `/model`, but it only shows models from providers you're currently logged in to (env vars, `auth.json`, Kimi CLI fallback, ollama). The failed model is excluded. Press `↑`/`↓` to choose, `enter` to retry the **same prompt** on the new model, `esc` to dismiss.

Before the actual provider request fires, the shared HTTP streaming clients used by OpenAI / Anthropic / Kimi / DeepSeek / Google / OpenAI-Codex also do up to four silent retries with short backoff (250ms, then 750ms, 750ms, and 750ms) on transient HTTP failures and connection-reset / EOF-before-headers errors. Once a stream is open, five reconnect attempts and a five-minute idle timeout independently guard stalled or interrupted streams. Most edge-proxy blips disappear without you ever seeing the rescue picker.

A rescue retry always **drops launch-time `--api-key` and `--base-url`** before rebuilding the agent. Those overrides are usually the reason the rescue triggered (bad key, typo'd base URL, corporate gateway only valid for the originally-picked provider), so the retry re-resolves credentials from env vars / `auth.json` / provider defaults instead. Use `/model` if you want overrides to stick.

No configuration is required — the candidate list is built dynamically from your active credentials. Bad-request / context-length / serialization errors are NOT routed to the rescue picker, because switching models won't fix them; those still surface as a normal error.

### Custom models

Place a `models.json` in `$ZUT_HOME` (`$XDG_STATE_HOME/zut/` when set, otherwise the platform default above) to add models that aren't in the baked-in catalog or to override existing entries:

```json
{
  "providers": {
    "openai": {
      "models": [
        {
          "id": "gpt-5.5",
          "name": "GPT-5.5",
          "reasoning": true,
          "reasoningLevelMap": {"minimum": "low", "max": ""},
          "contextWindow": 400000,
          "maxTokens": 128000
        }
      ]
    }
  }
}
```

Supported fields per model: `id` (required), `name`, `reasoning`, `reasoningLevelMap`, `contextWindow`, `maxTokens`, `baseUrl`, `priceInput`, `priceOutput`, `priceCacheRead`, `priceCacheWrite`.

`reasoningLevelMap` is optional. Protocol defaults apply when it is omitted. Add only model-specific exceptions using `minimum`, `low`, `medium`, `high`, `xhigh`, or `max` as keys. Map a key to another level when both inputs are equivalent, use an identity mapping such as `"max": "max"` to enable a level beyond the protocol default, or map it to an empty string or `off` to remove it. The same effective mapping drives `/reasoning` and provider requests.

Provider keys are normalized: `openai-codex` and `openai-responses` map to `openai`, `anthropic-messages` maps to `anthropic`, `moonshot`, `moonshot-ai`, and `kimi-code` map to `kimi`, and `deepseek-chat` and `deepseek-ai` map to `deepseek`. Built-in provider ids such as `groq`, `openrouter`, `github-copilot`, `amazon-bedrock`, `google-vertex`, `azure-openai-responses`, `fireworks`, `vercel-ai-gateway`, `mistral`, and `xai` can also be used directly.

User-defined models show `source: user` in `--list-models` and take precedence over both the baked-in catalog and live-discovered models. Adding a `models.json` does not hide the built-in catalog; entries are merged on top of it. Missing or invalid files are silently ignored.

#### Custom providers

A top-level provider key that is not a built-in id defines a custom provider. Give it a provider-level `baseUrl` and an `api` wire format (`openai` for OpenAI-compatible Chat Completions, the default, `openai-responses` for the OpenAI Responses API, or `anthropic` for the Anthropic Messages API). A model-level `baseUrl` overrides the provider-level one for that model; an unknown `api` value falls back to `openai` with a warning.

```json
{
  "providers": {
    "my-company": {
      "baseUrl": "https://llm.mycompany.com/v1",
      "api": "openai",
      "models": [
        { "id": "company-llm-v2", "name": "Company LLM v2" }
      ]
    }
  }
}
```

Custom providers are first-class: they appear in `--list-models`, `/model`, and `/login`. `models.json` never stores secrets. Supply the key through `/login`, `--api-key`, or a derived environment variable in upper snake case (so `my-company` reads `MY_COMPANY_API_KEY`). Because many self-hosted gateways do not expose a model-list endpoint, custom provider keys are accepted and stored without a verification probe; an invalid key surfaces on the first model call.

To retrieve this custom provider's key from a password manager, add a matching entry to `$ZUT_HOME/auth.json`:

```json
{
  "additional_api_key_creds": {
    "my-company": {
      "api_key_command": {
        "program": "op",
        "args": ["read", "op://Work/OpenAI/credential"],
        "timeout_ms": 120000
      }
    }
  }
}
```

The provider IDs in `models.json` and `auth.json` must match. Then select the custom model directly:

```bash
zut --provider my-company --model company-llm-v2
```

### Kimi Code

zut has built-in Kimi support through the Kimi Coding endpoint and Moonshot's OpenAI-compatible chat API.

```bash
zut --provider kimi
```

By default this uses:

- model: `kimi-for-coding`
- base URL: `https://api.kimi.com/coding/v1`

Credential lookup order for Kimi:

1. `--api-key`
2. `KIMI_API_KEY`
3. `MOONSHOT_API_KEY`
4. `$ZUT_HOME/auth.json`
5. the official Kimi Code CLI token at `~/.kimi/credentials/kimi-code.json`, unless disabled by `/logout kimi`

Use `/login` for either API-key login or Kimi Code subscription login. The subscription flow uses Kimi Code's device-code OAuth flow: zut opens the verification URL, waits for browser approval, stores the token in `auth.json`, and refreshes it automatically.

For direct Moonshot API keys or a custom compatible endpoint:

```bash
zut --provider kimi --model kimi-k2-0905-preview --base-url https://api.moonshot.ai/v1 --api-key "$KIMI_API_KEY"
```

Kimi K3 is built in as `kimi/k3`, `moonshotai/kimi-k3`, `moonshotai-cn/kimi-k3`, `opencode-go/kimi-k3`, `openrouter/moonshotai/kimi-k3`, and `vercel-ai-gateway/moonshotai/kimi-k3`. Its output limit is 131,072 tokens on every built-in route.

You can add additional Kimi/Moonshot model IDs to `models.json` under the `kimi` provider.

### xAI

xAI supports either `XAI_API_KEY` or `/login` subscription authentication. The subscription option is labeled `Sign in with SuperGrok or X Premium`, opens a prefilled device-authorization URL, and refreshes the stored token automatically. The default xAI model is `grok-4.5`, sent through the Responses API.

### DeepSeek

zut has built-in DeepSeek support through DeepSeek's OpenAI-compatible chat API.

```bash
zut --provider deepseek
```

By default this uses:

- model: `deepseek-v4-pro`
- base URL: `https://api.deepseek.com/v1`

Catalog ships with `deepseek-v4-pro` (reasoning) and `deepseek-v4-flash`. These are exactly the IDs returned by `GET https://api.deepseek.com/models` today. You can add additional model IDs to `models.json` under the `deepseek` provider.

Credential lookup order for DeepSeek:

1. `--api-key`
2. `DEEPSEEK_API_KEY`
3. `$ZUT_HOME/auth.json`

Use `/login` and pick **api key** to paste a DeepSeek key. zut probes `/v1/models` once and stores the key under `deepseek` in `auth.json`.

> **Auth model: API key only.** DeepSeek does not offer a subscription OAuth flow. The `/login subscription` step lists only Anthropic, OpenAI, and Kimi; DeepSeek shows up only under `/login → api key`.

> **Text only at the wire level.** DeepSeek's chat-completions endpoint currently rejects the multimodal content schema (`unknown variant image_url, expected text`). When the active provider is `deepseek`, zut silently drops `ImageBlock` parts from outgoing user/tool messages and keeps only the text. Switching back to a vision-capable model (Claude, GPT-4o/5, Gemini) re-sends the image normally because the session file still stores it.

For a custom-compatible endpoint (mirror, gateway, self-host):

```bash
zut --provider deepseek --base-url https://my-deepseek-mirror.example.com/v1 --api-key "$DEEPSEEK_API_KEY"
```

### Google Gemini

zut has built-in Google Gemini support through the [AI Studio Generative Language API](https://aistudio.google.com/).

```bash
zut --provider google
```

By default this uses:

- model: `gemini-2.5-pro`
- base URL: `https://generativelanguage.googleapis.com`

Catalog ships with `gemini-2.5-pro`, `gemini-2.5-flash`, `gemini-2.5-flash-lite`, `gemini-2.0-flash`, and `gemini-2.0-flash-lite`. Live discovery against `/v1beta/models` adds anything else your key can see.

Credential lookup order for Google:

1. `--api-key`
2. `GEMINI_API_KEY`
3. `GOOGLE_API_KEY`
4. `$ZUT_HOME/auth.json`

Use `/login` and pick **api key** to paste an AI Studio key. zut probes `/v1beta/models` once and stores the key under `google` in `auth.json`.

> **Auth model: API key only (this provider).** Google does not issue OAuth tokens for consumer Gemini Advanced / Google One AI Premium subscriptions, so there is no "log in with your Google subscription" flow. The `/login subscription` step quietly downgrades to the api-key form when you pick Google so you don't end up in a dead end. If you need OAuth/service-account auth instead of an API key, use the `google-vertex` provider below.

> **Free-tier rate limits.** AI Studio's free tier has tight per-minute and per-day caps that vary by model: `gemini-2.5-pro` is the strictest (a few requests per minute, ~50 per day), Flash and Flash-Lite are far more generous. If a Pro turn 429s with `"You exceeded your current quota"` while Flash on the same key still works, you've hit the Pro free-tier RPD. Either switch to Flash for agent loops, or [enable billing](https://aistudio.google.com/app/apikey) on your AI Studio project to flip the same key from free to pay-as-you-go pricing (`$1.25/M` input, `$10/M` output for Pro).

Reasoning levels (`--reasoning off|minimum|low|medium|high|xhigh|max`, also configurable through `/reasoning` or in `/settings` as **reasoning level**) map differently per generation. `max` is a distinct opt-in tier above `xhigh`. GPT-5.6 and adaptive-thinking Claude models receive native `max`; unsupported providers clamp it to their highest accepted effort. Budget-based providers retain their provider/model caps. Gemini 3.x uses the `thinkingLevel` enum (`MINIMAL`/`LOW`/`MEDIUM`/`HIGH`), with Gemini-3-Pro pinned to `LOW` minimum and `HIGH` for any medium-or-higher request. `off` sends no reasoning config. Gemini 2.0 models have no thinking config.

You can add additional Gemini model IDs to `models.json` under the `google` provider.

### Gemini Enterprise Agent Platform (formerly Google Vertex AI)

zut also has built-in support for [Gemini Enterprise Agent Platform](https://cloud.google.com/products/gemini-enterprise-agent-platform), Google's enterprise/GCP-hosted Gemini endpoint. Unlike the AI Studio `google` provider above, the `google-vertex` provider supports a Google Cloud API key plus `service_account` and `authorized_user` credential files. It does not support consumer Google login or the complete ADC credential chain, such as metadata-server and workload-identity credentials.

```bash
zut --provider google-vertex
```

Required configuration (env vars, read at construction time):

- `GOOGLE_CLOUD_PROJECT` — required, your GCP project id.
- `GOOGLE_CLOUD_LOCATION` — optional, defaults to `us-central1`.

Credential lookup order for Vertex:

1. `GOOGLE_CLOUD_API_KEY`: simplest option. An API key created in the GCP console is sent as `x-goog-api-key`, without a token exchange.
2. `GOOGLE_APPLICATION_CREDENTIALS`: path to a supported credential JSON file.
3. **Default ADC file**: if neither of the above is set, zut checks the file written by `gcloud auth application-default login`. This is `~/.config/gcloud/application_default_credentials.json` on Unix systems and `%APPDATA%\gcloud\application_default_credentials.json` on Windows.

Two credential file shapes are supported:

- `type: "service_account"`: zut signs a JWT with the private key and exchanges it for a short-lived access token.
- `type: "authorized_user"`: zut exchanges the stored client ID, client secret, and refresh token for a short-lived access token.

Access tokens are cached in memory and refreshed on demand.

If none of these are available, zut errors with `vertex: no auth — set GOOGLE_CLOUD_API_KEY or GOOGLE_APPLICATION_CREDENTIALS`.

### Local models with ollama

zut works with [ollama](https://ollama.com) out of the box. Ollama serves an OpenAI-compatible API locally, so any model you have pulled works with zut.

Quick start:

```bash
ollama pull qwen3.5:4b
zut --provider ollama --model qwen3.5:4b
```

That's it. No API key needed for local models. zut defaults to `http://localhost:11434`.

For a remote ollama instance or one behind auth:

```bash
zut --provider ollama --model llama3 --base-url https://my-server.com/v1 --api-key my-token
```

You can also add models to your `models.json` so you don't need flags every time:

```json
{
  "providers": {
    "ollama": {
      "models": [
        {
          "id": "qwen3.5:4b",
          "name": "Qwen 3.5 4B",
          "contextWindow": 32768,
          "maxTokens": 8192
        }
      ]
    }
  }
}
```

The `ollama` provider uses the OpenAI chat completions protocol internally, so it also works with any OpenAI-compatible server (vLLM, LM Studio, LocalAI, etc.).

### Local models with llama.cpp router mode

zut can connect to a recent [llama.cpp](https://github.com/ggml-org/llama.cpp) router, manage its GGUF files, and use loaded models through the router's OpenAI-compatible inference API. This is separate from Ollama. An Ollama server normally listens on port `11434` and should use zut's `ollama` provider instead.

Install or update llama.cpp. On macOS with Homebrew:

```bash
brew install llama.cpp
# or, when already installed
brew upgrade llama.cpp
```

Create a directory for GGUF files and start `llama-server` without `--model`, `-m`, or `-hf`. Supplying one of those options starts a single model rather than the router API zut needs.

```bash
mkdir -p ~/llama-models

llama-server \
  --models-dir ~/llama-models \
  --no-models-autoload \
  --jinja \
  --host 127.0.0.1 \
  --port 8080 \
  -ngl 999 \
  -c 32768
```

`--no-models-autoload` leaves load decisions to `/llama`. `--jinja` enables model chat templates and improves tool-call compatibility. `-ngl 999` requests maximum GPU offload, while `-c 32768` limits each loaded model to a 32K context. Adjust these values for your hardware.

Confirm that router mode is active before configuring zut:

```bash
curl http://127.0.0.1:8080/health
curl http://127.0.0.1:8080/models
```

The models request must return JSON with a `data` array. A 404 usually means the server is outdated, running on a different port, or was started in single-model mode.

In zut, run `/login`, choose **api key**, and select **llama.cpp**. Enter `http://127.0.0.1:8080` as the router URL and leave the API key empty for a local-only server. Do not enter `/v1`; zut derives the inference URL itself. The saved URL and optional key live in `$ZUT_HOME/auth.json`.

You can configure the same connection through environment variables:

```bash
export LLAMA_BASE_URL=http://127.0.0.1:8080
export LLAMA_API_KEY=optional-secret
```

When using a key, launch the server with the matching `--api-key` value. Keep `--host 127.0.0.1` unless remote clients must reach the router.

Run `/llama` to:

- inspect the router's current model states
- search Hugging Face for GGUF repositories
- choose a quantization and download it with byte progress
- explicitly load or unload a model
- press `d` to ask the router to remove a downloaded cache model after confirmation

Models discovered through `--models-dir` or a preset cannot be removed by zut. Delete those files from their configured source instead. The router removes the selected GGUF from its Hugging Face cache, but some llama.cpp versions retain shared repository artifacts such as `mmproj` files. Remove the repository's cache directory manually if those artifacts are no longer needed. Hugging Face search uses `HF_TOKEN` when available. Gated repositories require prior access approval, and the `llama-server` process also needs an authorized `HF_TOKEN` because the server performs the download.

Opening `/model` refreshes the router and lists every loaded model under provider `llama.cpp`. Unloaded models are intentionally omitted because they cannot answer inference requests. Load them through `/llama` first, then select them through `/model`.

A model installed through Ollama is kept in Ollama's internal storage and is not automatically available as a llama.cpp GGUF file. Download a GGUF copy through `/llama` or place GGUF files in `~/llama-models`, then restart the router so it discovers files added manually.

## Clipboard images

In the main chat, `ctrl+v` checks the system clipboard for an image before falling back to text. A pasted image appears as a `[clipboard image #N]` marker and is attached when you submit the prompt; `esc` or `ctrl+c` clears pending image markers with the rest of the input. Wayland image paste uses `wl-paste` from `wl-clipboard`; X11 image paste uses `xclip` when available. Clipboard images larger than 32 MiB are ignored and use the normal text-paste fallback.

## Inline images

When a tool returns an image (for example `read` on a PNG), zut renders it inline on terminals that support it: **Ghostty**, **Kitty**, **iTerm2**, **WezTerm**. On other terminals you see a text placeholder with MIME type, pixel dimensions, and byte size. Control with the `ZUT_INLINE_IMAGES` env var:

| Value | Effect |
|---|---|
| unset (default) | Auto-detect based on `TERM_PROGRAM`; use the text placeholder inside VS Code and Herdr. |
| `iterm`, `iterm2` | Force the iTerm2 OSC 1337 protocol. |
| `kitty` | Force the Kitty graphics protocol. |
| `off`, `none` | Always use the text placeholder. |

Herdr's Kitty graphics support is currently experimental, so zut does not enable inline images there automatically. After enabling `experimental.kitty_graphics` in Herdr, set `ZUT_INLINE_IMAGES=kitty` to opt in.

Frames containing images are full-repainted (no differential diff) to prevent stale image pixels from lingering through scroll. That costs one terminal flash per image-containing frame; set `ZUT_INLINE_IMAGES=off` if that bothers you.

## Tool rendering

By default each tool call (bash, read, write, edit, create_worktree, grep, lsp) renders inside a bordered panel — a `┌─ header ─┐`, `│`-prefixed body rows, and a `└─┘` footer. On a screen with many calls the borders can read as busy, so zut also offers a **flat** mode: a single quiet header line per call (`▌ bash …`) with indented, border-free output. Same information — tool name, arg summary, streamed output, the `... (N more lines, ctrl+o to expand)` truncation — just no frame.

Set the `tool_render` key in `$ZUT_HOME/config.json`:

```json
{
  "tool_render": "flat"
}
```

| Value | Effect |
|---|---|
| unset / `"box"` (default) | Each tool call is wrapped in a bordered panel. |
| `"flat"` | Boxless: a quiet header line plus indented output. |

The `ZUT_FLAT_TOOLS` env var overrides the config for a single run, which is handy for trying it without editing the file:

| Value | Effect |
|---|---|
| `1`, `true`, `yes`, `on`, `flat` | Force flat rendering. |
| `0`, `false`, `no`, `off`, `box` | Force the bordered panel. |
| unset | Fall back to the `tool_render` config key. |

```sh
ZUT_FLAT_TOOLS=1 zut   # flat, just this run
ZUT_FLAT_TOOLS=0 zut   # boxes, even if config.json says "flat"
```

Either way, theme colors still drive the rendering (the header uses your accent/foreground, output uses the tool-output color) and `ctrl+o` still expands a truncated result.

### Tool arg width

The header line for a tool call shows the tool name plus a one-line summary of its primary argument — a `path`, a `command`, or a query. That summary is truncated to 60 cells by default (`web_answer What is the best architecture to implement resilience wit...`). On a wide terminal that can clip long queries more than you'd like, so set the `ZUT_TOOL_ARG_WIDTH` env var to raise (or lower) the limit:

```sh
ZUT_TOOL_ARG_WIDTH=120 zut   # allow up to 120 cells before truncating
```

| Value | Effect |
|---|---|
| unset (default) | Truncate the arg summary at 60 cells. |
| integer in `[20, 500]` | Truncate at that many cells instead. |
| anything else | Ignored; falls back to the 60-cell default. |

## Compact input

By default a message you send renders as a padded, background-tinted bubble: a blank tinted row above and below the text, with a `▌` accent bar down the left. So even a one-line prompt occupies three rows. Set `compact_input` to collapse it to a single quiet `▌ your text` gutter line per wrapped row — no padding rows, no background tint.

```json
{
  "compact_input": true
}
```

| Value | Effect |
|---|---|
| unset / `false` (default) | Padded, background-tinted user bubble. |
| `true` | One quiet gutter line per wrapped row. |

The `ZUT_COMPACT_INPUT` env var overrides the config for a single run (`1`/`true`/`on`/`compact` force compact; `0`/`false`/`off`/`bubble` force the bubble):

```sh
ZUT_COMPACT_INPUT=1 zut   # compact, just this run
```

## Queued messages

You can keep typing while the agent is working. Pressing `enter` during a turn queues the message, including pasted screenshots, instead of interrupting. It appears above the status bar as `sliding in: <text> [image]` and is delivered at the next safe model-call boundary. Queue as many messages as you want; they run in order. `alt+up` returns the most recently queued message and its image markers to the input for editing. `esc` cancels the active turn, restores the most recent queued message in the same way, and drops any remaining stale follow-ups; `ctrl+c` while busy arms the exit hint instead of interrupting, and a second `ctrl+c` within two seconds exits zut.

To recover the most recently queued message back into the editor (to tweak it before it runs), press `Option+↑`. In VS Code's integrated terminal that chord doesn't survive xterm.js's macOS key handling — use `Option+Shift+↑` there. zut's hint line under the sliding-in queue adapts automatically based on `$TERM_PROGRAM`.

Slash commands also work while the agent is busy. Non-destructive ones (`/help`, `/info`, `/jump`, `/btw`, `/sessions`, `/skills`, `/reasoning`, `/settings`, `/jail`, `/unjail`, `/exit`) take effect immediately. Destructive ones (`/clear`, `/compact`, `/login`, `/logout`, `/model`, `/reload-ext`) cancel the active turn first and then run.


## Keys (interactive mode)

### Input

| Key | Action |
|---|---|
| `enter` | Submit (queued if the agent is busy). |
| `alt+enter` | Newline. |
| `tab` | Complete the selected slash command. |
| `esc` | Cancel the current turn (while busy); clear input (while idle). Two parsed bare `Esc` presses within 500 ms open `/session tree` only when the editor is idle and empty; dialogs, busy/queued work, and modified keys keep their existing precedence. |
| `ctrl+c` | Clear the input and queue (while idle) or arm the exit hint (while busy). Press again within 2s to exit. Use `esc` to cancel a running turn. |
| `ctrl+d` | Exit on empty input. |
| `ctrl+b` | Toggle the right sidebar; hidden or narrow widgets use a bounded above-input fallback. |
| `ctrl+l` | Redraw the screen. |
| `ctrl+v` | Paste clipboard text into the focused chat, side chat, dialog, filter, or credential input. In the main chat, image clipboard content is attached to the next prompt when the platform exposes it (macOS pasteboard, Wayland `wl-paste`, or X11 `xclip`). On Linux, text uses `wl-paste`, `xclip`, or `xsel`; terminal-native bracketed paste remains available without those commands. |
| `ctrl+o` | Expand or collapse long tool results (read, write, edit, bash, create_worktree, grep, lsp, and web_search outputs over ~12 lines). |
| `ctrl+1` ... `ctrl+9` | Switch to the model bound to that quick-model slot (configured in `/settings` -> model shortcuts). No-op while a turn is running. |
| `@` | Open the file picker. Browse files and directories in the working directory. |

### File picker (`@`)

| Key | Action |
|---|---|
| `@` | Open the file picker (type after a space or at the start of input). |
| `up`, `down` | Navigate the file list. |
| `right` | Open the selected directory. |
| `left` | Go back to the parent directory. |
| `enter` | Select the file or directory and insert it as a chip (`[file:name]` or `[dir:name/]`). |
| `esc` | Close the file picker. |

Type `@` followed by a filter string to narrow the list (e.g. `@read` shows only entries containing "read"). Selected files are inserted as compact chips that expand to the full path on submit. Dragged-and-dropped files and directories also collapse to chips automatically.

### Editor line navigation

| Key | Action |
|---|---|
| `ctrl+a`, `ctrl+e` | Jump to start or end of line. |
| `alt+left`, `alt+right` | Jump one word back or forward. |
| `ctrl+u`, `ctrl+k` | Delete to start or end of line. |
| `ctrl+w`, `alt+backspace` | Delete the previous word. |
| `up`, `down` | Move within multi-line input. At the top edge, `up` recalls previous prompts and `down` moves forward through prompt history. At the bottom edge, `down` opens the resident-subagent picker when a child exists, preserving the draft. |

### Chat scroll

| Key | Action |
|---|---|
| `pgup`, `pgdn` | Scroll one page up or down. |
| `up`, `down` (editor empty, not browsing prompt history) | Scroll three lines up or down. When resident children exist, `down` opens their picker first. This is how the mouse wheel reaches the scroll logic on most terminals. |

## Extensions

zut can be extended in any language via a subprocess + JSON-RPC protocol. Extensions can register slash commands, expose tools to the model, intercept tool calls (block or rewrite args), gate whole turns before the model is called, and rewrite the assistant's visible text before it reaches the user. None are installed by default; opt in explicitly. Hot-reload any time with `/reload-ext`.

### Install and manage

```bash
zut ext install <path|git-url>   # copy / clone into $ZUT_HOME/extensions/
zut ext install --build=go <path> # explicitly build and install local Go source
zut ext list                      # show installed extensions
zut ext doctor                    # diagnose load, registration, and conflict issues
zut ext logs <name> [-f]          # cat or tail the extension's stderr log
zut ext enable <name>             # re-enable a disabled extension
zut ext disable <name>            # disable without removing
zut ext remove <name>             # delete an extension directory
```

For local installs, zut validates `extension.json` and any executable path
relative to the extension directory before reporting success. It never runs a
build implicitly. Use `zut ext install --build=go <path>` to explicitly build a
local Go extension; other source-based extensions must provide their runtime
artifact or launcher themselves. A failed validation or build leaves no
partial installation behind.

`zut ext doctor` keeps normal extension startup fail-soft, but gives
you an explicit troubleshooting view: manifest errors, disabled or
shadowed extensions, subprocess load errors, ready/auto-ready status,
registered commands/tools, registration conflicts, warnings, and the
stderr log path.

For development, point `zut --ext <path>` at a working directory and skip the install step entirely. Repeatable; takes precedence over installed extensions of the same name.

### Updating extensions

`zut update` refreshes the zut binary **and** every installed extension that lives in a git checkout. Per-extension behaviour:

- Disabled extensions are skipped.
- Extensions without a `.git/` directory (installed by `zut ext install ./local-path`) are skipped — there is no remote to pull from.
- For the rest, zut stashes any dirty worktree state (including untracked runtime files like `todos.json` or `config.json`), runs `git pull --ff-only`, and pops the stash. If the pop produces conflicts, the conflict markers are left in place and you'll see a warning.
- Diverged branches, offline pulls, or any other git failure are reported as `failed` and the next extension is processed. `zut update` itself never aborts because of an extension.
- zut does **not** run any build step (`go build`, `npm install`, `make`) after the pull. Extension authors are expected to commit the runnable artifact (binary, transpiled JS, etc.). If you need a build, run it explicitly, reinstall the extension, or use `/reload-ext` for a working-tree copy.

### Theme-only extensions

An extension may ship only a theme: `extension.json` plus `theme.json` (or `themes/theme.json`) and no executable. zut loads it without spawning a subprocess and shows it in `/settings` with source information. See [docs/themes.md](docs/themes.md).

### Reference

`examples/extensions/` ships reference implementations in Go, TypeScript, Node, and shell, including `tasked-phases` for spec-driven phase/checklist tracking. See [docs/extensions.md](docs/extensions.md) for the full protocol, the SDK API (`packages/agent/ext`), and the phase roadmap.

## Skills

A skill is a per-folder `SKILL.md` file with a YAML frontmatter header. zut discovers skills at startup, surfaces their names in the system prompt, and exposes a built-in `skill` tool the model uses to load the body on demand.

By default zut loads built-in skills plus user-installed skills from:

- `./.zut/skills/<name>/SKILL.md` (project)
- `$ZUT_HOME/skills/<name>/SKILL.md` (global)
- `./.claude/skills/<name>/SKILL.md`, `~/.claude/skills/<name>/SKILL.md` (Claude-compatible layout)
- `./.agents/skills/<name>/SKILL.md`, `~/.agents/skills/<name>/SKILL.md` (agent-compatible layout)

User skill roots are scanned recursively. A nested skill keeps its frontmatter `name` and can also be loaded by its slash-separated path relative to the skill root, such as `systems-backend/subskills/golang-patterns`.

See [docs/skills.md](docs/skills.md) for the frontmatter fields, authoring tips, and example skills under `examples/skills/`.

## Telegram bot (bridge)

zut can run as a telegram bot so you can DM it from your phone. Two ways to run it: **from inside the TUI** (the running session mirrors into Telegram) or **as a standalone background daemon** (a headless bot with its own independent agent).

### From inside the TUI

Type `/telegram` in the running TUI to open a picker with **connect**, **disconnect**, and **status**. When connected:

- DMs from the paired user become prompts in the **same** session you're typing in, so you can continue a conversation from the terminal on your phone and back again.
- Messages you type in the TUI are mirrored into the Telegram thread prefixed `you: ...` and the assistant's replies come back prefixed `zut: ...`, so the Telegram chat stays a complete record of both sides of the conversation.
- Messages sent from Telegram show up as your own bubble in Telegram (no mirror) and the assistant's reply to them comes back bare (no prefix).
- The status bar shows a `- tg -` tag while the bridge is active.
- `/telegram connect` / `/telegram disconnect` / `/telegram status` (or `/tg`) also work as direct commands without the picker.

The in-TUI bridge refuses to start while the standalone daemon (below) is running, since two concurrent long-poll consumers of the same bot race on every update and silently drop messages.

### Standalone daemon

For headless servers or long-running bots unattached to a TUI:

```bash
zut telegram-bot setup     # paste a BotFather token, verify, save
zut telegram-bot run       # foreground: long-poll in this terminal (ctrl+c to stop)
zut telegram-bot start     # background: detach and return immediately
zut telegram-bot stop      # SIGTERM the background bot (SIGKILL after 5s)
zut telegram-bot logs -f   # tail $ZUT_HOME/logs/bot.log (omit -f to just cat)
zut telegram-bot status    # config (token masked) + running/stopped
zut telegram-bot reset     # forget the token and paired user
# short alias: `zut tg ...` is accepted for every subcommand
```

The background flavor writes the child's PID to `$ZUT_HOME/bot.pid` and redirects stdout and stderr to `$ZUT_HOME/logs/bot.log`. `zut telegram-bot stop` reads that PID, sends SIGTERM, waits up to five seconds, then escalates to SIGKILL if the child is still alive. Running two instances at once is refused at startup.

> **Use the installed binary for `start`.** `go run ./cmd/zut telegram-bot start` won't work. `go run` builds a binary in a temp directory and deletes it when it exits, which kills the detached child. Run `make install` (or `go build`) first and invoke the installed binary.

Setup flow:

1. Talk to [@BotFather](https://t.me/BotFather) on telegram, run `/newbot`, copy the token it gives you.
2. Run `zut telegram-bot setup` and paste the token when prompted.
3. Run `zut telegram-bot run` in the directory you want the agent to operate in.
4. Open your bot on telegram, send `/start`. The first user to do this claims the bridge (stored as `allowed_user_id`); every other user is rejected.

From then on, any DM you send is forwarded to the agent as a user prompt. Attached photos or `image/*` documents are downloaded and passed to vision-capable models. In-bot telegram commands are case-insensitive: `/help`, `/status`, `/stop` (cancel the current turn). Config lives in `$ZUT_HOME/bot.json` (mode 0600).

Starting either Telegram bridge removes any webhook configured for that bot before long polling begins, while preserving pending updates. Telegram does not allow webhooks and `getUpdates` polling at the same time, so do not share the bot token with another service that expects to keep a webhook active.

Bot mode respects the usual zut flags: `--provider`, `--model`, `--cwd`, `--reasoning`, `--continue`, `--no-session`, `--no-tools`, and so on. Run `zut tg run -c --model claude-opus-4-1` to resume the latest session on Opus, for example.

### Architecture: protocol-agnostic bot core

The messenger functionality is split in two layers. A generic, protocol-agnostic core lives in `packages/agent/modes/bot`: it owns the turn queue, agent prompting, built-in command dispatch (`/start`, `/help`, `/status`, `/stop`), status formatting, and per-turn credential refresh. Concrete transports implement the small `BotAdapter` interface (inbound polling, sending replies, a typing indicator, and optional protocol-specific status text); the Telegram support in `packages/agent/modes/telegram` is one such adapter.

This means additional messaging backends (Discord, Slack, Signal, and similar) can be added by implementing `BotAdapter` in a new package and wiring up a subcommand. No changes to the runner, agent, or core are required. Channel IDs are opaque strings owned by the adapter, so the shared runner stays free of protocol-specific types.

## Development

```bash
make help      # list the important developer commands
make build     # build ./bin/zut as 0.0.0-dev
make test      # go test -race ./...
make test-fast # go test ./...
make lint      # golangci-lint + go vet + gofmt check
make lint-install # install the pinned golangci-lint version
make fmt       # gofmt -w .
make install   # install the current checkout with go install
make go-install # install the latest published module version
make release VERSION=0.1.0 # cross-compile release binaries
```

Source layout (single Go module, four packages under `packages/`):

```
cmd/zut/                              main()
packages/provider/                    LLM client surface, model catalog, streaming clients
packages/provider/auth/               credential store, api-key probe, oauth, login server
packages/core/                        agent loop, sessions, cost tracking, compaction
packages/tui/                         terminal raw-mode, input parser, editor, renderer, markdown, view
packages/agent/                       cli wiring, arg parsing, system prompt, config
packages/agent/lsp/                    LSP clients, server discovery, linters, diagnostics
packages/agent/extensions/            extension subprocess manager
packages/agent/extproto/              extension wire-format types
packages/agent/modes/                 interactive tui, print, json, dialogs
packages/agent/modes/bot/             protocol-agnostic bot runner (BotAdapter interface)
packages/agent/modes/telegram/        telegram adapter, api client, daemon
packages/agent/tools/                 read, write, edit, bash, create_worktree, grep, lsp, web search, goals, sandbox
packages/agent/skills/                skill discovery, frontmatter parser, skill tool
packages/agent/subagents/             named profiles and resident background runtime
packages/agent/sdk/                   public Go SDK for embedding zut in-process (package sdk)
packages/agent/ext/                   public Go SDK for writing extensions (package ext)
```

Downstream consumers can depend on individual packages:
`go get github.com/bnema/zut/packages/core` pulls only `core` and its transitive deps (today: `provider`), no agent or TUI code.

## License

MIT
