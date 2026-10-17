---
name: gate-source-protection
description: Built-in gate files plus a frozen project-owned exact-path list decide which loop changes require protected review; a change list Git cannot read stops the run
subsystem: loop
sources: [internal/project/project.go, internal/tasks/audit.go, internal/loop/loop.go, internal/loop/changes.go, internal/scaffold/projectfile.go]
updated: 2026-10-17
---

Coop always protects its fixed Makefile, CI, hook, and agent-config paths. A project can add the
exact wrapper scripts or source files that implement its checker with top-level `gate_sources` in
`.agent/project.yaml`. Matching is exact: the project owns this finite source-closure statement;
Coop does not guess dependencies from names or imports.

The loop loads and validates the list once after taking the checkout lock, before any worker runs.
That same frozen list drives iteration telemetry, the mandatory protected audit, and later review
context. Editing or removing the declaration cannot erase protection during the current run;
`.agent/project.yaml` is itself a built-in protected file. A legitimate declaration change takes
effect when the loop restarts. A missing list preserves the prior built-in-only behavior.

Which files a task changed is read with Git from a repository the box can write, so a box can make
that read fail: delete an object, damage a ref. The sharp case is the run base's tree — `git log`
and `git status` still work without it, only diffs against it fail. The readers therefore fail
closed: `tasks.ProtectedGateChanges` and the loop's `loopChanges`, `rangeFiles` and `commitFiles`
return an error, never an empty list, and an empty range end is an error too. When a completed
task's files cannot be listed, the loop stops ("Could not list the files this task changed", with
the continue command) rather than audit: a reviewer reading the same repository can hit the same
failure, so its pass would prove little. The task stays done with its final-review debt. Its
continue command settles that review before it claims another task, and while the history stays
unreadable the review refuses at its round start (`record final-review round N: …`). The debt is
keyed by queue and workspace: a loop over other queues (`--tasks`) or in a fork does not see it.
Don't print raw `git` advice here: a box-written `.git/config` (say `core.fsmonitor`) would run on
the host; Coop's own reads go through the trusted view and `forkspace` fsck.
`signoffChanges` stops a round whose change read fails later ("Could not read this run's changes"),
and final verification reports the run unverified. Display-only reads (the diffstat, the review
packet's patch) stay best-effort.

## Changelog
- 2026-10-17 — the changed-file readers fail closed; an unreadable range stops the run, and its
  continue command reviews that task first (task 2026-10-17-fail-closed-when-the-protected-gate-audit-cannot).
- 2026-09-15 — added the explicit project source list and startup snapshot contract.
