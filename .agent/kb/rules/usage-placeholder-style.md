---
name: usage-placeholder-style
description: "one frozen `<angle>` lexicon for every usage string and error hint"
scope: cli-grammar
sources: [internal/cli/help.go]
check: "none"
updated: 2026-08-09
---

# One frozen lexicon for usage placeholders

Every `Usage:`/`usage:` string and error hint spells a value the SAME way, in `<angle>` brackets,
using this fixed lexicon — so a value never reads `m` here and `<m>` there:

| Value | Placeholder |
| --- | --- |
| a coding agent | `<agent>` (or the literal `claude\|codex\|gemini\|grok`) |
| a credential name | `<name>` (a bare arg) or `<credential>` (the slot in `coop credentials <agent> <credential> …`) |
| a model | `<model>` |
| a filesystem path (dir or file) | `<path>` |
| a task id | `<id>` |
| a count | `<n>` |
| "one or more" / continuation | ASCII `...` — never the Unicode `…` |

**Why:** the v3 audit found the same value spelled `p` / `<name>`, `m` / `<m>` / `<model>`,
`<dir>` / `<path>`, and Unicode vs ASCII ellipses across help and error strings — noise that makes
the CLI read as several tools. A user shouldn't have to learn that `m` and `<model>` mean the same.

**How to apply:**
- A new usage/error string → use the placeholders above; wrap in `<…>`; ASCII `...` for repetition.
- Never abbreviate to a single letter (`p`, `m`) and never use the Unicode ellipsis in a usage string.
- NOT mechanically gated yet. `TestCLIConformance` (`internal/cli/conformance_test.go`) landed and
  graduates [[list-verb-ls]], [[destructive-verb-rm]], and [[help-output-style]] — but it does not
  read usage strings for this lexicon. Enforce in review until someone adds that arm, then put its
  command in `check:`.

See also [[help-output-style]].

## Changelog
- 2026-07-02 — created
- 2026-07-11 — revised
- 2026-08-06 — card metadata added; corrected the stale conformance claim — TestCLIConformance landed, but it covers ls/rm/help rows, NOT the placeholder lexicon
- 2026-08-09 — validate-on-write backfill: swept every `Usage:`/`usage:` string and error hint in
  internal/cli/*.go (non-test), including help.go's full `commandHelp` text blocks. 2 violation
  clusters found: (1) Unicode ellipsis `…` instead of the required ASCII `...` in the `--peer`
  repeatable-flag documentation and its runtime error hint — help.go:98,258,437,470,709,777 and
  commands.go:319,1676; help.go:709's own `coop loop` Usage: line mixes both forms
  (`[--tasks <path>]...` ASCII, correct, vs `[--peer <peer>…]` Unicode, wrong) in one string. (2)
  Single-letter placeholders in taskcmd.go:368's add-task usage error — ``usage: coop %s "<title>"
  [--context <c> --acceptance <a> --approach <p> --subtask <s>...]`` — the exact `p`/`m`-style
  abbreviation the rule's own "Why" section names. Both queued for the lead as violations, not
  fixed here.
- 2026-08-09 — fixed sweep: both clusters above corrected. `…`→`...` at help.go:98,258,437,470,778
  and its now-doubled site help.go:710 (`[@account,…]` and `[--peer <peer>…]` in the same `coop
  loop` Usage: line), plus commands.go:319 and commands.go:1676 (same `[@account,…]` +
  `[--peer <agent>]…` pair, error-hint form). Also normalized tasks.go:352's `add "…"` hint to the
  established `add "<title>"` form (same value, same rule). taskcmd.go:368's single letters moved
  to taskcmd.go:371 by intervening commits; `<c>/<a>/<p>/<s>` → `<context>/<acceptance>/<approach>/
  <subtask>`, matching the flag names. help.go:258's column layout is auto-computed by rune count
  (`row`/manual two-space columns) and stayed aligned; verified by rendering `coop help`/`help
  <agent>`/`help loop`/`help fusion`. `make docs` regenerated docs/cli.md, docs/man/coop.1,
  site/llms.txt with no other drift. Left every other `…` hit in internal/cli/*.go alone —
  narrative comments, truncation ellipses (util.go's `truncate`, acpcontrol.go tool-title/history
  elision, commands.go's log-tail clip, loopchanges.go's audit-evidence clip), live status/list
  narration (ratelimit.go's countdown, taskcmd.go/taskwatch.go's "+N more" elision), and the
  unrelated `<…>` task-scaffold "fill this in" marker in taskcmd.go (294/378/1874/1916) — none are
  a `Usage:`/`usage:` string or error hint.
