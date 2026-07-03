# Scaffolded, editable config reads top-down and works as-is

Files that `coop <x> init` writes for a human to open and edit (preset.yaml,
`.agent/fleet.yaml`, the prompt Markdown) follow two rules:

**1. Comments LEAD their field — never trail it.** Put the note on its own line(s)
*above* the field, not after it:

    # REQUIRED — one of: claude, codex, gemini.
    agent: claude

not `agent: claude   # REQUIRED — ...`. Trailing comments are harder to scan and
force width games. Separate items with newlines, not `·`/`|` inline separators — a
comment block has vertical room, so one item per line (e.g. `Run:` / `Inspect:` /
`Format:` labels, each on its own line) beats cramming several onto one with dots.

**1a. Document EVERY field inline — don't bury field docs in a section header.**
Each field gets its own leading comment, on the lead AND on every role. A section
header covers section-level facts only (what the map is, the name rule); it is not
where per-field docs go — a reader shouldn't have to scroll up to learn what a field
does. Repeating a short common-field note across sibling roles (`# agent — one of:
claude, codex, gemini.` on each) is fine and expected; a bare field is not. Keep
each comment to one concise line to hold down the cost, and fold a field's mode/kind
note into that field's comment (e.g. the "native subagent" explanation sits on
`mode:`) rather than a separate block, so nothing is said twice.

**2. Scaffolded content is a usable default, not a fill-in-the-blank.** What you
generate must stand on its own for a generic project — real how-to-work guidance,
a runnable recipe — not `<placeholder>` / "(edit me)" stubs the user MUST replace
before it works. Tuning is *recommended*, not required; a leading HTML/`#` note
says "sensible defaults; tune for yours, or delete this" and how to drop it.

**Why:** reviewing the preset scaffold, the user said the leading form is "easier
to read," and "the roles you generate should be good enough for generic projects,
not something you MUST fill in (even though it's recommended)."

**How to apply:**
- New `init` scaffold (or edit to `preset.Template` / `fleetTemplate`): leading
  comments, usable defaults, every line ≤ 80 display columns (count runes — the
  comments use `—·→…`, which are multi-byte).
- Terse REFERENCE illustrations are exempt — a `coop help` example is a compact
  cheat-sheet where trailing annotations and tight space are the point, not a file
  anyone edits.
- A committed dogfood copy (e.g. `.agent/presets/frontier/`) must be REGENERATED
  from the template after any template change, never hand-edited, so it can't drift
  (Scaffold to a temp module main, since `internal/` can't be imported from /tmp).

See [[scaffold-fits-the-repo]] (a scaffold suits the target repo) and
[[help-output-style]].
