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
