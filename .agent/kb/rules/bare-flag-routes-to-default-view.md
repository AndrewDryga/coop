---
name: bare-flag-routes-to-default-view
description: "a leading flag where a subcommand goes routes to the group's default listing"
scope: cli-grammar
sources: [internal/cli/tasks.go, internal/cli/taskcmd.go]
check: "none"
updated: 2026-08-09
---

# A bare leading flag routes to the group's default view

`coop tasks --blocked` — a flag where a subcommand would go — must run the group's default
view with that flag, i.e. `coop tasks ls --blocked`, not fail with an unknown-subcommand
error. A leading `-` token is never a subcommand; on a group whose bare form already lists
(see bare-subcommand-shows-help.md), it is a flag for that listing.

**Why:** `coop tasks --blocked` erroring with "works one queue at a time … (ls, lint, …)"
scolds the user for a natural shorthand — they dropped the obvious default verb, exactly as
bare `coop tasks` already lets them. It reads worst in an umbrella project, where that error
text is longest and least relevant to what they asked.

**How to apply:**
- Normalize in the group dispatcher BEFORE routing: after pulling value-flags like `--tasks`,
  if the first remaining token starts with `-` (and isn't the lone `-`), prepend the default
  verb (`ls`). The normal flag validator then names the supported flags on a typo.
- Only for groups with a *listing* default (today: `tasks`). A group whose bare form shows
  help has no listing flags to route — this is the flag-shaped sibling of
  bare-subcommand-shows-help.md.
- Not mechanically lintable (needs per-dispatcher flow analysis), so it stays a reviewed
  rule; check it whenever a list command grows flags.

## Changelog
- 2026-07-17 — created
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-08-09 — validate-on-write backfill: swept internal/cli/tasks.go's `cmdTasks` (confirmed the
  leading-flag-to-`ls` normalization at tasks.go:229) and checked every other group with a listing
  default for the same treatment. 1 violation found: internal/cli/backlog.go's
  `cmdBacklog`/`cmdBacklogFolder` — backlog gained a listing default after this rule was written
  ("A bare `coop backlog` lists the drawer... like bare `coop tasks`", backlog.go:76) but never
  got the matching normalization; a leading flag (e.g. `coop backlog -x`) falls through to
  `unknownErr("backlog command", ...)` instead of routing to the listing. `backlogArgSpecs["ls"]`
  takes zero flags today, so nothing currently demonstrates user-visible breakage, but the
  structural gap is real and the card's own "today: tasks" framing is now inaccurate. Queued for
  the lead, not fixed here.
