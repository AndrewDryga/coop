---
name: credentials-not-profiles
description: "a stored account is publicly a \"credential\"; \"profile\" is retired, not aliased"
scope: cli-grammar
sources: [internal/cli/profiles.go, internal/cli/help.go]
check: "none"
updated: 2026-08-09
---

# The account concept is publicly named "credentials", never "profiles"

**The rule:** Every user-facing surface — command names, flags, help, errors, hints, docs —
says **credential(s)** for a stored account/login (a rate-limit slot). "Profile" is the
pre-v3 name and is RETIRED, not aliased: `coop profiles` fails loudly with the rewrite to
`coop credentials` (the v3 tombstone pattern — no working aliases, ever), and the
`--profile` FLAG is removed outright (no tombstone): coop doesn't recognize it at all, so
on an agent launch it forwards to the agent like any other arg (codex has its own
`--profile`) and elsewhere it's an unknown argument. The RECIPE concept (lead + roles +
models) is a **preset**, never a profile.

**Why:** "Profile" was carrying two unrelated meanings (an account, and a runtime
preference bundle), which is exactly what the credentials/presets split resolved. Any
new "profile" wording — or a surviving alias — re-muddies it; the user has directed
repeatedly that v3 carries NO legacy.

**Internal code is exempt:** on-disk layout (`<agent>/profiles/<name>/`), Go identifiers
(`ProfileAuthed`, `SetActiveProfile`, `DefaultProfileOf`), and file names stay — renaming
storage would force a data migration for zero user value. The boundary is what a USER
reads.

**Mechanical check:** grep new user-facing strings for `profile` before landing:
`grep -rn '"' internal/cli --include='*.go' | grep -i profile` should surface only
the `coop profiles` tombstone line and internal identifiers.

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
