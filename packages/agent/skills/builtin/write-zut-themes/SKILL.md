---
name: write-zut-themes
description: Help the user create, install, or package zut themes, including theme-only extensions.
---

# Writing zut themes

Use this skill when creating, editing, installing, debugging, or packaging a
zut theme. Read [docs/themes.md](../../../../../docs/themes.md) before
recommending a file shape.

## Theme model

`auto` is terminal-owned: it follows terminal defaults, ANSI palette, color
depth, and live appearance changes. A JSON theme overlays that adaptive base,
so missing fields remain terminal-owned. `dark` and `light` are fixed explicit
choices. There is no `inherited` selection.

User themes live at:

```text
$ZUT_HOME/themes/*.json
```

Extension-owned files are loaded in place:

```text
$ZUT_HOME/extensions/<extension>/theme.json
$ZUT_HOME/extensions/<extension>/themes/theme.json
<project>/.zut/extensions/<extension>/theme.json
<project>/.zut/extensions/<extension>/themes/theme.json
```

An active custom file reloads during interactive sessions. Always produce
complete valid JSON: zut keeps the prior valid file after a temporary invalid
save, and resets to auto only after persistent deletion.

## Minimal examples

```json
{
  "name": "pink-accent",
  "colors": { "accent": 204 }
}
```

```json
{
  "name": "split-accent",
  "colors": {
    "dark": { "accent": 204 },
    "light": { "accent": 161 }
  }
}
```

Top-level and `colors` values apply to both branches. Missing light or dark
branches use the available branch over the adaptive base.

## Valid values

All fields are optional. Semantic roles include `fg`, `muted`, `accent`,
`background`, `user`, `user_bubble_bg`, `user_bubble_fg`, `assistant`, `tool`,
`tool_out`, `error`, `warning`, `spinner`, `thinking_max`, `selection_bg`, and
`selection_fg`.

Use explicit color values only when an override is intended:

```json
254
"#42454b"
{ "mode": "256", "index": 254 }
{ "mode": "ansi", "index": 100 }
{ "mode": "rgb", "r": 66, "g": 69, "b": 75 }
```

Numeric values are literal xterm-256 indexes, not terminal palette slots. RGB
uses truecolor when available and falls back to terminal capability otherwise.
Omit `background` for terminal background ownership; setting it intentionally
paints full zut rows.

`spinner_frames` accepts 1–64 non-empty entries and `spinner_interval_ms` must
be 10–10000. Files must not exceed 1 MiB. Custom `syntax` entries use Chroma
style entries, such as `"#f05b8d bold"`.

## Theme-only extensions

A theme-only extension needs an `extension.json` and `theme.json`; no command
or executable is required.

```json
{
  "name": "my-theme-extension",
  "version": "1.0.0",
  "description": "Ships a zut color theme",
  "enabled": true
}
```
