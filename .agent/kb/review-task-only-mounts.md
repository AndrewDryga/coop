---
name: review-task-only-mounts
description: review boxes bind the repo read-only, then remount validated task-queue descendants writable; their order is the isolation boundary
subsystem: box
sources: [internal/box/run.go, internal/box/mounts.go, internal/cli/commands.go, internal/cli/tasks.go, internal/loopcfg/loopcfg.go]
updated: 2026-07-14
---

`between`, `signoff`, and `verify` default to `writes: tasks`. `reviewMountPolicy`
then passes the absolute queue roots to `box.Run` with `RepoReadOnly`. `writes: repo`
is the deliberate escape hatch and leaves the normal full-repository bind in place.

`ComputeMounts` emits the repository bind first. `Run` marks that bind read-only, then
inserts writable queue descendants immediately after it; secret decoys follow both.
The mount order matters: a descendant must follow its read-only parent or it remains
read-only in the box. The runtime result is source/config/gate files read-only while
task `log.md`/`state.md` updates and lifecycle moves can proceed.

Queue roots are already limited to the repository by `taskQueues`. Before adding a
writable bind, `repoWritableMounts` checks lexical containment and rejects every
symlink in the queue path. It skips only a genuinely absent configured queue, so a
future monorepo member does not block review of existing queues. A symlink cannot
remount an in-repo config or secret path writable at the queue's lexical target.

## Changelog

- 2026-07-14 — created after adding the task-only review mount policy; verified against
  the runtime-argument tests and the supported Docker nested-bind probe.
- 2026-07-14 — reopened after an in-repo queue symlink could bypass a protected
  target; queue traversal now rejects symlinks and skips only absent queues.
