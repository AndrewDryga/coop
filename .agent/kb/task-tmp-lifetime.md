---
name: task-tmp-lifetime
description: task-local tmp survives resumable states but is containment-cleaned on done before review; artifacts persist
subsystem: tasks
sources: [internal/cli/taskcmd.go, internal/cli/controller.go, internal/cli/commands.go, internal/scaffold/templates/agent/tasks/README.md]
updated: 2026-07-14
---
A task's `tmp/` is disposable but resumable: because it sits inside the task folder, ordinary
todo/in-progress/blocked/reopen moves carry it along. `tasksFolderMove` removes only `tmp/` when a
host `coop tasks done` reaches done; `coop loop` diffs the queue after an in-box worker returns and
removes `tmp/` from newly done tasks before between-task or signoff review can consume them.

The deletion boundary is the real task directory plus its literal contained `tmp` child. A task
folder symlink is refused, a `tmp` symlink is unlinked rather than followed, and nested symlinks are
removed without touching their targets. Cleanup errors fail completion/review loudly and can be
retried with `coop tasks done <id>`. Anything a reviewer or future maintainer needs belongs in
`artifacts/`, which survives done.

## Changelog
- 2026-07-14 — created from the task-local temporary-workspace lifecycle implementation and tests.
