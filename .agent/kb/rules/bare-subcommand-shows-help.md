---
name: bare-subcommand-shows-help
description: "a bare group prints help or its default view, never an empty-token error"
scope: cli-grammar
sources: [internal/cli/help.go, internal/cli/fork.go, internal/cli/tasks.go]
check: "none"
updated: 2026-08-09
---

# A bare subcommand group shows help, never an "unknown command \"\"" error

`coop <group>` with no subcommand must print that group's help and exit 0 — not
`coop: unknown <group> command "" — use: …`. An empty token means "tell me the
options," which is exactly what help is for; `unknownErr` (with its did-you-mean) is
for a *mistyped* subcommand, not a *missing* one.

**Why:** `coop tasks` → `unknown tasks command "" — use: list, lint, add, split`
reads as an error for doing nothing wrong, and buries the options in a one-line scold.
Bare `coop` prints help; a bare group should match that.

**How to apply:**
- In every group dispatcher, branch on the empty subcommand BEFORE `unknownErr`:
  `case "": return groupHelp("<group>")` (helper in help.go). Keep `unknownErr` only
  for a non-empty, unrecognized token.
- A group that has a *useful default view* may show that instead of help — the
  invariant is "never the empty-token error," not "always help." Current sweep:
  `fleet` → help; `pool` → shows the pool; `profiles` and `tasks` → list their queue;
  `fork` → `forkHelp`. None emits the empty-token error.
- Not easily lintable (it needs flow analysis of each dispatcher), so this stays a
  reviewed rule; check it whenever you add or touch a subcommand group.

## Changelog
- 2026-06-26 — created
- 2026-07-02 — revised
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-08-09 — validate-on-write backfill: swept every top-level group dispatcher's empty-token
  handling — fork (cmdFork → forkHelp, fork.go:99), fleet (cmdFleet → groupHelp("fleet"),
  fork_fleet.go:154), tasks (cmdTasks/tasksInQueue → list, tasks.go:306+330), backlog
  (cmdBacklog/cmdBacklogFolder → list, backlog.go:88), credentials (cmdCredentials → list,
  profiles.go:27), presets (cmdPresets → list, presetcmd.go:17), sessions (cmdSessions →
  groupHelp("sessions"), session_cmd.go:116). 0 code violations — every group avoids the
  empty-token error. 1 finding: the card's own "Current sweep" bullet is stale — it names "pool"
  (fully retired; not a case in cli.go's dispatch switch at all, confirmed by
  `TestV3RetiredForms`) and "profiles" (renamed to "credentials"), and never mentions backlog,
  sessions, or presets, which already correctly hold the invariant. Card documentation drift, not
  a code bug — flagged for the lead, not fixed here.
