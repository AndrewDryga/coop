---
name: task-tmp-lifetime
description: task-local tmp survives resumable states but is containment-cleaned on done before review; artifacts persist
subsystem: tasks
sources: [internal/tasks/cmd.go, internal/tasks/lease.go, internal/tasks/audit.go, internal/loop/loop.go, internal/scaffold/templates/agent/tasks/README.md]
updated: 2026-08-10
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

The loop also uses `tmp/lease.lock` as the stable inode for one controller's task lease and
`tmp/lease.json` as heartbeat evidence. The lock follows ordinary task-folder renames, is never
unlinked by release, and is dropped by the kernel on controller death; metadata must resolve the
task's current state folder before each write. Loop cleanup releases the lease before deleting a
newly done task's `tmp/`, so that cleanup is the only normal path that removes the lock file.

## Changelog
- 2026-07-14 — documented loop lease lifetime and the release-before-done-cleanup boundary against `tasklease.go`, `controller.go`, and `commands.go`.
- 2026-07-14 — created from the task-local temporary-workspace lifecycle implementation and tests.
- 2026-08-10 — sources repointed: `taskcmd.go`/`tasklease.go`/`controller.go` moved to
  `internal/tasks/cmd.go`/`internal/tasks/lease.go`/`internal/tasks/audit.go` (the 2026-08
  tasks/lease/completion-audit extraction). Facts unchanged.
- 2026-08-10 — sources repointed: the loop engine moved out of `internal/cli` into `internal/loop`; the loop's lease acquire/release
  lifetime this card describes is now `internal/loop/loop.go`, behavior unchanged.
