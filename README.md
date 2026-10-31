# zut

> [!NOTE]
> **Fork notice:** `zut` is an independent fork of [zot](https://github.com/patriceckhart/zot), adapted for my workflow and available to others.

## What is it?

A lightweight coding agent harness written (vibe-slopped) in Go.

- One static binary.
- Providers: Anthropic, OpenAI/Codex/Responses, Kimi, DeepSeek, Google Gemini/Vertex, GitHub Copilot, Bedrock, Azure OpenAI, OpenRouter, Groq, Cerebras, xAI, Together, Hugging Face, Mistral, Moonshot, Z.AI, Xiaomi, MiniMax, Fireworks, Vercel AI Gateway, OpenCode, Cloudflare AI, and Ollama/local models.
- seventeen core built-in tools (read, write, edit, bash, python, worktree, grep, glob, ast, lsp, web_search, web_open, web_find, web_click, update_goal, plan, ask_user_question), plus `schedule` and `sessions` in interactive daemon sessions, `subagent` for delegation, and `skill` when skills are enabled. [Public-web egress and availability](docs/web-search.md).
- five run modes (interactive TUI, print, stream, JSON, RPC).
- Built-in Telegram bot.
- Native MCP client: stdio and streamable HTTP, shared by primary agent, orchestrator, and subagents. No built-in OAuth; use a stdio bridge such as mcp-remote. [MCP](docs/mcp.md).
- Any-language subprocess + JSON-RPC extensions. None installed by default; opt in with `zut ext install` or `zut --ext`. Interactive tools allow user input without a reply deadline and cooperative cancellation; headless hosts reject them. [Extensions](docs/extensions.md).
- User/extension JSON [themes](docs/themes.md).
- Global/project `AGENTS.md` [standing instructions](#persistent-instructions-agentsmd).
- Reusable `SKILL.md` [instructions](docs/skills.md).
- Named [subagent profiles](docs/subagents.md) in `~/.agents/agents/*.md`.
- [Portable agents](docs/zutfiles.md): local directories, `.zut` files, or temporary public GitHub downloads.

## Install

### One-liner (macOS, Linux)

```bash
curl -fsSL https://git.bnema.dev/zut/raw/main/install.sh | bash
```

Detects OS/architecture, downloads the latest git.bnema.dev release, verifies SHA-256 against `checksums.txt`, and extracts to the first writable directory: `/usr/local/bin`, `~/.local/bin`, or `~/bin`. Pin a version or prefix:

```bash
curl -fsSL https://git.bnema.dev/zut/raw/main/install.sh | bash -s -- v0.1.0 ~/bin
```

### One-liner (Windows, PowerShell)

```powershell
iwr -useb https://git.bnema.dev/zut/raw/main/install.ps1 | iex
```

Installs `zut.exe` in `$HOME\bin`, adding it to the user PATH if needed. Open a new terminal.

### go install

```bash
go install github.com/bnema/zut/cmd/zut@latest
```

Reports the tagged module version; supports `zut update`. Update checks/changelogs try ermite (`git.bnema.dev`), then GitHub on API failure. Archives/checksums come from the answering source, pinned to the release tag.

Initial downloads require HTTPS on `git.bnema.dev` or `github.com`. Redirects also allow `objects.githubusercontent.com` and `release-assets.githubusercontent.com`, without forwarding the GitHub token. Release JSON and checksums are each limited to 1 MiB.

### From source

```bash
git clone https://github.com/bnema/zut
cd zut
make help         # list the important developer commands
make build        # produces ./bin/zut (0.0.0-dev by default)
make install      # install the current checkout with go install
make go-install   # install the latest published module version
```

`make go-install GO_INSTALL_VERSION=v0.1.0` pins a published version; `make install` installs the checkout as a development build.

### Prebuilt binaries

[Releases](https://git.bnema.dev/zut/releases) include Linux, macOS, and Windows archives for amd64/arm64 (except windows/arm64), plus `checksums.txt`. Download, verify, `chmod +x`, and place on `$PATH`.

## Authenticate

Run `zut`, then `/login` for browser login. The TUI opens without credentials.

### Credential lookup order

1. `--api-key`
2. Provider env var: `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `KIMI_API_KEY`, `MOONSHOT_API_KEY`, `DEEPSEEK_API_KEY`, `GEMINI_API_KEY`, `GOOGLE_API_KEY`, `GROQ_API_KEY`, `OPENROUTER_API_KEY`, `MISTRAL_API_KEY`, `XAI_API_KEY`, `CEREBRAS_API_KEY`, `TOGETHER_API_KEY`, `HF_TOKEN`, `ZAI_API_KEY`, `XIAOMI_API_KEY`, `MINIMAX_API_KEY`, `FIREWORKS_API_KEY`, `AI_GATEWAY_API_KEY`, `COPILOT_GITHUB_TOKEN`, `GITHUB_COPILOT_TOKEN`, or other backend-specific variables.
3. `$ZUT_HOME/auth.json` (API key/OAuth token; mode 0600).

`$ZUT_HOME` defaults to `$XDG_STATE_HOME/zut` on all platforms when `XDG_STATE_HOME` is set. Otherwise:
- macOS: `~/Library/Application Support/zut`
- Linux: `~/.local/state/zut`
- Windows: `%LOCALAPPDATA%\zut`

### API keys from commands

Keep keys in a password manager with a provider's `api_key_command`:

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

For `models.json` providers, put this credential object under `additional_api_key_creds`, keyed by provider ID.

- `program` runs directly, without a shell; separate arguments in `args`. Optional `timeout_ms` defaults to 120 seconds.
- Runs only when selecting that provider, not for login-status checks or background model-catalog refreshes.
- Successful output stays in memory for the process, never on disk. Print one non-empty stdout line; trailing CR/LF is stripped. Output limit: 64 KiB. Errors omit command output.
- Saving a key with `/login` replaces the command; `/logout` removes it.

**Treat `auth.json` as executable configuration:** anyone able to modify it can run programs as your user. zut neither interprets `!` prefixes nor runs command strings through a shell.

### `/login` flow

Run `zut`, type `/login`, and choose:

- **API key:** browser form on `127.0.0.1:<free-port>`. Choose from the full API-key provider list and paste a key. Lightweight model-list endpoints are probed before saving to `auth.json`; backends needing extra project/account env vars are saved directly.
- **Subscription:** Claude Pro/Max, ChatGPT Plus/Pro, Kimi Code, SuperGrok/X Premium, or GitHub Copilot. DeepSeek and Google Gemini require API-key login.
  - Anthropic/OpenAI callbacks require fixed ports: `localhost:53692` / `localhost:1455`, respectively, as required by their auth servers.
  - Anthropic: Claude Code OAuth, with browser callback or copy-code choices; messages use `api.anthropic.com`, bearer token, and Claude Code identity headers.
  - Anthropic/OpenAI Codex browser login accepts a pasted callback URL or code from the same transaction; supplied state must match. `esc` cancels. Codex has no standalone manual-code login.
  - OpenAI: Codex CLI OAuth; messages use `chatgpt.com/backend-api/codex/responses` with `chatgpt-account-id` from the returned id_token.
  - Kimi: Kimi Code device-code OAuth; opens verification URL and polls for browser approval. Messages use `api.kimi.com/coding/v1` with Kimi Code identity headers.
  - xAI: device-code OAuth; opens prefilled authorization URL, polls for approval, then uses the token with the xAI API.
  - GitHub Copilot: GitHub device-code login; stores the GitHub access token and exchanges it for short-lived inference tokens on demand. Claude uses Messages; Responses-tagged models use Responses; others use Chat Completions. Account-specific discovery filters the picker without enabling policies. [GitHub Copilot](docs/providers.md#github-copilot).

> Subscription login reuses published OAuth client IDs from Claude Code CLI, Codex CLI, Kimi Code CLI, xAI's device flow, and GitHub Copilot's device-code flow. Third-party use may violate their terms or be revoked anytime. Use at your own risk; API keys are the safe default.

### Compact status bar

Combines model/reasoning level; rounds cache/context percentages and costs:

```text
gpt-6-astra:medium  ↑261k ↓34k R12M C98% $8.52 plan:2/3 ctx31%/500k
```

- `↑` / `↓`: session input/output tokens; `R`: cache-read tokens; `C`: measured cache-hit rate.
- `plan`: current/total steps, only with an active plan. `ctx`: context used/model capacity.
- Values have stronger contrast than labels in dark/light terminals. Provider prefix and `(sub)` are omitted; `/session info` shows the provider.
- Subscription dollars estimate API-equivalent cost, **not an additional charge**. Detailed session/subagent views retain precision and billing labels.

### Token refresh

OAuth access tokens expire (Anthropic ~8h, OpenAI ~30d). Kimi, xAI, and GitHub Copilot also refresh/exchange automatically.

- Each credential lookup checks stored `expiry` with a 60s safety margin. When due, calls the provider's `oauth/token` with `refresh_token`, saves new `access_token`, `refresh_token`, and `expiry` to `auth.json`, and supplies the fresh token.
- Telegram also refreshes each turn for long-running bots.
- Refresh failures (revoked `refresh_token`, account logged out everywhere) appear in the TUI status line or bot DM. Run `/login` for fresh tokens.

All data lives under `$ZUT_HOME`:

```
$ZUT_HOME/
├── config.json         # last-used provider/model/theme and persistent settings
├── auth.json           # api keys and oauth tokens (mode 0600)
├── sessions/           # jsonl transcripts, one dir per cwd
├── models-cache.json   # live model discovery cache (24h ttl)
├── AGENTS.md           # optional: global instructions appended to the prompt
├── SYSTEM.md           # optional: replaces the built-in identity; addenda remain
├── skills/             # optional: user SKILL.md files
├── themes/             # optional: user theme JSON files
├── extensions/         # installed extensions, one dir per extension
└── logs/               # app log files
```

`$ZUT_HOME/SYSTEM.md` replaces built-in identity/docs guidance every run; delete it to restore defaults. Shared harness guidance and append addenda (`AGENTS.md`, skills) remain. `--system-prompt` overrides it per invocation, except a selected `systemPromptMode: replace` subagent profile sets the child's identity even with a parent override. Append-mode profiles retain shared guidance/global addenda.

Shared guidance applies across identities, providers, and models:
- Complete authorized, non-destructive work, not just plans; respect explanation/review requests and required approvals.
- User instructions outrank skills. Skills cannot grant permissions or override system/developer constraints.
- Write concise paragraphs, useful lists, and direct language, without canned transitions.
- Run scope-appropriate checks; report checks run and anything unverified.

These are prompt instructions, not behavioral guarantees or changes to tool permissions, confirmations, jail behavior, or sandboxing.

### HTTP proxy

Set `http_proxy` in `$ZUT_HOME/config.json` for zut-managed HTTP/HTTPS:

```json
{
  "http_proxy": "http://127.0.0.1:7890"
}
```

Applied at startup; restart after edits. Existing `HTTP_PROXY`, `HTTPS_PROXY`, `http_proxy`, and `https_proxy` env vars win for their protocol. `NO_PROXY`/`no_proxy` control bypasses. Prefer protected env vars for proxy credentials: `config.json` is not a credential store.

## Persistent instructions (AGENTS.md)

`AGENTS.md` adds standing guidance **on top of** the default system prompt, unlike `SYSTEM.md`'s identity replacement. For example, use it to discourage local models from editing immediately.

Startup discovery/appending order:
1. `$ZUT_HOME/AGENTS.md`: global, every project.
2. Every `AGENTS.md` from filesystem root to cwd, including `~/AGENTS.md` when inside your home. Deeper files may override earlier ones.

Enable **show loaded resources at startup** in `/settings` to list the active Zutfile agent, loaded context paths, extensions, and user-installed skills above the interactive transcript. Default: off. Agent section: only `zut run`. Built-in skills are omitted; lists are neither sent to the model nor stored in transcripts.

Example global rules in `$ZUT_HOME/AGENTS.md`:

```markdown
Treat questions and discussions as requests for explanation. Do not edit files or run tools unless explicitly asked to make a change. Ask before modifying code.
```

| Mechanism | Scope | Effect |
|---|---|---|
| `$ZUT_HOME/AGENTS.md` | global, every run | appended to default prompt |
| `./AGENTS.md` (and parent dirs) | project | appended to default prompt |
| `$ZUT_HOME/SYSTEM.md` | global, every run | replaces identity/docs guidance; append addenda remain |
| `--append-system-prompt <text>` | single run, repeatable | appends |
| `--system-prompt <text>` | single run | replaces identity/docs guidance; append addenda remain |

> zut does not read `CLAUDE.md`; only Claude-compatible skills under `.claude/skills/`. Move Claude Code instructions into global/project `AGENTS.md`.

## Changelog on update

On first launch after an upgrade, the TUI shows dismissible release notes; any key closes them. `config.json` records `last_changelog_shown` to avoid repeats. Fresh installs show none.

Development builds (`0.0.0-dev`) skip update/changelog requests and print `dev version detected; skipping update check`. Release fetches are best-effort: network failure or missing release page silently skips and retries next launch.
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
zut --resume <ID>                # load a saved session by daemon ID or transcript UUID
zut --list-models                # show supported models
zut --help
```

Print stats contain `provider`, `model`, `prompt_tokens`, `reasoning_tokens`, `generated_output_tokens`, and `elapsed_ms`, covering all prompt-triggered turns and tool loops. Prompt tokens include cache reads/writes. If separate reasoning counts are unavailable, `reasoning_tokens` is `null` and `generated_output_tokens` is total provider output, possibly including reasoning. Elapsed time excludes startup and credential resolution. Stats are written only on success.

## Flags

| Flag | Description |
|---|---|
| `--provider <id>` | Provider, e.g. `anthropic`, `openai`, `openai-codex`, `kimi`, `google`, `github-copilot`, `groq`, `openrouter`, `amazon-bedrock`, `ollama`; see Providers. |
| `--model <id>` | Model; see `--list-models`. |
| `--api-key <key>` | Override API key. |
| `--base-url <url>` | Override provider URL for tests/self-hosting. |
| `--insecure` | Skip TLS verification only for explicit `--base-url` or user-model `baseUrl` in `models.json` (self-signed local/internal servers). Built-in providers, auth, and model discovery retain TLS verification. |
| `--system-prompt <text>` | Replace default prompt, overriding `$ZUT_HOME/SYSTEM.md`. |
| `--append-system-prompt <text>` | Append system text; repeatable. |
| `--reasoning off\|minimum\|low\|medium\|high\|xhigh\|max` | Supported models only; default `off`. `max` is opt-in, above `xhigh`. |
| `--stats <path>` | Write JSON generation stats with `-p`/`--print`; incompatible with `--orchestrate`. |
| `--orchestrate` | Explicit bounded delegation and final synthesis in print, stream, or JSON mode. |
| `-c`, `--continue` | Resume latest session for this cwd. |
| `-r`, `--resume [ID]` | Pick a session, or load by daemon ID/transcript UUID. |
| `--session <path>` | Resume a session file. |
| `--no-session` | No session-file reads/writes. |
| `--cwd <path>` | Set working directory. |
| `--no-tools` | Disable all tools. |
| `--no-repetition-guard` | Disable repeated-content loop detection for intentional polling. |
| `--no-lsp` | Disable built-in LSP/linter tool. |
| `--tools <csv>` | Enable only listed tools; name `lsp` explicitly. Include `web_search` to retain/enable all four public-web tools (`web_search`, `web_open`, `web_find`, `web_click`), overriding the persisted web-search setting. |
| `--max-steps <n>` | Agent-loop limit; default unlimited. |
| `-e`, `--ext <path>` | Load extension; repeatable, overrides installed namesakes. |
| `--no-ext` | Skip discovery; explicit `--ext` still works (`--no-ext --ext ./x` loads only `x`). |
| `--no-skill` | Disable all skills, including built-ins, the `skill` tool, and system-prompt skill manifest. |
| `--no-context-files`, `-nc` | Skip global/project `AGENTS.md` discovery and loading. |
| `--no-yolo` | Confirm each tool call in interactive TUI: name/one-line args preview, or edit diff in tool panel. Choices: yes, yes-always-this-tool-this-session, yes-always-this-session, no. `/` opens a slash command; closing its input/child dialog returns to confirmation. Print/JSON/RPC ignore this flag with a stderr warning; tools run freely. |

## Tools

- `read`: read text or inline images (PNG, JPEG, GIF, WebP).
- `write`: create/overwrite files, creating parent directories.
- `edit`: apply exact-match replacements to an existing file.
- `grep`: bounded file/directory regex search within the active read scope, using ripgrep or portable grep, never a shell. Fallback regex, ignore, and hidden-file behavior may differ.
- `glob`: match `**/*.go`, `*.json`, `src/**/*.ts`, etc.; supports `*`, `?`, `**`, character classes, matching relative paths with `/`, basenames otherwise. Honors nested `.gitignore`, skips hidden entries unless requested, never follows nested directory symlinks; depth 24, at most 500 sorted matches.
- `ast`: structural search/rewrite with [ast-grep](https://ast-grep.github.io/); prefer for code syntax, `grep`/`edit` for text (`grep` suggests `ast` for call/definition-like regexes when available). Optional `language` defaults to extension inference; directly runs separately installed `ast-grep` or verified ast-grep `sg`, without a shell, returning compact `path:line` matches or a clear missing-tool error. Rewrites: plan in memory, confirm diff, check filesystem scope/current contents, write via temporary file and rename preserving permissions, then bounded diagnostics.
- `bash`: run in session cwd with merged stdout/stderr; a positive integer `timeout` (seconds) is required and enforced. Unix tries `/bin/bash -c`, then PATH `bash -c`, then POSIX `/bin/sh -c`; Windows uses `cmd /C`; macOS's default Bash 3.2 lacks newer features.
- `python`: run nonblank `code` in a fresh, stateless Python 3 subprocess in session cwd (`-u -B -`, code on stdin, merged stdout/stderr), requiring positive integer `timeout` seconds shared by discovery/execution; no installs, script file, bytecode cache, persistent state, or interactive input (EOF). Discovery: `python_interpreter` in `$ZUT_HOME/config.json` (one executable path, relative to cwd), active `VIRTUAL_ENV`, `./.venv`, then PATH `python3`/`python`; Windows rejects Store aliases, `py` launchers, and install-manager entrypoints, requiring a positively identified installed interpreter. Output: 50 KiB/2,000 lines with truncation marks; truncated byte-capped buffer persists to a temp file shown as `full output`/`full_output_path`, like `bash`; imports fail normally, args/output enter session history, deliberate file writes persist (use `write`/`edit` for reusable scripts, not opaque Python edits); unavailable to packaged agents or while jailed.
- `worktree`: `create` checks out a new branch from HEAD or `base` (branch/tag/commit) under `$ZUT_HOME/worktrees/<repo-id>`, overridden per repository by absolute Git setting `zut.worktrees.path`; `list` inventories registered worktrees and accepts `into` for eligibility checks. `cleanup` requires exact paths, normal preview confirmation, and immediate revalidation; refuses primary, dirty, detached, or unmerged checkouts (merge target: current branch or `into`), never removing unmerged work; `delete_branch` deletes removed worktrees' local branches only at the inspected commit, never the target or branches checked out elsewhere, and reports deletion failures without restoring removed worktrees. Workflow: create `feat/x` from main, sub-worktrees with `base: "feat/x"` (e.g. `feat/x-api`), commit/merge into `feat/x`, then clean with `into: "feat/x"`, `delete_branch: true`.
- `lsp`: configured servers/linters provide diagnostics, definitions, references, hover, symbols, renames, code actions, capabilities, and raw requests over stdio JSON-RPC; project `lsp.json` adds/overrides servers and CLI linters. Diagnostics are bounded, sorted, deduplicated by path/severity/code/start, and repeated issues grouped; successful native edits, writes, renames, and applied code actions attach bounded diagnostics when available. See [docs/lsp.md](docs/lsp.md).
- `web_search`: DuckDuckGo HTML search returns bounded titles, URLs, snippets, opaque references; `web_open` opens a full public URL/reference, `web_find` finds literal text in an opened page, `web_click` follows a displayed link. All four default on in normal CLI sessions, unavailable in bot, default SDK, and packaged agents; see [docs/web-search.md](docs/web-search.md).
- `update_goal`: start a mission if none exists, set its next bounded goal, pause for a decision, resume, mark a goal `done`/`blocked`, or atomically supersede an invalid goal with a necessary in-mission replacement. Users can pause/resume with `/goal`; only users clear goals.
- `ask_user_question`: up to 10 chained questions, optional single/multi-select and custom answers; interactive terminal only, not background agents. Arrows/j/k move, digits pick, Space toggles multiple choices, Enter confirms, Shift+Tab goes back, Esc declines/exits custom-text editing.
- `plan`: session checklist states `pending`, `in_progress`, `completed`; actions `set` (replace all; at least one step), `add`, `update`, `remove`, `clear` (remove all), `show`; concurrent `in_progress` steps allowed. Unlimited steps, first 12 shown after compaction; persisted in every mode via tool-result commits, inherited by forks/imports, cleared by `/clear`. Updates display inline and emit JSON/RPC `plan_update` events.
- `schedule`: add/list/cancel recurring five-field cron prompts, capturing current machine timezone. Due tasks wait for active turns, run attached or detached, record output in the transcript; `$ZUT_HOME/daemon/schedules.json` survives daemon restart, but session closure (including idle GC) cancels tasks. Unavailable in print/stream/JSON/RPC.
- `sessions`: inspect/message other open daemon sessions: `list` shows ID, attach/run state, model, directory, title; `tree` groups by repository with resident subagents, plan step, goal. `list`/`tree` always allowed; `send` targets ID, unique prefix, or exact title (even detached), delivering a tagged message as a turn, but requires an attached sender and refuses message-triggered turns to prevent loops.

Tool results retain resource provenance in durable history. Before provider requests, an equivalent newer read replaces an older one; multi-file searches are replaced only after all discovered files are consumed/mutated. This provider-only stale-context projection leaves history intact. Changed older results restart prompt caching there; compact deterministic markers limit cost.

With `/jail`, filesystem tools, `grep`, `glob`, `ast`, and LSP workspace edits reject paths outside session cwd. `worktree` also confines repository root, Git metadata, and resolved destination. `python` is rejected at admission: the jail cannot confine arbitrary Python (use `/unjail`); when admitted, it has unrestricted host filesystem/network/process access like `bash`. Jail does not sandbox web-search egress.

## Modes

- **Interactive** (default): streaming chat TUI, spinner, cost meter, slash commands. `/settings`, `/subagents`, pickers, and confirmations use focused floating panes over the dimmed live view; below 80 columns, full-width bottom drawers.
- **Print**: `zut -p "prompt"` runs to completion; stdout contains only final assistant text.
- **Stream**: `zut --stream "prompt"` skips TUI, streams assistant text to stdout, tool activity to stderr.
- **Piped input**: stdin implies print unless a mode is explicit. Print/stream/JSON prepend stdin to the positional prompt with a newline: `cat README.md | zut "summarize this text"`.
- **JSON**: `zut --json "prompt"` writes one agent event per stdout line; schema: [docs/rpc.md](docs/rpc.md).
- **RPC**: long-lived `zut rpc` child bridges a daemon session; stdin commands, stdout events/responses, all NDJSON. Any-language embedding; schema: [docs/rpc.md](docs/rpc.md), clients: `examples/rpc/{python,node,shell,go}`.

### Headless orchestration

`--orchestrate` enables bounded resident delegation only in print/stream/JSON, never implicitly. The strict manager-only parent splits non-overlapping child scopes, coordinates workers, and yields rather than implementing/repeating delegated work. Requires `subagent` spawn: default tools or `--tools subagent:spawn`, unavailable with `--no-tools` or packaged-agent `PermissionSet`. Interactive/RPC and `--stats` are incompatible.

Children inherit CLI provider/model/reasoning unless profile/spawn overrides, not the strict manager role. Parent receives profile metadata via `[subagents_list]` before selection; only the selected body and child-specific configuration load afterward. See [docs/subagents.md](docs/subagents.md).

Headless completion waits for every accepted child; `required:true` adds a durable obligation surviving terminal failure/interruption. Print emits final synthesis only; stream emits all parent turns to stdout, tool diagnostics to stderr; JSON preserves all parent events, including completion `user_message` updates. Child output is evidence for the next parent turn, not a host log.

Resident management enforces global fair concurrency (default six turns), optional queue timeout, required-result gating, and cancellation. Limit: 32 completion follow-up waves per headless invocation. Cancellation shuts down parent and all resident children normally; children cannot spawn descendants. Profile manifest/lifecycle: [docs/subagents.md](docs/subagents.md).

If an initial print/stream/JSON request exceeds context, zut compacts existing history and retries the already-appended prompt once. Print emits recovered final text only; stream keeps text on stdout/diagnostics on stderr; JSON suppresses the recoverable first terminal error to preserve successful JSONL. Compaction failure or a second context error is terminal, not general autonomous follow-up. Resident child turns get the same recovery; RPC/SDK/bot/Telegram do not.

A normal turn with no final answer (empty or commentary-only) gets one bounded continuation in the same turn. Interactive shows `Continuing: no final answer received.`; print emits only the recovered answer; stream sends text to stdout, notice to stderr; JSON/RPC emit `turn_recovery` with `reason` (`missing_answer`/`commentary_only`) and `attempt` 1. Still no answer: visible incomplete-turn error, not silent success. Recovery neither guarantees completion nor overrides permissions, confirmations, or denials.

## zutfile agents

Zutfiles share instructions, skills, requirements, and enforced tool permissions. Run a directory, packed `.zut`, short name, or public GitHub source:

```bash
zut run ./my-agent
zut run ./my-agent.zut
zut run frkr/zot-archify
zut run acme/agents/code-reviewer --cwd /path/to/project
```

Single-part names resolve only to local directories/`.zut` archives. Anyone can publish `owner/repository` or a collection's `owner/repository/agent`; no built-in owner, official collection, configured registry, or allowlist. GitHub archives download to temporary storage, are validated/run, then removed on exit; agent data, consent receipts, and sessions persist in `$ZUT_HOME`. Authoring, permissions, packaging, limits: [docs/zutfiles.md](docs/zutfiles.md).

## Embedding

- **Go in-process**: import `github.com/bnema/zut/packages/agent/sdk`; one `Runtime` per project, `Prompt(ctx, text, images)` returns an `Event` channel. Credential-/endpoint-scoped model cache loads before runtime resolution, including dynamic OpenCode Go metadata. `sdk.Config.MaxSteps` zero means unlimited; set `50` to retain the former default. Example: `examples/sdk/`.
- **Any language, out-of-process**: spawn `zut rpc`, exchange NDJSON on stdin/stdout. Schema: [docs/rpc.md](docs/rpc.md); clients: `examples/rpc/`.

Both use the same event schema; transcripts from either can replay through the other.
## Slash commands

TUI and messaging commands ignore name case, not argument case. Type `/` in the TUI for autocomplete.

| Command | Description |
|---|---|
| `/help` | Show keys and commands. |
| `/info` | Show session ID, full transcript path, cwd, provider, and model. |
| `/login` | Open API-key or subscription login. |
| `/logout [provider]` | Clear one provider's credentials, or all if omitted. |
| `/model` | Pick a model, or set it with `/model <id>`. |
| `/profile <1-9>` | Activate a [model profile](#model-profiles). |
| `/reasoning` | Set reasoning for subsequent calls. |
| `/fast` | Toggle fast mode. |
| `/orchestrator` | Toggle proactive delegation. |
| `/goal <objective>` | Start an autonomous interactive mission. |
| `/llama` | Manage the configured llama.cpp router and GGUF models. |
| `/sessions` | Resume a session. |
| `/fork` | Branch after a past user turn without starting a provider call. |
| `/session` | Inspect, export, import, fork, or browse the current session. |
| `/jump` | Scroll to a turn, optionally filtered with `/jump <text>`. |
| `/btw` | Open a full-context side chat outside the main thread. |
| `/subagents` | Manage resident background agents. |
| `/skills` | Browse discovered SKILL.md files. |
| `/compact` | Summarize the transcript to free context. |
| `/study` | Read the current directory or a supplied path. |
| `/jail` | Confine tools to the cwd. |
| `/unjail` | Remove the session's confinement. |
| `/reload-ext` | Re-read extension manifests, respawn subprocesses, and rebuild the tool registry. |
| `/telegram` | Manage the Telegram bridge (alias `/tg`). |
| `/settings` | Change persistent settings. |
| `/clear` | Clear the transcript. |
| `/detach` | Leave the TUI with the daemon session running for `zut attach <id>`. |
| `/exit` | Close the daemon session and exit zut. |

- `/logout openai-codex` clears ChatGPT/Codex subscription auth but preserves a public OpenAI API key. `/logout kimi` also disables fallback to the official Kimi Code CLI token until you log in to Kimi through zut again.
- `/llama` appears after llama.cpp login is configured. Connect to the router, load/unload/remove cached models, or search/download Hugging Face GGUFs with live progress.
- `/study` runs "Read and understand everything in the current directory." before targeted questions. Supply a typed, drag-dropped, or `@`-selected path to narrow it: `/study [dir:packages/]`, `/study cmd/zut/main.go`.
- `/telegram [connect|disconnect|status]` opens a picker without an argument. When connected, paired-user DMs become prompts in the running session; assistant replies are mirrored to Telegram.

Extension commands appear below a divider in autocomplete, sorted by name.

### `/goal`

The first interactive request creates a durable user-owned mission. `/goal <objective>` starts/replaces its goal and works immediately. While active, each idle period starts one leased hidden follow-up; queued user input goes first.

The main agent can use `update_goal` to:
- Start a mission when none is active.
- Pause for a decision, resume once settled, settle the goal, or set a bounded next goal.
- Atomically mark an invalid objective `superseded` and replace it within the same mission, never to broaden scope or avoid hard work.

`mission_id` defaults to the current mission and must match if supplied. Subagents cannot start/update missions. Status-bar state and `/goal history` persist with the session.

Token use is recorded but unlimited unless `config.json` sets a budget:

```json
{
  "goals": {
    "max_token_budget": 1000000
  }
}
```

`max_token_budget` limits new goals; reaching it sets `budget_limited`.

- Output-limit truncation compacts durable state, then resumes in a fresh leased turn; it is not no progress or a blocker.
- A turn without concrete tool action gets one corrective continuation, then becomes `stalled`.
- `esc` cancels only the turn, not the goal. The next message runs first; incidental input is followed by one automatic return to the goal.
- Terminal provider errors block the goal rather than retry forever.
- `/goal` inspects; `/goal history` shows history; only `/goal pause` pauses execution; `/goal resume` continues; `/goal clear` removes it.

Complete-transcript branches inherit goals; earlier forks clear them because the objective may not yet exist. Manual `/compact` retains the active goal without follow-up; use `/goal resume`.

### Shell escape (`!command`)

Prefix a command with `!` to run it without the model. The remainder goes to the `bash` tool's shell in the session cwd, honoring `/jail`:
- Unix: `/bin/bash -c`, then `bash -c` from `PATH`, then POSIX `/bin/sh -c`.
- Windows: `cmd /C`.

Escapes always time out at 600 seconds (10 minutes). Command, merged output, and exit status enter the transcript as user context, without starting a model turn. `esc` cancels; escapes cannot start during an agent turn or another escape.

### `/sessions`

Daemon sessions come first, marked `this terminal`, `attached` elsewhere, or `detached`. Selection switches this terminal; the released session keeps running. Previous cwd sessions follow newest first, with timestamp, model, message count, cost, and title.

Keys:
- `Tab`: switch **this directory** / **all sessions** within the active normal or Zutfile-agent namespace; other agent namespaces never appear.
- `/`: search contiguous persisted user/assistant text (minimum two characters). The temporary in-memory corpus excludes tool calls/results, images, developer context, and compaction payloads. Queries reuse it without reopening transcripts; results show counts and first highlighted excerpts.
- `up`/`down`, `enter`: select as soon as an entry appears; entries and search corpus load in the background.
- `r`: rename; `d`: confirm deletion. Active session protected; sessions open in other zut processes are undetectable and deleted like ordinary files.
- `esc`: clear search or cancel/close.

Resume switches workspace to the recorded cwd before replay, updating tools, instructions, sandbox, and file suggestions. It restores provider/model, rebuilding the agent if needed. Failed staged switches leave the current session unchanged.

Tree-navigation branches appear only in `/session tree`. Unavailable with `--no-session`. Fresh interactive sessions get a concise model title after the first real prompt; resume restores it (see AI terminal titles).

### `/session`

`/session` opens an operation picker; direct forms include `/session info`, `/session timeline`, `/session export [path]`, `/session import <path>`, `/session fork`, and `/session tree`.

- **`/session timeline`**: read-only floating inspector of the current system prompt, visible messages, reasoning summaries, and tool calls.
  - `up`/`down` or `pgup`/`pgdn`: select an event.
  - `tab`/`shift+tab`: summary, payload, result, schema, or transcript-derived timing.
  - `left`/`right` or `home`/`end`: scroll details; `/`: search; `esc`: return.
  - `ctrl+e`: export JSON to the default export directory, with `0600` permissions where supported.

  Images show MIME/size; private provider signatures and encrypted reasoning are omitted. Review conversation/tool text before sharing. Context estimates use bytes, not exact tokens.
- **`/session export [path]`**: save the running main transcript to `~/Downloads/<timestamp>-<session-id>-<prompt-slug>.zutsession` by default. Override with a path: directories get dated filenames; bare names gain `.zutsession`. Strips metadata cwd to hide filesystem layout. Includes messages, tool calls/results, compactions, usage, **not `/subagents` child journals**. Copy those machine-local conversations manually from child views.
- **`/session import <path>`**: copy a `.zutsession` into `$ZUT_HOME/sessions/<cwd-hash>/` with fresh ID/current cwd, then switch to it. Imports appear in `/sessions`, `/jump`, and the current-family tree when applicable. Drag-dropped paths work; surrounding quotes are stripped.
- **`/fork`**, also **`/session fork`**: a `/jump`-style picker copies through a past user turn into a new branch, switching without a provider call. Parent stays on disk. Try another question or rewind; tree user rows instead restore editable prompts with images.

  Daemon `/fork`, tree branches, and `/btw` inherit only registered-parent launch settings: cwd, tool/extension policy, confirmation/sandbox, environment, zutfile permissions. No client overrides; unregistered parents fail with `parent session not found`.
- **`/session tree`**: browse root ancestor/descendants, not unrelated cwd roots. Opens immediately; history loads in the background, newest first. `up`/`down`, `enter` select available rows; `esc` cancels loading or closes.

  Pre-compaction user/assistant messages remain forkable; tools, compaction checkpoints, shell/image helpers, and internal context are hidden. Messages indent at branch points; `[current]` marks the endpoint. Empty/detached points have boundary rows. Fork points lost to compaction appear detached under the parent's current tail. Parentless/orphaned sessions are roots.

  User rows branch **before** the message, restoring editable text blocks/images with no provider call until submission. Assistant rows check out safe message boundaries; empty/detached boundaries open an empty editor. Navigation branches appear only in the tree.

  When daemon-attached, user rows check out a hidden branch, then restore the draft after attachment. Edit and Enter to resend there.

  Tree navigation requires a readable complete current family and no active turn, shell escape, compaction, startup/session load, dialog, picker, suggestion, or queued message.

  **Double Escape:** two parsed, unmodified bare `Esc` events within 500 ms open the tree under the same checks, from an idle editor without text/pending images. Intervening keys/timeouts reset; modified Escape does not count. Dialogs, confirmations, panels, pickers, and suggestions retain Escape behavior; busy Escape cancels. If terminal input cannot supply those events, use `/session tree`.

### `/jump`

One row per user prompt shows turn number, tool count, and first prompt line.
- `up`/`down`: select; `enter`: jump; `esc`: cancel.
- Printable characters extend the filter; backspace narrows it.
- `/jump <text>` pre-filters and jumps directly if exactly one turn matches.

Only the viewport changes, not the transcript. The turn appears at the top with `viewing turn N of M, pgdn to catch up`. Reach the bottom with `pgdn` or arrow scrolling to clear the indicator.

### `/btw`

This overlay freezes main context. Each isolated turn sees `system + main transcript + side-chat history so far`, with main tools/guards, including per-tool `--no-yolo` confirmation.

```
/btw                              # open the overlay, type questions interactively
/btw does PUT replace the whole resource?
```

`enter` sends; `esc` cancels calls or closes if idle; `ctrl+c` closes immediately. Replies/tools stay in the overlay, never enter the main transcript or session file, and are never re-read by later main turns.

### `/subagents`

Resident subagents are in-process conversations, not extra `zut` processes, each with stable session ID, provider client, tool registry, cancellation boundary, durable journal, and structured latest-turn result.

> **V0.x breaking changes:** old subprocess state is ignored and unsupported; jobs are never migrated or replayed. Remove old state when no longer needed. The `subagent_spawn`, `subagent_status`, `subagent_stop`, and `subagent_resume` tools are replaced by `subagent` with an `action`. Replace `--tools subagent_spawn` (now selects nothing) with `--tools subagent:spawn`, or `--tools subagent` for all actions. Under `--no-yolo`, "always allow" is per action: approving `subagent:status` never approves `subagent:spawn` or another action.

> For parallel coding, choose `isolation:"worktree"` so children cannot edit the host checkout. Shared mode is for read-only review or explicitly coordinated work. Worktrees prevent accidents; they are not a security sandbox.

```text
/subagents                                      # open the dashboard
/subagents new <task>                           # spawn an agent (shared mode)
/subagents new --agent reviewer <task>          # use a named markdown profile
/subagents new --reasoning high <task>          # set child reasoning
/subagents logs <id>                            # open one agent's transcript
/subagents send <id> <text>                     # send a follow-up turn
/subagents resume <id> <text>                   # explicitly continue a child session
/subagents kill <id>                            # stop a live child
```

- **Dashboard:** bounded child list by latest state change; rows show profile/model, `completed` instead of internal success, relative update time, short ID. `up`/`down`, `enter` open; `esc` closes. With resident children, composer `down` opens it without clearing drafts.
- **Child transcript:** main renderer combines finalized journals with immutable live projection. PgUp loads older bounded pages; Up/Down preserves scrollback; PgDn follows output. `enter` sends follow-ups, retaining drafts until durable acceptance; `esc` closes.
- **Persistence:** managed `transcript.jsonl` stores acceptance/finalized records; `metadata.json` is rebuildable; `result.json` holds up to 256 KiB final visible assistant text; worktrees may add `patch.diff`. Completion updates carry bounded text; child views show full history. Shutdown stops children; relaunch marks queued/running work `interrupted`, never replays. Resume with an explicit prompt.
- **Scheduling:** fair global default: six concurrent turns, one per child (`subagents.max_concurrent` overrides). No queue deadline unless positive `subagents.queue_timeout` durably fails overdue prompts. Required work stays unresolved until an explicit successful follow-up.
- **Limits:** no cumulative token budget/step cap; inherited `--max-steps` defaults to unlimited. Turns have model-context bounds and one compaction recovery. Last-turn prompts reaching 85%, 90%, 95% context get one non-blocking finish-up reminder per band in the next request.
- **Control:** `subagent` action `status` with `agent_id`, `include_result: true` retrieves terminal results without model calls. Check progress/side effects before resume; its `wait` blocks on the answering turn. Resume steers running turns unless `mode: "queue"` schedules next. `interrupt` cancels only the turn, retaining context for wrap-up. See [lifecycle and persistence](docs/subagents.md#lifecycle-and-persistence).
- **Workspace/results:** worktree mode captures a patch and changed-file list without applying it; shared mode uses the host checkout. References: `subagent://<id>`, `subagent://<id>/history`, `subagent://<id>/result`, `subagent://<id>/patch`.

**Proactive delegation:** `/settings` → **proactive delegation** or `/orchestrator` makes the interactive primary delegate implementation, debugging, testing, review, and broad research by default. Answers, quick lookups, few-line edits, and integration stay local.

The starting reply names parts, owners, and delegated profiles. The primary maps dependencies, parallelizes independent work, and uses feature-branch sub-worktrees for multi-part features. Review gates merges; merged worktrees/branches are removed. Blocking work uses `required: true`; the primary yields for host completion, never duplicating active workers' investigation, review, tests, or implementation.

Host-driven asynchronous completion/required-work outcomes persist through `[auto-subagents update]` and `[required-subagents update]`. Never poll status, dashboards, metadata, journals, event logs, or files solely to wait.

Even when off, permitted canonical `subagent` tools/profile metadata support user delegation and active skill workflows: named profiles, reasoning, per-spawn fast overrides, shared/worktree isolation, persisted lifecycle/results. See [docs/subagents.md](docs/subagents.md).

### Named subagent profiles

Discover markdown/frontmatter profiles in `~/.agents/agents/*.md` and `ZUT_AGENT_PROFILES` paths. Available `subagent` action `spawn` supplies `[subagents_list]`: choose a matching `agent` profile, or a clearly described general worker without `agent`. Bodies apply only to the selected child.

Without spawn permission, metadata is omitted; the proactive interactive primary works locally. Explicit headless `--orchestrate` remains manager-only and reports unavailable delegation instead of implementing. See [docs/subagents.md](docs/subagents.md) for discovery, fields, reasoning, and examples.

### `/settings`

`up`/`down` select; `enter` or `space` changes; `esc` closes (or backs out of a sub-view first). Settings save to `$ZUT_HOME/config.json` without restart. Timing differs by setting:

- **web search:** on by default for normal CLI DuckDuckGo search and `web_open` exploration of in-memory references or public URLs. `web_search_enabled: false` removes `web_search`, `web_open`, `web_find`, and `web_click` from the current interactive session and future normal CLI runs unless invocation tool overrides apply. Explicit `--tools web_search`, alone or in CSV, wins for that session and makes the row read-only with an explanation; rerun without it to change the default. Bot, default SDK, and packaged-agent runs never gain access from this setting. See [docs/web-search.md](docs/web-search.md).
- **render images when supported:** inline screenshots and `read` images via terminal image protocol, otherwise text placeholders. `TERM_PROGRAM` detection can be overridden; terminals with no image protocol force the greyed-out row off.
- **terminal alerts:** on by default, immediate; `terminal_alerts_enabled`. Bell on input needs (tool confirmation, question, login, blocked/paused goal), main-session stop, or structured extension alert. Terminal may make it audible, visual, or silent.
- **AI terminal titles:** on by default; `terminal_title_enabled`. One small hidden request to the active model after a fresh interactive session's first real prompt sets `zut: <title>` (max 40 Unicode characters). Saved with the session, restored on resume, never conversation context. Toggle applies immediately; startup context, resumed history, and slash commands never trigger generation. Disable to avoid the extra request.
- **proactive delegation:** off by default; `/orchestrator` is the shortcut. See `/subagents` and Named subagent profiles above for delegation, permissions, host-driven completion, no-polling rules, and headless `--orchestrate` behavior.
- **fast mode:** off by default; `/fast` is the shortcut, effective next model call. Uses OpenAI Fast service tier for OpenAI, OpenAI Responses, and OpenAI Codex; other providers return unsupported-provider errors. May cost more; availability depends on model/account. Saves to the active profile, restored on profile switch; otherwise `fast_mode`.
- **lsp in main session:** on by default; `lsp_enabled` enables built-in `lsp` and code diagnostics.
- **lsp in sub-agents:** on by default; `subagent_lsp_enabled` applies to newly started children.
- **auto-compact threshold:** `off`, `70%`, `80%`, `85%` (default), `90%`, or `95%` of advertised model context; `auto_compact_threshold`. Controls before/after interactive-turn compaction. Accounting includes uncached input plus provider cache reads/writes, so cached turns still qualify. See `/compact` for recovery, handoff, and cancellation rules; `off` disables only percentage triggers.
- **jail new sessions by default:** off by default; `jail_by_default` confines new interactive, print, JSON, RPC, and background agents to their cwd and updates the current interactive session immediately. `/jail` and `/unjail` override only that session, not the default.
- **compact transcript rendering:** off by default; immediate; `compact_mode`. Tool calls use quiet headers and indented output rather than boxes; sent messages omit padded background bubbles.
- **show loaded resources at startup:** off by default; immediate; `show_instructions_at_startup`. Compact sections above the transcript list the active Zutfile agent (`zut run`), loaded `AGENTS.md` paths, extensions, and user-installed skills, excluding built-ins.
- **TUI settings:** sub-view ordered status, working spinner, input, subagent operations. **Input style:** `plain` (default prompt line), `lines` (separators above/below), or `block` (user-bubble style). **Status position:** model/usage/cwd above or below input. **Working spinner position:** busy spinner above or below input. Immediate; keys `tui_input_style`, `tui_status_position`, `tui_working_position`.
- **reasoning level:** off, minimum, low, medium, high, xhigh, max; saved in `config.json`, effective next call. Selector shows only distinct supported levels; non-reasoning models offer only `off`. Opt-in `max` is native on GPT-5.6 and adaptive-thinking Claude; other backends clamp to highest accepted effort. `/reasoning` opens it; `Ctrl+R` cycles supported levels.
- **color theme:** `auto` follows terminal foreground/background, ANSI palette, color depth, and supported live appearance changes. Fixed dark/light palettes and JSON overlays from `$ZUT_HOME/themes` or loaded extensions are available. Active custom files safely reload; invalid edits retain the last valid appearance, persistent deletion resets to auto. See [docs/themes.md](docs/themes.md).
- **model profiles:** nine shared provider/model/reasoning/fast-mode favorites. `enter` opens a slot's `/model` picker; `left`/`right` selects reasoning without changing the live model; `enter` saves; `backspace` clears. Active-slot edits apply/save model and reasoning immediately; `/fast` applies/saves its fast preference. Other-slot assignments leave the live model unchanged, even during a turn. Clearing the active slot detaches it without changing the live model. See [Model profiles](#model-profiles).

Dictation (`Alt+V`) has no settings row: configure `dictation` in `config.json` with `recorder`, `max_seconds`, and `language`. See [Dictation](#dictation).

### Model profiles

Use `Ctrl+1` ... `Ctrl+9` in the editor, or `/profile 1` ... `/profile 9`:
- Empty slots open `/model`: pick a model, adjust reasoning with `left`/`right`, then `enter` saves/activates using current fast mode. `esc` cancels unchanged. A slot concurrently cleared elsewhere also opens the picker.
- Assigned slots restore model, clamped reasoning, and fast mode, keeping transcript and accumulated cost across providers.
- Active-slot `/model`, `/reasoning`, `Ctrl+R`, and `/fast` changes save back automatically. Pick another slot first to preserve a favorite. Without an active slot, reasoning/fast changes save as plain `reasoning`/`fast_mode`.
- Status shows `[1]` ... `[9]`. Switching is forbidden during a turn; `/settings` assignment/clearing is allowed.

Daemon sessions share nine slots; edits appear immediately everywhere, but active slots are per-session. Attachment requires matching live model/reasoning/fast mode. Startup mismatches (resume/CLI options), rescue model swaps, and credential reloads detach without overwriting favorites.

If switching succeeds but saving fails, **model switched; profile not saved** means the live model stays and the slot detaches. Fix saving, then reselect.

Merge these fields into `$ZUT_HOME/config.json` while zut is closed. `quick_model_shortcuts` positions are 1–9; `active_model_profile` stores the default slot:

```json
{
  "active_model_profile": 1,
  "quick_model_shortcuts": [
    {"provider": "openai-codex", "model": "gpt-6-astra", "reasoning": "medium", "fast_mode": true},
    {"provider": "openai", "model": "gpt-5.6-sol", "reasoning": "max", "fast_mode": false}
  ]
}
```

- `{}` is an empty position. Hand edits affect later-created sessions.
- Missing, `0`, or out-of-range `active_model_profile` means no active slot; edits save as plain settings until selection. Activating sets the new-session default; clearing that slot resets it to `0`.
- Legacy model-only slots load: missing/empty `reasoning` and missing `fast_mode` mean off.
- Startup uses profile defaults, normalizes provider aliases, and clamps reasoning. Unavailable provider/model repairs the profile to the resolved fallback, preserving fast mode. CLI options and session restoration retain precedence; fast mode still requires provider support.
- Favorites are separate from named Zutfile and subagent profiles.

**AZERTY/terminals:** Ctrl with the French top row (`&`, `é`, `"`, `'`, `(`, `-`, `è`, `_`, `ç`) selects 1–9 without Shift; modified digits work too. Zut requests Kitty/CSI-u or xterm modifyOtherKeys reporting. If a terminal/multiplexer reserves the chord or sends ambiguous legacy control bytes, configure enhanced forwarding or use `/profile N`. Bare punctuation and ambiguous bytes are never reinterpreted. macOS Command also works when the terminal forwards Super.

### `/skills`

Lists discovered SKILL.md names, sources, descriptions; built-ins hidden. `enter` previews; `up`/`down`/`pgup`/`pgdn` scroll; `esc` returns. Reopening rediscovers edits. User discovery follows directory symlinks, skips cycles/dangling links, preserves nested aliases. Extension skills stay within their directory.

`p`/`g` toggle project/global pins. Bodies join fresh supported conversations' first user prompt; a read-only pending-selection notice leaves the editor unchanged. Resume never reloads pins. Pins save invocation, not tokens. See [pinning skills](docs/skills.md#pinning-skills) for storage, lifecycle, modes.

### `/compact`

A structured model summary replaces the transcript with one synthetic user message, preserving recent exchanges verbatim and resetting context. Use around 80%. Manual `/compact` never silently starts work.

Automatic compaction runs in the background at the `/settings` threshold (`off`, `70%`, `80%`, `85%` default, `90%`, `95%`). `condensing history, esc to cancel` appears above the status bar, with `(auto)` beside context percentage. `esc` aborts without changing the transcript. `off` keeps manual compaction and automatic compaction/retry for context-window or payload-too-large responses.

Structural unfinished tails and model-token-limit output continue as before; truncated-output continuation is a separate priority rule. After successful after-turn threshold compaction:
- Visible assistant text promising future work may get at most two hidden, bounded recovery continuations. Completed text settles; newer queued user input wins.
- Interrupted sessions retain outstanding hidden handoffs on resume, including portable export/import, without showing them as chat messages.
- Only complete-effective-transcript branches retain handoffs. Earlier-turn `/fork` or `/session tree` selections and all pre-compaction-history branches drop them and start no hidden continuation.

Requests/compaction retain at most 32 KiB per tool result, 128 KiB total tool text, newest preferred. Omissions are marked for the model; full output stays visible and session-stored.

### `/jail`

Roots an accident-prevention sandbox at the status-bar cwd:
- `read`, `write`, `edit`, `grep`: resolve paths, including symlinks, and reject outside targets.
- `worktree`: repository root, Git metadata, and resolved destination must all stay inside.
- `bash`: rejects obvious escapes (`sudo`, `rm -rf /`, leading `cd /`, `cd ..`, `cd ~`, `chmod -R`, `dd of=/`, and similar), plus outside-pointing shell arguments/redirections.

Status shows `jailed, ~/your/cwd`. Persist across launches with `/settings` → **jail new sessions by default**; `/unjail` unlocks only the current session.

This is not a hard security boundary. For real isolation, run zut in docker or a proper sandbox.

## Sessions

Interactive and print/json runs save JSONL transcripts in `$ZUT_HOME/sessions/<cwd-hash>/` unless `--no-session`. Resume with `--continue`, `--resume`, `--session <path>`, or TUI `/sessions`. Resume restores the recorded provider/model, rebuilding the agent if needed; failed rebuilds leave the current session intact. Empty sessions are deleted on close.

Usage checkpoints may store sanitized `retry_lifecycle` attempts, backoffs, terminal state, and allowlisted failure category, never raw provider errors or response bodies. Provider-context reconstruction ignores this metadata.

Outside the TUI, `zut sessions prune` groups sessions with missing working directories, showing counts and sizes for selection and confirmed permanent deletion. It rechecks selected directories before deletion; errors other than "not found" preserve sessions.

Unmounted paths can look deleted, and other zut processes' open sessions cannot be detected. Mount remote filesystems and ensure no live session uses a selected group before deleting.

`--older-than` prunes by last activity, including existing directories; `--cwd PATH` limits it to one directory. Units: `m` minutes, `h` hours, `d` 24-hour days, `w` 7-day weeks, `mo` calendar months, `y` calendar years. Files are rechecked before deletion and preserved if recently active.

```sh
zut sessions prune --dry-run                         # list missing-directory groups
zut sessions prune --older-than 4h --dry-run         # inspect four-hour inactivity
zut sessions prune --older-than 1mo --cwd ~/project  # select old sessions in one directory
zut sessions prune --older-than 1y --all             # select all matches, then confirm
zut sessions prune --older-than 30d --all --yes      # delete without prompting
```

`--all` selects every match but still confirms; `--yes` requires `--all` and skips prompts. Malformed, unreadable, and symlinked entries are reported and preserved. Scans cover `$ZUT_HOME/sessions/`, including named-agent namespaces, not explicit session files elsewhere.

## Daemon and sessions

A local daemon owns sessions. `zut`, `zut -p`, `--stream`, `--json`, `zut rpc`, and Telegram create or attach as clients. The first client auto-starts detached `zut daemon run`, printing `Starting zut daemon…` then `zut daemon ready (1.2s)` on stderr. An existing daemon produces no startup message.

```sh
zut ls [--json]                    # open daemon sessions
zut tree [--json]                  # sessions and subagents
zut attach [id|title-prefix]       # TUI; no argument: latest detached session in this directory
zut kill <id>...                   # exact IDs or unique ID prefixes
zut daemon status                 # pid, version, uptime, session count
zut daemon start|stop|restart      # starts empty
zut daemon logs [-f]               # print/follow log
zut daemon run [--idle-gc-hours N] # foreground; close detached idle sessions after N hours (default 24; 0 disables)
```

Every daemon starts empty, including after crashes or auto-spawn. Old transcripts remain resumable, not detached live sessions. Startup best-effort deletes old `daemon/state.json` (possibly containing client environment secrets) and stale `daemon/sessions/` review journals; saved schedules remain.

Agents can see and message other sessions with the `sessions` tool ([Tools](#tools)).

TUI `/detach` leaves the session running and prints its `zut attach` command; `/exit` closes it. In daemon mode, idle or busy `ctrl+c` opens `Q quit · D detach · Esc cancel` after dialog handling or idle-input clearing:

- Q closes like `/exit`; failure keeps it open. After cleanup and terminal restoration, surviving transcripts get a `zut --resume <daemon-id>` hint. Resume creates a new live session; older transcripts still resume by UUID. No hint for `--no-session` or deleted empty transcripts.
- D acts like `/detach`.
- Esc or any other key cancels without interrupting the turn.
- A second `ctrl+c` forces exit after a bounded close attempt, reporting close errors as an exit notice.

Headless runs close on exit unless `--keep-session`, which leaves them in `zut ls` and reattachable.

Linux `zut daemon install` writes `~/.config/systemd/user/zut-daemon.service` with `Restart=on-failure` and prints `systemctl --user` commands; `--now` runs them. `zut daemon uninstall [--now]` reverses this.

Paths and logging:

- Socket: `$XDG_RUNTIME_DIR/zut/daemon.sock`, fallback `$ZUT_HOME/run/daemon.sock`; Windows: `%LOCALAPPDATA%\zut\run\daemon.sock`. Override with `ZUT_DAEMON_SOCKET`.
- Lock, pid, schedules: `$ZUT_HOME/daemon/`.
- Daemon log: `$ZUT_HOME/logs/daemon.log`, `0600`, rotates at 10 MiB to one backup, `daemon.log.1`. Oversized logs rotate on startup; later rotation precedes a write exceeding the limit. Records are not split.
- Interactive startup timings (session created, attached, first frame): `$ZUT_HOME/logs/client.log`, `0600`. Concurrent clients may append; rotation to `client.log.1` occurs only when opened over 10 MiB.
- `ZUT_DAEMON_LOG=debug|info|warn|error` sets each process's level from its environment; default/unknown values use `info`. Restart the daemon to change its level. Default daemon logs include `daemon ready total=…` and `session ready total=…`; `debug` adds startup/session phase timings (config, MCP servers, extensions, skills, session file) and shared MCP server stderr.

In the daemon environment, `ZUT_DAEMON_PPROF=1` or `true` enables pprof at `127.0.0.1:0` (OS-selected port); empty/unset disables it. Other values specify loopback `host:port`, including port `0`. Only loopback IPs and `localhost` (pinned to `127.0.0.1`) are accepted.

The bound address is logged and written to `$ZUT_HOME/daemon.pprof`, removed on shutdown. Listen failures are non-fatal; profiling stops with the daemon. Restart to apply changes. Endpoints expose process data to local users.

```sh
ZUT_DAEMON_PPROF=1 zut daemon
go tool pprof http://$(cat ~/.local/state/zut/daemon.pprof)/debug/pprof/heap
# fish:
go tool pprof http://(cat ~/.local/state/zut/daemon.pprof)/debug/pprof/heap
```

Examples use Linux defaults. Prefer `ZUT_HOME` if set, otherwise `$XDG_STATE_HOME/zut` if set. macOS defaults to `~/Library/Application Support/zut`; Windows to `%LOCALAPPDATA%\zut`.

Limitations:

- Bot services do not restart with the daemon. Run `zut telegram-bot start` again for a new session and polling.
- Windows autospawn is untested.
- Linux, macOS, FreeBSD, NetBSD, OpenBSD: socket peers must match the daemon UID, in addition to permissions (`0600` socket in your private directory).
- Windows: socket directory ownership must be the current user (Administrators when elevated), with a protected DACL granting access only to that user. zut applies it only to directories it creates or its default socket directory; it refuses non-private custom `ZUT_DAEMON_SOCKET` directories. There is no per-connection UID check.
- DragonFly BSD, Solaris, illumos, AIX, Plan 9, js, wasip1: daemon startup fails closed; daemon-backed modes are unavailable.
- These checks do not isolate same-user processes or privileged administrators.

## Providers

Built-in providers:

- **Subscription-capable**: Anthropic Claude Pro/Max (`anthropic`), OpenAI Codex / ChatGPT Plus/Pro (`openai-codex`), Kimi Code (`kimi`), SuperGrok/X Premium (`xai`), GitHub Copilot (`github-copilot`).
- **Direct API**: Anthropic, OpenAI Chat Completions, OpenAI Responses, DeepSeek, Google Gemini, Kimi/Moonshot, Moonshot CN, Groq, Cerebras, xAI, Together AI, Hugging Face Router, OpenRouter, Mistral, Z.AI, Xiaomi/MiMo token-plan regions, MiniMax global/CN, Fireworks, Vercel AI Gateway, OpenCode/OpenCode Go.
- **Cloud/platform**: Amazon Bedrock, Google Vertex AI, Azure OpenAI, Cloudflare Workers AI, Cloudflare AI Gateway.
- **Local/compatible**: Ollama, llama.cpp router, OpenAI-compatible local endpoints via `--base-url`.

`/login` stores keys or subscription credentials. `/model` lists only providers available through env vars, `auth.json`, Kimi CLI fallback, local Ollama, or a configured llama.cpp router.

## Models

`--list-models` and `/model` draw from three sources across built-in providers (the picker filters by availability):

- **Catalog**: baked-in Claude, GPT/Codex, Gemini/Gemma, Kimi/Moonshot, DeepSeek, Groq Llama/Gemma/Compound, OpenRouter, Bedrock/Vertex IDs, Azure OpenAI deployments, Copilot, and other provider entries.
- **Live**: provider model endpoints queried with stored keys; cached 24h in `$ZUT_HOME/models-cache.json`, refreshed in the background at startup. OpenCode Go joins `https://opencode.ai/zen/go/v1/models` with limits, prices, names, reasoning, and wire-adapter metadata from `https://models.dev/api.json` (the linked provider page's JSON source). Its cache is credential-fingerprint/endpoint scoped; SDK runtimes load matching metadata before resolving. Catalog listing proves neither consent nor regional eligibility, quota, or account access.
- **Speculative**: upstream-generator IDs not yet public. They return 404 until released.

The status-line context meter compares last-turn usage with the advertised context window. Historical tool-result text sent to providers is bounded, preserving call/result structure. Displayed and saved tool output remains complete.

### Model fallback (rescue)

Recoverable errors open a **rescue** model picker over chat: expired token (`401`), permission denied (`403`), rate limit (`429`), outage (`502`/`503`/`504`), or transient network failure.

Rescue uses `/model`'s vertical fuzzy list, excluding the failed model and including only providers available through env vars, `auth.json`, Kimi CLI fallback, or ollama. `↑`/`↓` selects; `enter` retries the **same prompt**; `esc` dismisses. `tab` shows the full, pane-wrapped error; scroll with arrows or `PageUp`/`PageDown`, then `tab`/`esc` returns.

Shared OpenAI / Anthropic / Kimi / DeepSeek / Google / OpenAI-Codex HTTP streaming clients silently retry transient HTTP failures and connection-reset / EOF-before-headers errors up to four times (250ms, 750ms, 750ms, 750ms). Open streams get up to five reconnect attempts.

Separately, five minutes of stream silence triggers one immediate retry (`idle timeout (1/1)`); a second silent stream fails. Embedders configure `core.Agent.MaxIdleRetries` (`0` disables), independently of `MaxRetries`. Cancellation and context deadlines are never retried. Follow-ups queued during a stall go with the retry.

Rescue **drops launch-time `--api-key` and `--base-url`**, rebuilding with env / `auth.json` / provider defaults to avoid bad or provider-specific overrides. Use `/model` to retain overrides.

Candidates come from active credentials; no configuration needed. Bad-request, context-length, and serialization errors remain normal errors, not rescue candidates.

### Custom models

Use `$ZUT_HOME/models.json` (`$XDG_STATE_HOME/zut/` if set, otherwise the platform default above) to add or override catalog entries:

```json
{
  "providers": {
    "openai": {
      "models": [{
        "id": "gpt-5.5",
        "name": "GPT-5.5",
        "reasoning": true,
        "reasoningLevelMap": {"minimum": "low", "max": ""},
        "contextWindow": 400000,
        "maxTokens": 128000
      }]
    }
  }
}
```

Model fields: `id` (required), `name`, `reasoning`, `reasoningLevelMap`, `contextWindow`, `maxTokens`, `baseUrl`, `priceInput`, `priceOutput`, `priceCacheRead`, `priceCacheWrite`.

Optional `reasoningLevelMap` overrides protocol defaults. Keys: `minimum`, `low`, `medium`, `high`, `xhigh`, `max`. Map equivalent levels to each other; identity mappings such as `"max": "max"` enable levels beyond protocol defaults; `""` or `off` removes a level. `/reasoning` and requests use the same effective map.

Provider normalization:

- `openai-codex`, `openai-responses` → `openai`
- `anthropic-messages` → `anthropic`
- `moonshot`, `moonshot-ai`, `kimi-code` → `kimi`
- `deepseek-chat`, `deepseek-ai` → `deepseek`

Built-in IDs also work directly: e.g. `groq`, `openrouter`, `github-copilot`, `amazon-bedrock`, `google-vertex`, `azure-openai-responses`, `fireworks`, `vercel-ai-gateway`, `mistral`, `xai`.

User entries show `source: user` in `--list-models` and override baked-in/live metadata without hiding other entries. Missing/invalid files are silently ignored.

#### Custom providers

Unknown provider IDs define custom providers. Set provider `baseUrl` and `api`: `openai` (default, Chat Completions), `openai-responses` (Responses), or `anthropic` (Messages). Unknown `api` warns and falls back to `openai`. Model `baseUrl` overrides provider `baseUrl`.

```json
{
  "providers": {
    "my-company": {
      "baseUrl": "https://llm.mycompany.com/v1",
      "api": "openai",
      "models": [{"id": "company-llm-v2", "name": "Company LLM v2"}]
    }
  }
}
```

Custom providers appear in `--list-models`, `/model`, and `/login`. `models.json` holds no secrets. Supply keys through `/login`, `--api-key`, or upper-snake-case env vars (`my-company` → `MY_COMPANY_API_KEY`). Configured URLs also work keyless. Custom keys are saved without probing; invalid keys fail on first call.

Provider `"auth": "none"` ignores inherited, CLI, env, and stored credentials for inference/discovery and never executes command-backed keys. Use only for trusted keyless endpoints.

Provider `"discover": true` opts into OpenAI-style `/models` discovery and requires provider `baseUrl`. Discovered defaults: 32K context, 4K output; explicit entries override metadata. Failed refreshes retain the in-memory list; background discovery never executes command-backed credentials. See [custom model discovery](docs/providers.md#custom-model-discovery) for schema and limits.

For password-manager keys, add a matching `$ZUT_HOME/auth.json` entry:

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

Provider IDs must match in `models.json` and `auth.json`. Select directly:

```bash
zut --provider my-company --model company-llm-v2
```

### Kimi Code

Kimi supports the Kimi Coding endpoint and Moonshot OpenAI-compatible chat API.

```bash
zut --provider kimi
```

Defaults: model `kimi-for-coding`, URL `https://api.kimi.com/coding/v1`.

Credential order: `--api-key` → `KIMI_API_KEY` → `MOONSHOT_API_KEY` → `$ZUT_HOME/auth.json` → official CLI token `~/.kimi/credentials/kimi-code.json` (unless `/logout kimi` disabled it).

`/login` supports keys or Kimi Code subscription device-code OAuth: opens verification URL, waits for browser approval, saves to `auth.json`, and auto-refreshes.

For direct Moonshot keys or compatible endpoints:

```bash
zut --provider kimi --model kimi-k2-0905-preview --base-url https://api.moonshot.ai/v1 --api-key "$KIMI_API_KEY"
```

Kimi K3 routes: `kimi/k3`, `moonshotai/kimi-k3`, `moonshotai-cn/kimi-k3`, `opencode-go/kimi-k3`, `openrouter/moonshotai/kimi-k3`, `vercel-ai-gateway/moonshotai/kimi-k3`. All have a 131,072-token output limit. Add other Kimi/Moonshot IDs under `kimi` in `models.json`.

### xAI

Use `XAI_API_KEY` or `/login` subscription option `Sign in with SuperGrok or X Premium`. It opens a prefilled device-authorization URL and auto-refreshes the stored token. Default: `grok-4.5` via Responses API.

### DeepSeek

DeepSeek uses its OpenAI-compatible chat API.

```bash
zut --provider deepseek
```

Defaults: `deepseek-v4-pro`, `https://api.deepseek.com/v1`. Catalog: `deepseek-v4-pro` (reasoning), `deepseek-v4-flash`, matching current `GET https://api.deepseek.com/models` IDs. Add others under `deepseek` in `models.json`.

Credential order: `--api-key` → `DEEPSEEK_API_KEY` → `$ZUT_HOME/auth.json`.

`/login → api key` probes `/v1/models` once, then stores the key under `deepseek` in `auth.json`.

> **API key only:** no DeepSeek subscription OAuth. `/login subscription` lists only Anthropic, OpenAI, and Kimi; DeepSeek appears only in `/login → api key`.

> **Text-only wire format:** DeepSeek rejects multimodal content (`unknown variant image_url, expected text`). zut silently drops outgoing user/tool `ImageBlock` parts, retaining text and stored images. Switching to vision-capable Claude, GPT-4o/5, or Gemini sends images again.

For mirrors, gateways, or self-hosted endpoints:

```bash
zut --provider deepseek --base-url https://my-deepseek-mirror.example.com/v1 --api-key "$DEEPSEEK_API_KEY"
```

### Google Gemini

Google Gemini uses the [AI Studio Generative Language API](https://aistudio.google.com/).

```bash
zut --provider google
```

Defaults: `gemini-2.5-pro`, `https://generativelanguage.googleapis.com`. Catalog also includes `gemini-2.5-flash`, `gemini-2.5-flash-lite`, `gemini-2.0-flash`, `gemini-2.0-flash-lite`. `/v1beta/models` discovery adds IDs visible to your key.

Credential order: `--api-key` → `GEMINI_API_KEY` → `GOOGLE_API_KEY` → `$ZUT_HOME/auth.json`.

`/login → api key` probes `/v1beta/models` once, then stores the AI Studio key under `google` in `auth.json`.

> **API key only:** Gemini Advanced / Google One AI Premium consumer subscriptions provide no OAuth tokens. Selecting Google under `/login subscription` falls back to the key form. For OAuth/service-account auth, use `google-vertex`.

> **Free-tier limits:** AI Studio caps vary by model, per minute/day. Pro is strictest (a few requests/minute, ~50/day); Flash/Flash-Lite allow more. Pro returning 429 `"You exceeded your current quota"` while Flash works means Pro free-tier RPD exhaustion. Use Flash for agent loops or [enable billing](https://aistudio.google.com/app/apikey) on the project; the same key becomes pay-as-you-go (Pro: `$1.25/M` input, `$10/M` output).

Reasoning: `--reasoning off|minimum|low|medium|high|xhigh|max`, `/reasoning`, or `/settings` **reasoning level**. `max` is opt-in above `xhigh`: native on GPT-5.6, GPT-6, and adaptive-thinking Claude; unsupported providers clamp to their highest effort. Budget-based providers retain model/provider caps.

Gemini 3.x uses `thinkingLevel` (`MINIMAL`/`LOW`/`MEDIUM`/`HIGH`). Gemini-3-Pro has a `LOW` minimum and uses `HIGH` for medium-or-higher requests. `off` omits reasoning config except for always-on thinking models; Gemini 2.0 has no thinking config.

Claude Opus 5.5:

- Anthropic: `--provider anthropic --model claude-opus-5-5`, 500,000 context tokens.
- Copilot: `--provider github-copilot --model claude-opus-5.5`, 1,000,000 context tokens, subject to account access.
- Output: up to 128,000 tokens. Always-on adaptive thinking: `low`, `medium`, `high`, `xhigh`, `max`; default/`off`/`minimum` use `low`. Selector hides unsupported levels.
- Catalog prices per million: $4 input, $20 output, $0.20 cache reads, $5 cache writes. Copilot billing follows your plan.

GPT-6 Sol/Luna: select `gpt-6-sol` or `gpt-6-luna` with `--provider openai`, `openai-responses`, `openai-codex`, or `github-copilot` (API keys or Codex/Copilot subscriptions). Both use Responses API, up to 128,000 output tokens, and `low` through `max` reasoning. Context: OpenAI 272,000; Codex 500,000 working budget; Copilot 1,000,000. Subscription access depends on account availability and organization policy.

Add Gemini IDs under `google` in `models.json`.

### Gemini Enterprise Agent Platform (formerly Google Vertex AI)

[Gemini Enterprise Agent Platform](https://cloud.google.com/products/gemini-enterprise-agent-platform) is Google's enterprise/GCP Gemini endpoint. `google-vertex` supports Cloud API keys and `service_account` / `authorized_user` files, not consumer Google login or full ADC (metadata-server/workload-identity credentials).

```bash
zut --provider google-vertex
```

Environment, read at construction:

- `GOOGLE_CLOUD_PROJECT`: required GCP project ID.
- `GOOGLE_CLOUD_LOCATION`: default `us-central1`.

Credential order:

1. `GOOGLE_CLOUD_API_KEY`: GCP console key sent as `x-goog-api-key`, no token exchange.
2. `GOOGLE_APPLICATION_CREDENTIALS`: supported JSON file path.
3. Default ADC file from `gcloud auth application-default login`: Unix `~/.config/gcloud/application_default_credentials.json`; Windows `%APPDATA%\gcloud\application_default_credentials.json`.

Supported file types:

- `type: "service_account"`: signs a private-key JWT and exchanges it for a short-lived token.
- `type: "authorized_user"`: exchanges client ID, client secret, and refresh token for a short-lived token.

Tokens are cached in memory and refreshed on demand. Without credentials: `vertex: no auth — set GOOGLE_CLOUD_API_KEY or GOOGLE_APPLICATION_CREDENTIALS`.

### Local models with ollama

[ollama](https://ollama.com) serves a local OpenAI-compatible API; any pulled model works.

```bash
ollama pull qwen3.5:4b
zut --provider ollama --model qwen3.5:4b
```

Local defaults: `http://localhost:11434`, no API key.

### Ollama Cloud with OLLAMA_API_KEY

`OLLAMA_API_KEY` from [ollama.com/settings/keys](https://ollama.com/settings/keys) selects Ollama Cloud without `--base-url`:

```bash
export OLLAMA_API_KEY=...
zut --provider ollama --model gemma4:31b
```

Endpoint precedence:

1. `--base-url` wins; `--base-url http://localhost:11434` forces local even with a key.
2. Model/provider `baseUrl` in `models.json`.
3. A real key (`--api-key`, `OLLAMA_API_KEY`, `/login`) selects `https://ollama.com/v1`.
4. No key: local, no auth.

`/login → api key → Ollama` saves to `auth.json`. `--api-key` overrides env/stored keys.

Remote/authenticated example:

```bash
zut --provider ollama --model llama3 --base-url https://my-server.com/v1 --api-key my-token
```

Save model settings to avoid repeated flags:

```json
{
  "providers": {
    "ollama": {
      "models": [{
        "id": "qwen3.5:4b",
        "name": "Qwen 3.5 4B",
        "contextWindow": 32768,
        "maxTokens": 8192
      }]
    }
  }
}
```

`ollama` uses OpenAI Chat Completions and also works with vLLM, LM Studio, LocalAI, and other compatible servers.

### Local models with llama.cpp router mode

zut manages GGUF files and loaded-model inference through a recent [llama.cpp](https://github.com/ggml-org/llama.cpp) router's OpenAI-compatible API. This is separate from Ollama (normally port `11434`; use provider `ollama`).

Install/update llama.cpp, e.g. macOS Homebrew:

```bash
brew install llama.cpp
# or, when already installed
brew upgrade llama.cpp
```

Create a GGUF directory; start `llama-server` without `--model`, `-m`, or `-hf` (those select single-model mode, not the required router API).

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

`--no-models-autoload` delegates loading to `/llama`; `--jinja` enables chat templates for better tool calls. `-ngl 999` requests maximum GPU offload; `-c 32768` caps each loaded model at 32K context. Adjust for your hardware.

Check router mode before configuring zut:

```bash
curl http://127.0.0.1:8080/health
curl http://127.0.0.1:8080/models
```

`/models` must return JSON with a `data` array. A 404 usually means an outdated server, wrong port, or single-model mode.

In `/login → api key → llama.cpp`, enter router URL `http://127.0.0.1:8080`; leave the key empty for local-only use. Omit `/v1`; zut derives inference URLs. URL/key are saved in `$ZUT_HOME/auth.json`.

Or use environment variables:

```bash
export LLAMA_BASE_URL=http://127.0.0.1:8080
export LLAMA_API_KEY=optional-secret
```

For auth, pass the matching server `--api-key`. Keep `--host 127.0.0.1` unless remote access is needed.

`/llama` lets you:

- inspect router model states;
- search Hugging Face GGUF repositories;
- choose quantization and download with byte progress;
- explicitly load/unload models;
- press `d` to request confirmed deletion of downloaded cache models.

zut cannot remove `--models-dir` or preset models; delete them at their source. Router deletion removes the selected cached GGUF, but some llama.cpp versions retain shared artifacts such as `mmproj`; remove the repository cache directory manually if unneeded.

Hugging Face search uses `HF_TOKEN` when set. Gated repositories need prior approval and an authorized `HF_TOKEN` in `llama-server`, which performs downloads.

`/model` refreshes the router and lists only loaded models under `llama.cpp`; unloaded models cannot infer. Load with `/llama`, then select with `/model`.

Ollama's internal model storage is not a llama.cpp GGUF source. Download via `/llama`, or put GGUFs in `~/llama-models` and restart the router to discover manually added files.
## Clipboard images

In main chat, `ctrl+v` tries images before text. `[clipboard image #N]` markers attach on submit; `esc` or `ctrl+c` clears them with the input. Wayland needs `wl-paste` (`wl-clipboard`); X11 uses `xclip` when available. Images over 32 MiB fall back to text paste.

## Dictation

`Alt+V` starts recording; press again to stop and transcribe. `Esc` cancels without uploading. The transcript inserts at the cursor without submitting, with any model/provider.

Below the editor, red `● REC mm:ss` shows the recorder and live microphone levels. Upload shows a spinner, duration, size, and service; insertion briefly shows word count, service, and transcription time.

**Requirements.** Use `/login` OpenAI/ChatGPT OAuth (as for `openai-codex`), not an API key. Errors: `Dictation needs a ChatGPT login (/login)`, `ChatGPT login expired, run /login`, or `No speech detected` for silence.

**Recorder.** First on `PATH`: `pw-record`, `parecord`, then `ffmpeg` (PulseAudio on Linux; AVFoundation `:0` on macOS). Windows requires `dictation.recorder`, e.g. `["ffmpeg", "-loglevel", "error", "-f", "dshow", "-i", "audio=Microphone (device name)", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-y", "{output}"]`. Omit `-nostdin` for graceful stopping.

**Configuration.** Optional keys in `$ZUT_HOME/config.json`:

```json
{
  "dictation": {
    "recorder": ["pw-record", "--rate", "16000", "--channels", "1", "--format", "s16", "--container", "wav", "{output}"],
    "max_seconds": 600,
    "language": "fr"
  }
}
```

- `recorder`: argv, no shell; must contain `{output}` and write WAV.
- `max_seconds`: auto-stop/transcribe timeout; default `600`, range `1`–`1800`. Other values fail config loading.
- `language`: ISO-639-1 hint, e.g. `fr`; omitted means auto-detect.

**Privacy and limitations.** Private temporary WAV (16 kHz mono), deleted after transcription. Uploads go to ChatGPT's private, undocumented `chatgpt.com/backend-api/transcribe`, which may change or stop without notice. OpenAI retains audio (reportedly 30 days).

## Inline images

Tool images (e.g. `read` on PNG) render inline in **Ghostty**, **Kitty**, **iTerm2**, and **WezTerm**. Other terminals show MIME type, dimensions, and byte size. `ZUT_INLINE_IMAGES` controls this:

| Value | Effect |
|---|---|
| unset (default) | Detect via `TERM_PROGRAM`; placeholders in VS Code and Herdr. |
| `iterm`, `iterm2` | Force iTerm2 OSC 1337. |
| `kitty` | Force Kitty graphics. |
| `off`, `none` | Always show placeholders. |

Herdr support is experimental: enable `experimental.kitty_graphics`, then opt in with `ZUT_INLINE_IMAGES=kitty`.

Image frames fully repaint to prevent stale pixels on scroll, costing one terminal flash per frame. Use `ZUT_INLINE_IMAGES=off` to avoid it.

## Tool rendering

Tool calls (bash, read, write, edit, worktree, grep, lsp) default to bordered panels (`┌─ header ─┐`, `│` body, `└─┘` footer). Flat mode uses `▌ bash …` and indented output. Both show tool name, args, streamed output, and `... (N more lines, ctrl+o to expand)` truncation.

In `$ZUT_HOME/config.json`:

```json
{
  "tool_render": "flat"
}
```

| Value | Effect |
|---|---|
| unset / `"box"` (default) | Bordered panels. |
| `"flat"` | Header and indented output, no borders. |

`ZUT_FLAT_TOOLS` overrides config for one run:

| Value | Effect |
|---|---|
| `1`, `true`, `yes`, `on`, `flat` | Force flat. |
| `0`, `false`, `no`, `off`, `box` | Force boxes. |
| unset | Use `tool_render`. |

```sh
ZUT_FLAT_TOOLS=1 zut   # flat, just this run
ZUT_FLAT_TOOLS=0 zut   # boxes, even if config.json says "flat"
```

Both use theme colors (accent/foreground headers, tool-output body); `ctrl+o` expands truncated results.

### Tool arg width

Headers summarize the primary `path`, `command`, or query on one line, truncated at 60 cells (e.g. `web_answer What is the best architecture to implement resilience wit...`). Override with:

```sh
ZUT_TOOL_ARG_WIDTH=120 zut   # up to 120 cells
```

| Value | Effect |
|---|---|
| unset (default) | 60 cells. |
| integer in `[20, 500]` | Use this many cells. |
| anything else | Fall back to 60. |

## Compact input

Sent messages default to background-tinted bubbles with a left `▌` bar and blank padding rows above/below (three rows for one line). `compact_input` removes tint and padding, leaving `▌ your text` per wrapped row:

```json
{
  "compact_input": true
}
```

| Value | Effect |
|---|---|
| unset / `false` (default) | Padded, tinted bubble. |
| `true` | One gutter line per wrapped row. |

`ZUT_COMPACT_INPUT` overrides config: `1`/`true`/`on`/`compact` forces compact; `0`/`false`/`off`/`bubble` forces bubbles.

```sh
ZUT_COMPACT_INPUT=1 zut   # compact, just this run
```

## Queued messages

While the agent works, `enter` steers text and screenshots into its active turn. Pending messages appear above the status bar as `sliding in: <text> [image]` until delivered, in order, before the next model call.

- The running tool finishes; remaining calls in its batch get skipped with error results so the agent can reconsider. Sub-agent reports/host events do not skip tools.
- Submit any number. Unread accepted messages continue in a follow-up turn; compaction delays delivery until it finishes.
- Telegram submissions wait for separate turns and replies.
- `alt+up` recalls the newest queued user message with image markers, skipping host events/reports. Without a known message, normal editor/scroll navigation applies.
- `esc` cancels the turn, recalls that message, and drops stale follow-ups. `ctrl+c` never interrupts a busy turn.
- Daemon recall appends the draft/images on a new line if you edit during retrieval. Only one recall runs; another `esc` requests cancellation after retrieval.
- Daemon `ctrl+c` opens the quit/detach menu above. In-process, it arms the exit hint; repeat within two seconds to exit.

Recall keys: `Alt+↑` (`Option+↑` on macOS); `Alt+Shift+↑` in Vev (pane navigation owns `Alt+↑`); `Option+Shift+↑` in macOS VS Code terminals. The `$TERM_PROGRAM`-aware hint appears only when a user message can be recovered.

Busy slash commands:
- Immediate: `/help`, `/info`, `/jump`, `/btw`, `/sessions`, `/skills`, `/reasoning`, `/settings`, `/jail`, `/unjail`, `/exit`.
- Cancel the turn first: `/clear`, `/compact`, `/login`, `/logout`, `/model`, `/reload-ext`.

## Keys (interactive mode)

### Input

| Key | Action |
|---|---|
| `enter` | Submit (steers the active turn if the agent is busy). |
| `alt+enter` | Newline. |
| `tab` | Complete selected slash command/file/path; otherwise next model profile (empty slot opens picker). |
| `shift+tab` | Previous model profile (empty slot opens picker). |
| `esc` | Busy: cancel turn. Idle: clear input. Two parsed bare `Esc` presses within 500 ms open `/session tree` only with an idle, empty editor. Dialogs, busy/queued work, and modified keys retain precedence. |
| `ctrl+c` | Idle: clear input/queue. Otherwise daemon: Q quit / D detach / Esc cancel menu; Ctrl+C again forces quit. In-process: exit hint; repeat within 2s to exit. Outside the menu, `esc` cancels running turns. |
| `ctrl+d` | Exit on empty input. |
| `ctrl+l` | Redraw the screen. |
| `ctrl+v` | Paste text into focused chat, side chat, dialog, filter, or credential input. Main chat attaches images to the next prompt via macOS pasteboard, Wayland `wl-paste`, or X11 `xclip`. Linux text uses `wl-paste`, `xclip`, or `xsel`; native bracketed paste needs none. |
| `ctrl+o` | Expand/collapse read, write, edit, bash, worktree, grep, glob, lsp, web_search results over ~12 lines. |
| `ctrl+r` | Cycle supported reasoning levels; applies to future calls and persists in `config.json`. |
| `ctrl+1` ... `ctrl+9` | Activate persistent model/reasoning/fast-mode profile; empty slots open picker. French AZERTY Ctrl + top-row symbols work if forwarded. Rejected during turns. See [Model profiles](#model-profiles); fallback: `/profile N`. |
| `alt+v` | Start/stop-and-transcribe dictation; `esc` cancels. Needs ChatGPT login. See [Dictation](#dictation). |
| `@` | Browse working-directory files/directories. |

### File picker (`@`)

| Key | Action |
|---|---|
| `@` | Open picker at input start or after a space. |
| `up`, `down` | Navigate the file list. |
| `right` | Open the selected directory. |
| `left` | Go back to the parent directory. |
| `enter` | Insert selected file/directory chip (`[file:name]` or `[dir:name/]`). |
| `esc` | Close the file picker. |

Filter with `@read` to show entries containing "read". File chips expand to full paths on submit. Dropped files and directories also become chips.

### Editor line navigation

| Key | Action |
|---|---|
| `ctrl+a`, `ctrl+e` | Jump to start or end of line. |
| `alt+left`, `alt+right` | Jump one word back or forward. |
| `ctrl+u`, `ctrl+k` | Delete to start or end of line. |
| `ctrl+w`, `alt+backspace` | Delete the previous word. |
| `up`, `down` | Move in multiline input. Top edge: `up`/`down` browse older/newer prompts. Bottom edge: `down` opens resident-subagent picker if children exist, preserving draft. |

### Chat scroll

| Key | Action |
|---|---|
| `pgup`, `pgdn` | Scroll one page up or down. |
| `up`, `down` (editor empty, not browsing prompt history) | Scroll three lines; `down` opens resident-child picker first if available. Most terminals route the mouse wheel here. |

## Extensions

Opt-in subprocess JSON-RPC extensions work in any language; none are installed by default. They register commands/tools, block or rewrite tool args, gate turns before model calls, and rewrite visible assistant text. Hot-reload with `/reload-ext`.

### Install and manage

```bash
zut ext install <path|git-url>   # copy / clone into $ZUT_HOME/extensions/
zut ext install --build=go <path> # explicitly build/install local Go source
zut ext list                      # installed extensions
zut ext doctor                    # load, registration, conflict diagnostics
zut ext logs <name> [-f]          # print / tail stderr log
zut ext enable <name>             # re-enable
zut ext disable <name>            # keep files
zut ext remove <name>             # delete directory
```

Local installs validate `extension.json` and extension-relative executable paths. Builds are never implicit: use `--build=go` for local Go source; other languages must supply artifacts/launchers. Failed validation/builds leave no partial install.

`doctor` reports manifest errors, disabled/shadowed extensions, load errors, ready/auto-ready state, commands/tools, conflicts, warnings, and stderr log paths; normal startup remains fail-soft.

Develop with repeatable `zut --ext <path>`: no install, and working copies override installed namesakes.

### Updating extensions

`zut update` updates the binary and installed git extensions. Daemons keep the old binary until `zut daemon restart`; version mismatches print a reminder, even when zut is up to date. No automatic restart.

`zut update --restart-daemon` installs then runs the equivalent restart (starts a missing daemon). Sessions restore idle, not mid-turn. If already up to date, only a mismatched daemon restarts; otherwise: `zut: daemon already runs <version>; nothing to restart`. Restart failure returns an error but leaves the new binary installed. `zut update --check` only checks; it cannot combine with `--restart-daemon`.

Per extension:
- Skip disabled extensions and those without `.git/` (e.g. `zut ext install ./local-path`).
- Stash dirty/untracked files (e.g. `todos.json`, `config.json`), `git pull --ff-only`, then pop. Pop conflicts leave markers and a warning.
- Git failures (divergence, offline, etc.) report `failed` and continue; they never abort `zut update`.
- No post-pull build (`go build`, `npm install`, `make`). Authors should commit runnable artifacts (binary/transpiled JS). Otherwise build explicitly, reinstall, or `/reload-ext` a working copy.

### Theme-only extensions

`extension.json` plus `theme.json` or `themes/theme.json` needs no executable/subprocess. Themes appear in `/settings` with their source. See [docs/themes.md](docs/themes.md).

### Reference

Go, TypeScript, Node, and shell examples: `examples/extensions/`. Protocol and SDK API (`packages/agent/ext`): [docs/extensions.md](docs/extensions.md).

## Skills

Skills are folder-local `SKILL.md` files with YAML frontmatter. Startup discovery adds names to the system prompt; the built-in `skill` tool loads bodies on demand.

Defaults: built-ins plus user skills in:
- `./.zut/skills/<name>/SKILL.md` (project)
- `$ZUT_HOME/skills/<name>/SKILL.md` (global)
- `./.claude/skills/<name>/SKILL.md`, `~/.claude/skills/<name>/SKILL.md` (Claude-compatible)
- `./.agents/skills/<name>/SKILL.md`, `~/.agents/skills/<name>/SKILL.md` (agent-compatible)

User roots scan recursively. Nested skills retain frontmatter `name` and accept root-relative slash paths, e.g. `systems-backend/subskills/golang-patterns`.

Frontmatter, authoring, and `examples/skills/`: [docs/skills.md](docs/skills.md).

## Telegram bot (bridge)

Both modes run in the daemon: bridge the current TUI session, or run a separate headless bot session for phone DMs.

### From inside the TUI

`/telegram` opens **connect**, **disconnect**, **status**. Direct commands: `/telegram connect`, `/telegram disconnect`, `/telegram status` (alias `/tg`).

When connected:
- Paired-user DMs prompt the same session; switch between phone and terminal.
- TUI messages mirror as `you: ...`, replies as `zut: ...`.
- Telegram messages remain their own bubbles (no mirror); replies have no prefix.
- Status bar shows `- tg -`.

The daemon bridge survives `/detach`. It refuses to poll a bot already used by the bot service: competing long polls lose messages.

### Bot service

```bash
zut telegram-bot setup     # paste/verify/save BotFather token
zut telegram-bot start     # run in daemon; start daemon if needed
zut telegram-bot stop      # stop bot and close session
zut telegram-bot logs -f   # bot daemon-log lines; omit -f to print/exit
zut telegram-bot status    # masked config + running state
zut telegram-bot reset     # forget token and paired user
# `zut tg ...` aliases every subcommand
```

One headless session appears in `zut ls`, using `start`'s cwd, environment, and flags. Daemon restarts do not restart it; the next `start` creates a new session. Old transcripts remain resumable.

Setup:
1. Ask [@BotFather](https://t.me/BotFather) for `/newbot`; copy its token.
2. Run `zut telegram-bot setup`; paste the token.
3. Run `zut telegram-bot start` in the agent's working directory.
4. Send the bot `/start`. The first user claims it (`allowed_user_id`); all others are rejected.

DMs become prompts. Photos and `image/*` documents download for vision-capable models. Case-insensitive bot commands: `/help`, `/status`, `/stop` (cancel turn). Config: `$ZUT_HOME/bot.json`, mode 0600.

Both bridges remove existing webhooks before long polling, preserving pending updates. Telegram forbids simultaneous webhooks and `getUpdates`; do not share this token with a webhook service.

Normal flags apply: `--provider`, `--model`, `--cwd`, `--reasoning`, `--continue`, `--no-session`, `--no-tools`, etc. Example: `zut tg start -c --model claude-opus-4-1` resumes the latest session on Opus.

### Architecture: protocol-agnostic bot core

`packages/agent/modes/bot` owns turn queues, prompting, `/start`, `/help`, `/status`, `/stop`, status formatting, and per-turn credential refresh. Transports implement `BotAdapter`: inbound polling, replies, typing indicator, optional protocol-specific status. Telegram's adapter is in `packages/agent/modes/telegram`.

Add Discord, Slack, Signal, etc. with a new adapter package and subcommand; no runner, agent, or core changes. Channel IDs are adapter-owned opaque strings, not protocol-specific shared types.

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
cmd/zut/  main()
packages/provider/  LLM client surface, model catalog, streaming clients
packages/provider/auth/  credential store, api-key probe, oauth, login server
packages/core/  agent loop, sessions, cost tracking, compaction
packages/tui/  terminal raw-mode, input parser, editor, renderer, markdown, view
packages/agent/  cli wiring, arg parsing, system prompt, config
packages/agent/lsp/  LSP clients, server discovery, linters, diagnostics
packages/agent/extensions/  extension subprocess manager
packages/agent/extproto/  extension wire-format types
packages/agent/modes/  interactive tui, print, json, dialogs
packages/agent/modes/bot/  protocol-agnostic bot runner (BotAdapter interface)
packages/agent/modes/telegram/  telegram adapter, api client, daemon
packages/agent/tools/  read, write, edit, bash, worktree, grep, glob, lsp, web search, goals, sandbox
packages/agent/skills/  skill discovery, frontmatter parser, skill tool
packages/agent/subagents/  named profiles and resident background runtime
packages/agent/sdk/  public Go SDK for embedding zut in-process (package sdk)
packages/agent/ext/  public Go SDK for writing extensions (package ext)
```

Downstream consumers can depend on individual packages:
`go get github.com/bnema/zut/packages/core` pulls only `core` and its transitive deps (today: `provider`), no agent or TUI code.

## License

MIT
