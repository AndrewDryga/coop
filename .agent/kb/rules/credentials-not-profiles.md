---
name: credentials-not-profiles
description: "a stored account is publicly a \"credential\"; \"profile\" is retired, not aliased"
scope: cli-grammar
sources: [internal/cli/profiles.go, internal/cli/help.go, internal/cli/cli_test.go, internal/cli/commands.go, README.md, site/docs.html, MIGRATING.md, docs/agents.md]
check: "none"
updated: 2026-10-17
---

# The account concept is publicly named "credentials", never "profiles"

**The rule:** Every current user-facing surface — command names, flags, help, errors, hints, docs —
says **credential(s)** for a stored account/login (a rate-limit slot). Historical migration tables
may name the retired form only to map it to the current command. "Profile" is the
pre-v3 name and is RETIRED, not aliased: `coop profiles` resolves as a plain unknown
command — no alias, no did-you-mean (the edit distance is past the threshold), and no
special retired-form note either (`TestV3RetiredForms`: v3 carries no legacy, not even a
tombstone hint) — and the
`--profile` FLAG is removed outright (no tombstone): coop doesn't recognize it at all, so
on an agent launch it forwards to the agent like any other arg (codex has its own
`--profile`) and elsewhere it's an unknown argument. The RECIPE concept (lead + roles +
models) is a **preset**, never a profile.

**One exception:** a removed SAFETY flag whose passthrough could reach an agent's own same-named
flag and start a writable box is refused as an unknown option with advice
(`refuseRemovedExposureFlags`). `--readonly` and `--bare` are refused that way in every command
that took them, because `claude --help` shows Claude Code's own `--bare` ("Minimal mode: skip
hooks, LSP, plugin…"): forwarding coop's would start a normal writable box. It is still not an
alias, since nothing runs under the old flags, and a flag after `--` still reaches the agent.

**Why:** "Profile" was carrying two unrelated meanings (an account, and a runtime
preference bundle), which is exactly what the credentials/presets split resolved. Any
new "profile" wording — or a surviving alias — re-muddies it; the user has directed
repeatedly that v3 carries NO legacy.

**Internal code is exempt:** on-disk layout (`<agent>/profiles/<name>/`), Go identifiers
(`ProfileAuthed`, `SetActiveProfile`, `DefaultProfileOf`), and file names stay — renaming
storage would force a data migration for zero user value. The boundary is what a USER
reads.

**Mechanical check:** grep new user-facing strings for `profile` before landing:
`grep -rn '"' internal/cli --include='*.go' | grep -i profile` should surface only internal
identifiers and tests, never current help, errors, or hints.

## Changelog
- 2026-07-03 — created
- 2026-07-04 — revised
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-08-09 — validate-on-write backfill: swept the card's own mechanical check (`grep -rn '"'
  internal/cli --include='*.go' | grep -i profile`) plus `--profile` flag handling. 0 stray
  user-facing "profile" strings (every hit is an internal identifier — `AgentProfileDir`,
  `ActiveProfile`, `SetActiveProfile`, `DefaultProfileOf`, `EffectiveProfiles`, `ProfileAuthed` — or
  a test file) and `--profile` is unhandled, so it forwards through like any other arg. 1 finding:
  the card's claim that `coop profiles` "fails loudly with the rewrite to `coop credentials`" is
  stale — it now falls through cli.go's default dispatch case to a generic `unknownCommandErr`
  with no did-you-mean (levenshtein("profiles","credentials")=8, past the threshold) and no rewrite
  pointer, confirmed by internal/cli/cli_test.go's `TestV3RetiredForms` comment ("not a special
  'X is retired' note"). Card-vs-code drift, not a code bug — possibly-wrong, flagged for the lead.
- 2026-08-09 — drift repair from the backfill sweep's findings: claim corrected — coop profiles is a plain unknown command, no tombstone hint (TestV3RetiredForms).
- 2026-08-26 — removed retired-command history from current help and README guidance; kept the
  historical migration mapping and exempt internal storage terminology.
- 2026-10-05 — the README became a short landing page and its detail moved to docs/agents.md (task 2026-10-05-rewrite-the-readme-and-markdown-docs-in-the-webs); sources follow it.
- 2026-10-17 — recorded the one exception to "no tombstone": the removed `--readonly` and `--bare`
  are refused as unknown options with advice instead of being forwarded, because Claude Code has its
  own `--bare` ("Minimal mode: skip hooks, LSP, plugin…" in `claude --help`) and forwarding coop's
  would start a normal writable box. Swept the six commands that took them; each refuses both.
