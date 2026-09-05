---
name: task-state-is-the-folder
description: a task's state IS its directory; a bare `mv` to a missing state dir silently corrupts the queue
subsystem: tasks
sources: [internal/tasks/dir.go, internal/tasks/cmd.go, internal/tasks/queue.go, internal/tasks/projection.go]
updated: 2026-09-05
---
A task is a folder, and its STATE is which directory it sits in: `00_todo/` `10_in_progress/`
`50_blocked/` `99_done/` (the numeric prefix only sorts `ls` in lifecycle order). Moving the folder
IS the state change — there is no status field.

The trap: inside a box `coop` isn't installed, so an agent moves folders itself — and a bare
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

## Changelog
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
