# zut skills

A skill is a reusable instruction set written as a single
`SKILL.md` file with a YAML frontmatter header. Unless a skill disables
model invocation, zut discovers it at startup and surfaces it to the model
in two ways:

1. The system prompt gains a short manifest:
   `Available skills: ... - code-review — Run a self-review pass...`
2. A built-in `skill` tool lets the model load any one skill's full
   body on demand.

The on-demand-load model keeps token usage cheap: only the manifest
goes into every request; the body is fetched as a tool result the
one or two turns the model actually needs it.

## Anatomy

```markdown
---
name: code-review
description: Run a thorough self-review pass on a recent change.
allowed-tools: [read, bash]
permissions:
  bash: ["git diff*", "git log*"]
---

# Code review

When asked to review code, ...
```

### Frontmatter fields

| field | required | purpose |
|---|---|---|
| `name` | optional | skill identifier; defaults to the directory name |
| `description` | required | one-line summary shown in the system prompt |
| `disable-model-invocation` | optional | when `true`, hide the skill from the model's startup manifest; invoke it explicitly with `/skill:<name>` |
| `allowed-tools` | optional | list of tool names the skill is meant to use; informational |
| `permissions` | optional | per-tool patterns; informational |

`allowed-tools` and `permissions` are **parsed but not enforced** in
this version. They appear in the rendered skill body so the model can
see them and self-regulate. Future versions may enforce.

The body (everything after the second `---`) is plain markdown.
There's no template engine; the model sees what you write.

## Discovery

zut looks in these directories, in priority order, and registers the
first `SKILL.md` it finds for each unique name:

| location | scope |
|---|---|
| `./.zut/skills/<name>/SKILL.md` | project (native) |
| `$ZUT_HOME/skills/<name>/SKILL.md` | global (native) |
| `./.claude/skills/<name>/SKILL.md` | project (claude-compat) |
| `~/.claude/skills/<name>/SKILL.md` | global (claude-compat) |
| `./.agents/skills/<name>/SKILL.md` | project (agent-compat) |
| `~/.agents/skills/<name>/SKILL.md` | global (agent-compat) |

The compat paths are deliberate: a `SKILL.md` written for an existing
skill ecosystem works in zut unchanged. Drop your existing
`.claude/skills/` or `.agents/skills/` directories into a project and
zut will pick them up. User skill roots are scanned recursively, so
nested files such as `~/.agents/skills/systems-backend/subskills/golang-patterns/SKILL.md`
are discovered too. The frontmatter `name` is the canonical identifier;
for nested user skills, the slash-separated directory path relative to the
skill root is also accepted by the `skill` tool and `/skill:` commands.

Enabled extensions may bundle skills by declaring relative directories in
`extension.json`, for example `"skills": ["skills"]`, with files at
`skills/<name>/SKILL.md`. A declared directory may also contain a single
`SKILL.md` directly; its directory name supplies the fallback skill name and
child directories are not scanned. Bundled skills are copied by `zut ext
install` and loaded automatically. User/project skills take precedence over
bundled skills; bundled skills take precedence over embedded built-ins. When
two extensions provide the same skill name, the first declaration wins. The
host rejects manifest paths that escape the extension directory, including
symlinked skill files that resolve outside it. Skill directories inside a
bundled directory may be symlinks, but only while they resolve inside the
extension directory; a link that leaves it is reported and skipped. Real
directories load before links, a directory reached through both a real path and
a link loads once, and only direct children of the declared directory are
considered.

User and project skill roots may themselves be symlinks, and so may the
directories inside them, so a checkout can live in one place and be linked into
a skills directory. Real directories are scanned before links, so a link cannot
take a name or slash alias ahead of the real tree. A directory reached twice
through links is scanned once, link cycles are ignored, and a link that points
at nothing is skipped silently. A link to the skills directory itself or to any
of its parents (for example `~` or `/`) is ignored, so it cannot pull the
surrounding filesystem into discovery; links to unrelated shared checkouts work
normally. Each skills directory is scanned to at most 64 levels and 10,000
directories; beyond that zut reports a warning, keeps the skills already found,
and skips the rest. Other failures, such as a permission error on a
link target, are reported. A skill found through a link reports the installed
(link) path, and its slash alias follows the path it was reached by.

When `XDG_STATE_HOME` is set on any platform, `$ZUT_HOME` defaults to
`$XDG_STATE_HOME/zut`. Otherwise it defaults to `~/Library/Application Support/zut/`
on macOS, `~/.local/state/zut` on Linux, or `%LOCALAPPDATA%\zut` on Windows.

## Inspecting installed skills

In zut, run `/skills`. A picker lists every discovered skill with its
description and source path. Press enter on a row to view the full
body inline. Press esc to go back.

## Pinning skills

In the `/skills` list, press `p` to toggle a project pin or `g` to toggle a
global pin. Rows show `[p-]`, `[-g]`, or `[pg]` for the selected scopes. Global
and project pins are combined in alphabetical order of canonical skill name,
with each skill loaded only once even when pinned under both its name and a
nested slash alias. Unpinning one scope leaves the other intact. When the host
provides no pin preference callbacks, the picker is read-only: no markers are
shown and `p`/`g` do nothing. A failed save reports an error without changing
preferences.

