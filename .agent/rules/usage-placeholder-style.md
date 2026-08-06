---
name: usage-placeholder-style
description: "one frozen `<angle>` lexicon for every usage string and error hint"
scope: cli-grammar
sources: [internal/cli/help.go]
check: "none"
updated: 2026-08-06
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
