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
symlinked skill files that resolve outside it.

When `XDG_STATE_HOME` is set on any platform, `$ZUT_HOME` defaults to
`$XDG_STATE_HOME/zut`. Otherwise it defaults to `~/Library/Application Support/zut/`
on macOS, `~/.local/state/zut` on Linux, or `%LOCALAPPDATA%\zut` on Windows.

## Inspecting installed skills

In zut, run `/skills`. A picker lists every discovered skill with its
description and source path. Press enter on a row to view the full
body inline. Press esc to go back.

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
