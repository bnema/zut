# Resident subagents

> **V0.x breaking change:** zut no longer supports subprocess subagent state.
> Previous state is ignored rather than read or migrated; remove it when you
> no longer need it. Existing child jobs are never resumed.

Subagents are independent `core.Agent` conversations resident in the same zut
process as their parent. A child has a stable, private session identity, its own
provider client and tool registry, a cancellation boundary, and an authoritative
journal. Starting a child never launches another zut executable.

## Profiles

Profiles are Markdown files discovered from `~/.agents/agents/*.md`, paths in
`ZUT_AGENT_PROFILES`, and the `~/.pi/agent/agents/*.md` compatibility discovery
location. They are inputs only; zut does not execute them.

```markdown
---
name: reviewer
description: Read-only code reviewer
tools: [read, grep]
model: openai-codex/gpt-5.6-luna
thinking: high
systemPromptMode: replace
inheritProjectContext: false
inheritSkills: false
fastMode: false
---

Inspect the requested scope and report evidence-backed findings.
```

`tools` is a capability boundary. An omitted value inherits the child-safe
catalogue, `tools: []` grants no tools, and an explicit list replaces the
default. Names unavailable to the child (including host-only extensions) and
tools denied by `subagents.allowed_tools` are omitted from an explicit list;
they never grant the child additional access. `model`, `provider`, `thinking`,
the prompt mode, project/skill inheritance, and `fastMode` are resolved before
the child is accepted. A spawn-level model, provider, reasoning, or fast-mode
override takes precedence where supplied.

Children are always fresh conversations: they receive the delegated task and
resolved host/profile context, never the parent transcript or hidden reasoning.
They cannot recursively create more children.

## Lifecycle and persistence

The manager records each accepted child under its managed state root:

```text
subagents/<child-id>/
  transcript.jsonl  # authoritative accepted turns and finalized messages
  owner.lock        # host-ownership coordination; do not modify or remove
  .transcript-backup-* # retained private pre-repair transcript, when repaired
  metadata.json     # rebuildable bounded state
  result.json       # latest state plus a bounded final assistant summary
  patch.diff        # optional worktree capture
```

Acceptance is durable before `subagent_spawn` reports success. Finalized user,
assistant, tool-call, and tool-result messages are journaled; streamed deltas
and hidden reasoning are not exposed as ordinary history. A tool call is stored
before execution and reconciliation repairs incomplete call/result pairs.

All children stop when the host exits. On the next start, queued or running
turns are marked `interrupted`; zut never replays their task. Resume only with
an explicit new prompt through `subagent_resume`, the child-session composer,
or `/subagents resume <id> <prompt>`.

Multiple zut processes may share a state root. Each resident journal has one
host owner at a time. A process that finds a child owned elsewhere reports it
as such and does not inspect, recover, or modify its transcript. Resume that
child from its owning host or after the owner exits. On a subsequent safe
reconciliation, zut repairs only the known legacy false-recovery sequence; it
leaves any ambiguous journal corruption untouched. A repair retains the
pre-repair transcript as `.transcript-backup-*`; it can contain private session
data and is intentionally not removed automatically.

`subagent_status` reports `owned_elsewhere: true` when another process owns a
child. The field is omitted when false. A foreign-owned child cannot be resumed,
or have its history or result read, from this process.

The global scheduler admits the oldest eligible accepted prompt, runs at most
one turn for each child, and defaults to six concurrent turns. Set
`subagents.max_concurrent` to a positive limit to override it. Queueing is
unlimited by default; a positive `subagents.queue_timeout` cancels an accepted
prompt that has not received a slot and records its terminal failure durably.
`subagents.allowed_tools` is an allowlist for child-visible tools and
`subagents.allowed_roots` limits eligible child workspaces. Removed legacy
settings, including `tui_subagent_position`, are ignored.

`required: true` makes a delegated result an obligation of the parent turn.
Failed, cancelled, and interrupted required work remains unresolved until an
explicit successful follow-up. A host restart never retries it automatically.

## Tools and slash commands

The model-facing tools retain their logical names:

- `subagent_spawn` accepts `task`, optional `agent`, `model` and `provider`,
  `reasoning`, `fast_mode`, `required`, and `isolation` (`shared` or
  `worktree`). It returns a logical `subagent://<id>` reference.
- `subagent_status` returns bounded state for one child or the current set.
- `subagent_stop` stops one live child.
- `subagent_resume` accepts an explicit follow-up prompt for an existing child.

These calls are asynchronous. Completion arrives through the host’s typed
completion update, not process polling. Do not use sleep loops, repeated
status calls, journal files, or terminal UI inspection as a completion signal.
Successful completions include the final visible assistant summary, capped at
256 KiB; open the child session for the complete durable transcript.

`/subagents` opens the resident-child list, ordered by the latest state change.
Each row leads with the profile (or model), shows a human terminal state such
as `completed`, a relative update time, and a short ID. Press Down from the
main composer to open this list without clearing a draft when a child exists.
Use arrows and Enter to open a child session; `/subagents new <task>` creates a child; `/subagents logs <id>`
opens its history; `/subagents result <id>` shows the bounded final summary;
`kill <id>` stops it; and `resume <id> <prompt>` continues it.

The child session uses the same transcript renderer as the main session.
Recent history is bounded and loaded asynchronously; PgUp asks for an older
page. Up/Down preserves the current reading position, and PgDn returns to the
live tail after unread updates accumulate. The screen combines durable finalized
history with an immutable in-memory projection of the active turn. Enter submits
the local composer only after the manager durably accepts it; Esc closes the session. History may contain task
text, tool arguments, tool results, and source-derived output, so treat it as
private local session data.

## Worktrees and references

Shared children use the host working directory. With `isolation: "worktree"`,
zut prepares a detached Git worktree, captures a patch and changed-file list on
successful completion, and never applies that patch automatically. Failed or
interrupted worktrees are retained for inspection; an idle successful worktree
is cleaned when its child is explicitly stopped.

Logical references avoid exposing runtime paths:

```text
subagent://<id>
subagent://<id>/history
subagent://<id>/result
subagent://<id>/patch
```

## Provider caching

Each main and child conversation sends a stable logical session ID and a new
turn ID per accepted prompt to the provider layer. Provider adapters decide
whether and how to use them: OpenAI/Codex translates a supported stable session
into its cache key and routing header, while other adapters preserve deterministic
request input and use only cache controls their provider/model/endpoint supports.
An OpenAI-compatible wire shape is not itself evidence that a cache extension
or transport feature is available. Models continue through their declared
provider transport; a transport change must not change the logical session or
turn identity used by the provider request.
