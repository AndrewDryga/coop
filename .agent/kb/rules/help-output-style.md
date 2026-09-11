---
name: help-output-style
description: "UPPERCASE headings with one em-dash explanation, one command per line, no `·`, command cell under 32 runes"
scope: cli-output
sources: [internal/cli/help.go, internal/cli/cli.go, internal/cli/fork_cmd.go, internal/cli/presetcmd.go, internal/cli/help_test.go, internal/cli/approved_output_test.go, internal/cli/conformance_test.go]
check: "none"
updated: 2026-09-11
---

# Help output: UPPERCASE section headers, one command per line, no "·"

`coop help` and every `coop <cmd> --help` are a scannable command reference, not prose:

- Section headings are UPPERCASE (`THE BOX`, `RUN AGENTS`, `FORKS`, `SETUP & MAINTENANCE`);
  sub-labels are capitalized too (`Usage:`, `FLAGS`, `REVIEW`).
- Each menu heading carries ONE short explanation after an em dash, in normal text (only the
  UPPERCASE name is bold): `TASKS — each task is a folder in .agent/tasks/`. Name the actual file
  or directory when it helps a newcomer find the feature — `.agent/tasks/`, `.agent/loop.yaml`,
  this project's Compose file — rather than describing the category abstractly.
- The menu is STATE-AWARE where that helps and nowhere else: a `GET STARTED` block appears only
  with no usable account, the SERVICES heading names this project's real services, and with none
  configured the whole `coop up`/`coop down` rows are dimmed while the row that ADDS one stays
  bright. Base it on project configuration, never a runtime probe; an unreadable configuration is
  not proof of absence. The deterministic reference form (`coop help --all`, docs) drops all of it.
- One command per line. Never collapse distinct commands into a `coop fork <verb>`
  placeholder, and never pile several commands' descriptions behind a `·`.
- No `·` (middle dot) anywhere in help text — split into labeled lines or list rows.
- Pad the command column on PLAIN text so a description never glues to a long command
  (`row()` keeps a minimum gap) — see [[no-color-in-width-fields]].
- Flags, examples, and sub-verbs live in the command's own `coop <cmd> --help`, not
  crammed into the top-level index. `coop help <cmd> [<sub>]` and `coop <cmd> [<sub>] --help`
  resolve to the SAME page — one renderer, two spellings — including for a registered agent, whose
  own CLI is reached explicitly with `coop <agent> -- --help`. A help request runs nothing: no
  runtime, no login, and none of the command's own required arguments.
- The closing footer belongs to no section, so it starts at column zero.
- Name the concrete file/artifact a command acts on, not a vague category. `coop up`/`down`
  say **`.agent/compose.yml`** (its real services when present), never "sibling services" — a
  glanceable row has no body to explain an abstraction, so the concrete name IS the
  explanation. Same for any row/error one-liner: prefer the real path/file/flag.
- A row's command cell stays **≤ 32 runes** so every description starts at the same column
  (the table look breaks the moment one row pushes past the gap). Too long? Drop optional
  flags from the cell — they live in the command's own help page.
- A group with more than ~6 rows is a wall — split it by what the user is doing (e.g.
  AGENTS / CREDENTIALS, MODELS & PRESETS / THE BOX), not by implementation.
- **A topic page is prose for one job, not an index.** The reference above is the top-level
  table and the group pages that list commands. A page that answers "how do I use this one
  thing" — `coop help <agent>`, `coop help presets`, `coop help <preset>` — uses Title-case
  section headings (`Usage:`, `Examples`, `Options`, `Run it`, `Lead models`, `Edit this
  preset`), says what the reader does rather than how coop implements it, and ENDS with the one
  pointer that continues their work (`coop help presets`, `coop help models`) instead of the
  generic `Run 'coop help' for all commands.` footer. Registering a page as self-contained is
  `selfContainedHelp` in help.go; agent and preset pages print through their own renderer.

**Why:** the top-level help is scanned, not read. Lowercase headers, collapsed verbs, and
`·`-separated descriptions read as clutter; people expect a man-page-like reference where
each command stands on its own line. "I never saw any docs collapsing it like that." And on
the up/down rows: "it should stay the filename so it's obvious what this is" — "sibling
services" hides the one thing that makes the row make sense.

**How to apply:**
- New command → add a one-line row to `helpText` (under its group) AND a `commandHelp`
  entry (synopsis + `Usage:` + flags). A test ties `commandHelp` to `topLevelCommands`.
- Focused regressions cover parts of the rule: `TestHelpRowsAlign` gates the 32-rune command cells in
  top-level and fork help; `TestHelpTextAligned` checks the top-level column gap, selected rows, and
  section headers; `TestAllHelpAvoidsMiddleDots` renders every help page; `TestCLIConformance` ties
  live verbs and selected usage forms to help. The card remains `check: none` because those tests do
  not enforce capitalization and one-row layout on every focused page; review owns the full rule.
- Never put `·` in a help string. Runtime status/stat lines may still use it as a separator.

## Changelog
- 2026-09-11 — implemented the approved main menu: ten UPPERCASE groups each with one em-dash
  explanation, the state-aware `GET STARTED` and SERVICES variants, and a column-zero footer. The
  exact bytes of all four approved menu states are pinned in `internal/cli/testdata/approved`
  (`TestApprovedMainMenu`), which supersedes prose review for that surface. Swept the rest of the
  rule's claims: the old 80-column budget became the approved menu's own 92 (the `coop context`
  row sets it), and `coop help <cmd> <sub>` / `coop <cmd> <sub> --help` now share one resolver.
  NOT changed here: the topic pages' Title-case headings, which the review supersedes with
  UPPERCASE — they are rewritten with their own command's approved page, not by this slice.
- 2026-09-11 — added the topic-page tier from the approved per-agent/preset transcripts: Title-case
  headings and a closing pointer instead of UPPERCASE headers and the all-commands footer. Swept
  every page rendered through `printCommandHelp`: only the agent pages, `presets`, and the preset
  projection are self-contained today; the command index and every group page keep the reference
  shape unchanged.
- 2026-09-03 — added `forkHelpText` and both test files to `sources`; replaced the false claim that
  `TestHelpRowsAlign` enforced the whole card with an exact test-to-claim map, and set `check: none`
  because the focused regressions do not enforce every page-level claim.
- 2026-08-25 — removed the Fleet-only long verb-list exception, shortened the direct-fork rows,
  and extended `TestHelpRowsAlign` from the top-level index to `forkHelpText`; both surfaces now
  enforce the 32-rune command-cell rule.
- 2026-08-10 — path-only: `forkHelp`/`forkHelpText` stayed in cli as
  `internal/cli/fork_cmd.go` while the `·`-bearing runtime paths (`forkBrief`, the merge prompt)
  left for `internal/forkctl`. The guard is unchanged; its second half now points at two files.
- 2026-06-17 — created
- 2026-07-11 — revised
- 2026-08-06 — card metadata added (format v1); body unchanged
