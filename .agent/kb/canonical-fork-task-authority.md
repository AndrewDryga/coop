---
name: canonical-fork-task-authority
description: the project owns one Markdown task; a fork owns only an exact-generation execution projection and reviewed candidate
subsystem: tasks
sources: [internal/tasks/identity.go, internal/tasks/owner.go, internal/tasks/assignment.go, internal/tasks/assignment_registry.go, internal/tasks/projection.go, internal/tasks/candidate.go, internal/tasks/snapshot.go, internal/forkspace/generation.go, internal/forkspace/execution.go, internal/forkctl/land.go]
updated: 2026-09-18
---

The canonical queue is the task database. Fork work never creates a second authority: the host
claims one exact `TaskInstance` (queue ID, task ID, folder inode — the device is recorded but not
compared, since a reboot renumbers the volume) for one immutable
`forkspace.Identity`, moves the canonical folder to in-progress, and materializes only that task
under `.coop/task-executions/<generation>/<assignment>/tasks`. Existing loop prompts, receipts,
between-review, and signoff run against this projection; the canonical queue is never mounted into
the box.

Projection state is evidence, not authority. The host opens it with rooted no-follow operations,
rejects extra tasks/symlinks/hardlinks/oversized trees, snapshots it immutably, then syncs only the
allowed Markdown/artifact surface while holding exact task authority. Blocked work updates the
canonical blocked folder. A projected done task becomes `reviewing`; after all signoff and signing,
one generation candidate atomically binds HEAD/tree plus every assignment and projection digest.
Canonical tasks remain in-progress until merge lands that exact candidate.

Merge writes a replayable land intent before the parent fast-forward. Recovery can therefore finish
canonical task completion, receipts, proposal import, assignment removal, and workspace teardown
exactly once after a crash. Stop merely pauses assignments. Removal/fresh/session discard either
refuse unresolved ownership or write and replay an exact-generation discard. Trailers are human
labels and consistency checks only.

`ReadProjectSnapshot` is the shared read model: it joins canonical queues (including external
assignment roots), typed owners, reverse indexes, generations, candidates, land intents, detached
workers, sandbox executions, and remote-session reservations. Corrupt or unverifiable evidence is
rendered as a problem and blocks mutation; copied queue folders are never state-ranked into truth.
Execution records normally live beside fork state and fall back to project-keyed durable user state
when an ordinary repository's parent is read-only.

## Changelog
- 2026-09-18 — the fork workspace generation now binds the inode too, and the three places that
  compared whole task instances with `!=` (proposal.go, candidate.go, projection.go) go through
  `sameTaskInstance`. See [[identity-fences-compare-the-inode]].
- 2026-09-18 — the instance fence compares the inode, not the device (6d92825a for ownership; the
  completion receipts, windows and pending reviews followed). Verified against identity.go,
  completion.go and lease.go; the fork workspace generation still compares the device (queued).
- 2026-08-28 — created with canonical scheduling, one-task projections, generation candidates,
  replayable landing/discard, and the unified project activity snapshot.
