---
name: task-tmp-lifetime
description: task-local tmp survives resumable states but is containment-cleaned on done before review; artifacts persist
subsystem: tasks
sources: [internal/tasks/cmd.go, internal/tasks/clutter.go, internal/tasks/lease.go, internal/tasks/audit.go, internal/loop/loop.go, internal/forkctl/land.go, internal/forkctl/isolated_land.go, internal/taskmcp/tools.go, internal/taskmcp/evidence_handoff_test.go, internal/scaffold/templates/agent/tasks/README.md]
updated: 2026-10-16
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

Nothing else in the folder is ever removed. Every completion path only warns when the folder holds
an executable ELF/Mach-O file or more than 25 MB, or is too deep or too full to measure
(`TaskBulkWarning`, `internal/tasks/clutter.go`):
a host `coop tasks done` (including an already-done retry) and the loop after its finalize step
print it, `coop fork merge` prints it for each task, led by that task's folder, once a plain or
isolated landing has archived them all (`warnLandedTaskBulk`, `internal/forkctl/land.go`), and a
successful or already-done `tasks_complete` result ends with it when it applies.
The task's top-level `tmp/` never counts, because assigned MCP completion measures the folder
before the host has removed it; a `tmp/` further down does. The walk starts at the project folder
holding the queue and follows no link on the way down to the task, so a box that swaps a folder
between them for a link learns nothing about where it points; for the repository's own queue that
folder is the box's mount point (a subproject's, fork's or COOP_TASKS queue starts below a folder
the box could swap, as every other queue operation does). See
[[task-folders-keep-evidence-not-bulk]].

Assigned MCP completion moves and normalizes the folder before the host finalizes it;
its return does not mean scratch has already vanished. The tool description asks for
promotion and verified durable paths before the call (links into tmp are not durable), and for
keeping programs and rebuildable bulk out of artifacts/.
All successful/already-done replies remind the agent of host cleanup and require an
honest handoff: verified durable artifacts or an explicit unretained-log limitation.
This feedback does not inspect artifacts or attest that a check ran. A native agent
must still be qualified against the actual handoff, separately from lifecycle tests.

Task authority does not live in `tmp/`. The host-only registry under
`~/.local/state/coop/task-leases/` owns its flock and heartbeat metadata, so provider-writable task
scratch is never lease evidence and task-folder moves do not relocate authority.

## Changelog
- 2026-10-16 — `coop fork merge` warns too, naming each task's folder, after
  `FinalizeForkCandidateTask` has archived every task of the landing, plain or isolated;
  re-verified that landing still removes only `tmp/` (`finalizeCompletedTask`).
- 2026-10-16 — the loop's finalize step and the `tasks_complete` result now warn too, and `tmp/` no
  longer counts; re-verified that no completion path removes anything but `tmp/`.
- 2026-10-15 — added the done-time bulk warning; re-verified against `tasksFolderMoveWith` and
  `finalizeCompletedTask` that done still removes only `tmp/`.
- 2026-09-13 — rechecked assigned versus host completion timing, added evidence-handoff feedback
  and lifecycle tests for retained copies, disappearing scratch/dangling links and refusals.
- 2026-08-25 — removed the task-local lease mirror; `tmp/` is scratch only, while the host registry
  is the sole task-lease authority and heartbeat store
- 2026-07-14 — documented loop lease lifetime and the release-before-done-cleanup boundary against `tasklease.go`, `controller.go`, and `commands.go`.
- 2026-07-14 — created from the task-local temporary-workspace lifecycle implementation and tests.
- 2026-08-10 — sources repointed: `taskcmd.go`/`tasklease.go`/`controller.go` moved to
  `internal/tasks/cmd.go`/`internal/tasks/lease.go`/`internal/tasks/audit.go` (the 2026-08
  tasks/lease/completion-audit extraction). Facts unchanged.
- 2026-08-10 — sources repointed: the loop engine moved out of `internal/cli` into `internal/loop`; the loop's lease acquire/release
  lifetime this card describes is now `internal/loop/loop.go`, behavior unchanged.