On a fresh interactive session, a read-only `[Pinned skills]` section above the
input lists the skills that will accompany your next message, each with its
scope: `(project)`, `(global)`, or `(project + global)`. Opening zut does not
start a model turn. The complete skill bodies and their directories are
included in your first genuine user message and persisted with it, without
changing the editor text. `/clear` prepares pins again, as does `/cd` for the
new directory. Resuming, importing, or forking an existing session does not
inject pins, even when its transcript is empty. Changing models and compacting
context do not reload pins.

Changing pins before the first message updates the pending selection; after it,
changes apply on the next fresh conversation or `/clear`. Unpinning does not
remove instructions already in the transcript. A queued user prompt carries the
preload once. Recalling or discarding it before delivery restores the raw draft
and leaves pins pending for the next user submission; injected bodies are not
inserted into the editor. A failed or cancelled pre-turn compaction also leaves
the undelivered selection pending for the next user submission. Recall restores
the selection already attached to that draft; pin changes made after its
submission still apply on the next fresh conversation or `/clear`.

Pins are persisted as part of the first user message. Session previews and
export filename slugs derived from that message can therefore show the pin
preamble rather than the user's request. Input history and queued-draft recall
show the original request.

Preferences live in `$ZUT_HOME/skill-pins.json`, never in the repository:

```json
{
  "global": ["code-review"],
  "projects": {
    "/workspace/project": ["test-fix"]
  }
}
```

Project keys are the absolute starting directory with symlinks resolved when
possible, not the Git root, so subdirectories have separate project pins and a
symlinked alias shares the pins of its target. Pins store discovered skill
names, not copies of skill files, so normal discovery precedence applies.
Missing or disabled skills produce a warning and are skipped. A malformed
preferences file is reported and is never overwritten by a toggle; fix or
remove it. The file must contain a JSON object, so `null`,
arrays, and scalars count as malformed. Only the `global` and `projects` fields
are supported: any other top-level field is reported as an unsupported field and
the file is left untouched, so unknown data is never silently dropped by a save.
If `skill-pins.json` is a symbolic link, zut reads pins through it but refuses
to save (the link and its target are never replaced or modified), so pinning
from `/skills` reports an error; make the file a regular file to change pins
from zut. Saves replace the file through a temporary file
in the same directory. Concurrent edits from separate zut processes are not
merged, and an open session refreshes preferences on `/skills` and `/clear`,
not continuously.

`--no-skill` disables pin preloading. Pinning a skill that sets
`disable-model-invocation: true` is allowed. Print, stream, and JSON modes also
include pins with the first prompt of a fresh session, including runs with
session persistence disabled; zutfile startup `pre` commands run first, and
warnings go to stderr. Resumed sessions get nothing. Explicit `--orchestrate`
runs, RPC, the SDK, standalone bot modes, and resident background workers do not
load pin preferences. The
interactive Telegram bridge uses the interactive conversation's pins.

**Token cost:** pinning saves repeated invocation, not tokens. Every pinned
skill's complete body is added once to the first message and stays in the
context window (and in every later request) until compaction summarizes it,
unlike the on-demand `skill` tool where only the short manifest is always
present. Keep pins small, especially with local models.

## Invoking skills

For normal skills, the system prompt tells the model the skill names and
short descriptions. When a request matches, the model calls the `skill` tool
to load the full instructions on demand.

To force a specific skill, invoke it as a slash command. Typing `/skill:` opens a filtered list of discovered user skills. Use the arrow keys to select one, `tab` to complete its name, or `enter` to invoke the highlighted skill.

```text
/skill:code-review
/skill:code-review focus on security issues
```

zut expands the command into a user message containing the complete skill
body, its directory for resolving relative references, and any text following
the command as the request. This bypasses model-side skill selection.

Set `disable-model-invocation: true` in a skill's frontmatter when it should
only run after explicit user invocation. The skill remains visible in
`/skills` and available through `/skill:<name>`, but its name and description
are omitted from the model's startup context.

## Writing good skills

- **Be procedural.** Number steps. Tell the model what to do in what
  order. Skills are habits, not knowledge dumps.
- **Be precise about boundaries.** "Stop after step 4" is more
  effective than "don't go too far".
- **Trim aggressively.** A 200-line skill bloats every turn the
  model uses it. Aim for 20–80 lines.
- **One skill per behaviour.** Don't pack three workflows into one
  SKILL.md; the model picks one path. Two separate skills work better.
- **Lead with the trigger.** First paragraph should make it
  obvious *when* to use the skill so the model self-selects correctly.

## Examples

See `examples/skills/` for starter skills:

- `code-review/` — self-review pass on a recent diff
- `test-fix/` — diagnose + minimally fix a failing test

## Comparison to other discovery layouts

| ecosystem | path | zut reads it? |
|---|---|---|
| (native) | `.zut/skills/<name>/SKILL.md` | yes |
| (claude-style) | `.claude/skills/<name>/SKILL.md` | yes |
| (agent-style) | `.agents/skills/<name>/SKILL.md` | yes |

Cross-pollination is intentional: pick whichever convention you're
already using and zut tags along.
