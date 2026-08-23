# zut themes

zut uses the controlling terminal as the source of truth for its default
appearance. Theme files are JSON overlays: every field is optional.

## Built-in choices

Open `/settings` and select **color theme**.

- **auto** (default) follows the terminal default foreground and background,
  ANSI palette, color depth, and reported light/dark scheme. It does not paint
  a full-row background or set a cursor color.
- **dark** and **light** use zut's fixed palettes. They remain fixed when the
  terminal appearance changes.
- A **custom theme** overlays `auto`. Unspecified roles remain terminal-owned;
  explicit JSON values remain literal overrides.

`inherited` is no longer a theme choice. A persisted `inherited` value resets
to `auto`.

`ZUT_THEME=dark` or `ZUT_THEME=light` forces that choice for one process. The
settings dialog still saves the next-launch preference, but cannot override the
environment for the running process. An unset `ZUT_THEME` or `ZUT_THEME=auto`
uses the saved preference.

## Live terminal changes

Interactive zut queries terminal defaults and ANSI slots at startup. When the
terminal supports DEC mode 2031 color-scheme notifications, zut follows
reported scheme changes and refreshes its terminal profile. It probes the mode
before changing it and disables it on exit only when zut enabled a known-reset
mode itself. Terminals without notification support receive a low-rate OSC 11
and scheme query fallback (about two seconds).

Terminal support varies through multiplexers and remote sessions. If a reply
does not arrive, zut remains usable with terminal default SGR colors and ANSI
slots. Existing scrollback is never recolored or cleared by a theme change;
only the visible live frame is repainted.

## Theme files and reload

User themes live under:

```text
$ZUT_HOME/themes/*.json
```

Extension-owned themes are loaded in place from:

```text
$ZUT_HOME/extensions/<extension>/theme.json
$ZUT_HOME/extensions/<extension>/themes/theme.json
<project>/.zut/extensions/<extension>/theme.json
<project>/.zut/extensions/<extension>/themes/theme.json
```

An active custom file is polled every 500 ms. A valid complete edit applies
without restarting zut. Invalid JSON, invalid colors, unsupported syntax
styles, and oversized files leave the last valid revision active. Brief atomic
rename gaps are tolerated; a file that stays missing resets the saved choice to
`auto` and shows a status message.

## Minimal files

```json
{
  "name": "pink-accent",
  "colors": { "accent": 204 }
}
```

Shared overrides can appear at the top level or directly in `colors`.
Mode-specific overrides go under `colors.dark` and `colors.light`; zut uses a
reported color scheme when available, and a background-luminance fallback
otherwise.

```json
{
  "name": "split-accent",
  "colors": {
    "dark": { "accent": 204 },
    "light": { "accent": 161 }
  }
}
```

```json
{
  "name": "spinner-only",
  "spinner_frames": ["◢", "◣", "◤", "◥"],
  "spinner_interval_ms": 120
}
```

## Color values

Every semantic color accepts one of:

```json
254
"#42454b"
{ "mode": "256", "index": 254 }
{ "mode": "ansi", "index": 100 }
{ "mode": "rgb", "r": 66, "g": 69, "b": 75 }
```

Numeric values are literal xterm-256 indexes. ANSI values are literal ANSI SGR
colors. RGB values use truecolor when available, otherwise zut quantizes them
to the active terminal capability. `auto`-generated colors are distinct:
they retain terminal-default or terminal-palette-slot provenance and therefore
change with the terminal palette.

Color roles are `fg`, `muted`, `accent`, `background`, `user`,
`user_bubble_bg`, `user_bubble_fg`, `assistant`, `tool`, `tool_out`, `error`,
`warning`, `spinner`, `thinking_max` (or `thinkingMax`), `selection_bg`, and
`selection_fg`. `background` intentionally paints the full zut row; omit it
to retain terminal background ownership.

`spinner_frames` contains 1–64 non-empty frames. `spinner_interval_ms` must be
between 10 and 10000. Theme files are limited to 1 MiB.

## Syntax

Custom `syntax` entries use Chroma style entries, for example:

```json
{
  "colors": {
    "dark": {
      "syntax": {
        "keyword": "#f05b8d bold",
        "literal_string": "#58c760",
        "comment": "#a1a1a1 italic"
      }
    }
  }
}
```

In `auto`, zut keeps Chroma for lexing but emits its own exact terminal SGR
sequences. Keywords, names, strings, errors, numbers, comments, and plain text
therefore retain terminal ANSI-slot identity instead of being remapped through
the xterm color cube.
