---
name: task-state-is-the-folder
description: a task's state IS its directory; a bare `mv` to a missing state dir silently corrupts the queue
verified: 2026-07-12
retire-when: the task system stops using directory-per-state (see .agent/tasks/README.md)
---
A task is a folder, and its STATE is which directory it sits in: `00_todo/` `10_in_progress/`
`50_blocked/` `99_done/` (the numeric prefix only sorts `ls` in lifecycle order). Moving the folder
IS the state change — there is no status field.

The trap: inside a box `coop` isn't installed, so an agent moves folders itself — and a bare
`mv 00_todo/x 10_in_progress/` when `10_in_progress/` does NOT exist does not error; POSIX `mv`
silently RENAMES the task folder to a file called `10_in_progress`, corrupting the queue.
`scaffoldStateDirs` (`internal/cli/taskdir.go`) pre-creates all four state dirs precisely so a
`coop tasks split` slice or a seeded fork queue is always safe to move within. On the HOST use
`coop tasks` (never a manual `mv`); a producer that hands an agent a queue must scaffold the four
state dirs first.
