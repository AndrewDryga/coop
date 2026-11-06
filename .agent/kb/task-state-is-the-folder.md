---
name: task-state-is-the-folder
description: a task's state IS its directory; a bare `mv` to a missing state dir silently corrupts the queue
subsystem: tasks
sources: [internal/tasks/dir.go, internal/tasks/cmd.go, internal/tasks/queue.go, internal/tasks/projection.go, internal/cli/context.go, internal/cli/session_tasks.go]
updated: 2026-11-06
---
A task is a folder, and its STATE is which directory it sits in: `00_todo/` `10_in_progress/`
`50_blocked/` `99_done/` (the numeric prefix only sorts `ls` in lifecycle order). Moving the folder
IS the state change — there is no status field.

Inside a box `coop` isn't installed. A loop work box and an interactive agent box in a plain
checkout change state through the box's `coop-tasks` tools instead ([[in-box-task-channel]]); a
box without them (a fork session) moves folders itself. The trap there: a bare
`mv 00_todo/x 10_in_progress/` when `10_in_progress/` does NOT exist does not error; POSIX `mv`
silently RENAMES the task folder to a file called `10_in_progress`, corrupting the queue.
`ScaffoldStateDirs` (`internal/tasks/dir.go`) pre-creates all four state dirs precisely so a queue
or one-task fork projection is safe to move within. On the HOST use `coop tasks` (never a manual
`mv`); a producer that hands an agent a queue must scaffold the four state dirs first. Split slices
and whole-queue fork copies are retired because a second lifecycle tree is a second authority.

Removal syntax has one pure parser, shared by the command preflight, aggregate dispatcher and
single-queue handler. It accepts one ID or `--all-done`, never both, before repository/project/queue
discovery or archive counting. An empty archive or `--yes` does not excuse malformed arguments;
valid removals still use the shared confirmation gate and existing lease/authority cleanup.

ID-addressed commands, including `release`, share one ranking (`matchRank`): the exact ID, then the
ID or its slug (the ID without its date) in any case, then a part of an ID in any case. The best rank
any task has wins, in one queue and across queues alike, so `idea` names `2026-11-06-idea` beside
`2026-11-06-idea2`; duplicate matches at that rank refuse with queue-selection guidance, and a miss
that names a backlog idea says so with its `coop backlog promote` command. Task commands work only
queues inside a project: outside a Git repository (and without `COOP_REPO`) they refuse even an
existing queue, and an absolute `--tasks` outside the repository is refused; only a fork loop takes
one outside it. Release then uses the
ordinary single-queue handler: it returns an in-progress task to todo and clears the caller's
claim, preserving progress and handoff notes. It cannot clear a sandbox assignment or another live
process's claim. Update `state.md` before handing work back, as the CLI help instructs.
Explicit `--tasks` overrides configured queues and project-derived discovery.
`coop context --task` retains the selected Item from that same resolver instead of flattening
queues, which would let MatchTask choose the first duplicate exact ID. It validates original
argument boundaries before extracting queue flags, then resolves task metadata before optional
Git status; task paths still append last so route reasons and scope ordering stay stable.

## Changelog
- 2026-11-06 — an exact slug beats a longer ID, lookups ignore case, a parked idea's miss names its
  promote command, and task commands refuse every queue outside a project (task
  2026-11-06-keep-new-task-queues-out-of-git-settle-outside-q).
- 2026-11-03 — interactive agent boxes (`coop acp`, `coop <agent>`) now get the task tools too, so a
  hand move is only the fallback of a box without them (task
  2026-11-03-let-coop-acp-and-coop-run-agents-change-task-sta).
- 2026-09-22 — corrected release's lifecycle description against `tasksFolderRelease`: it returns
  active work to todo, preserves handoff notes, and refuses competing live claims and sandbox owners.
- 2026-09-05 — shared context task identity resolution and repeatable selectors; regressed
  duplicate/exact precedence, malformed syntax/metadata before Git and unchanged scope reasons.
- 2026-09-05 — wired release through aggregate identity resolution; verified derived/configured/
  explicit selection, unchanged task trees, ambiguity/usage denials and retained sandbox authority.
- 2026-09-05 — shared strict removal grammar now runs before discovery in single and aggregate
  paths. Regressed syntax refusals with retained completion-window authority, empty/populated
  archives and confirmation flags, plus valid aggregate and substring behavior.
- 2026-08-28 — replaced copied/split queues with a host-built one-task projection; the four-state
  move invariant is unchanged, but only the canonical folder decides project lifecycle.
- 2026-07-14 — reverified folder-state ordering, state-dir scaffolding, and atomic move semantics against `taskdir.go` + `taskcmd.go`.
- 2026-07-12 — created: task state = its folder; a bare `mv` to a missing state dir silently renames the task to a file (`scaffoldStateDirs` pre-creates all four).
- 2026-08-10 — sources repointed: `taskdir.go`/`taskcmd.go` moved to `internal/tasks/dir.go`/
  `internal/tasks/cmd.go` (the 2026-08 tasks/lease/completion-audit extraction); `scaffoldStateDirs`
  is now exported `tasks.ScaffoldStateDirs`. Facts unchanged.
