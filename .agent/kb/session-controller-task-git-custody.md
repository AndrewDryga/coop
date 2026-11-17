---
name: session-controller-task-git-custody
description: confirmed engineering task bookkeeping is separate checkpoint custody, not product changes or a blanket hidden task folder
subsystem: sessions
sources: [internal/sessionsvc/service.go, internal/sessionsvc/workspace_task.go, internal/sessionsvc/checkpoint_restore.go, internal/sessionsvc/checkpoint.go, internal/tasks/controller_task.go]
updated: 2026-11-17
---

Confirmed engineering sessions generate a queue identity and six files beneath their deterministic
controller task ID. These are task custody, captured separately by the workspace checkpoint; raw
Git status must not present them to the model or validator as unfinished product changes.

After proving the task identity, creation and verified checkpoint restore install anchored local
Git exclusions for those exact filenames, enumerated across the four lifecycle states. Restore
must reinstall them because local Git metadata is not checkpoint payload. The existing rooted
`forkspace.Exclude` refuses redirected or unbounded metadata and preserves existing rules.
Directory-only negations keep unexpected descendants visible at those metadata basenames.
Preparation fences turn admission and prevalidates the unused writable session before any
filesystem write. Exact already-bound operation recovery only republishes the durable receipt.

Never ignore `.agent/`, the queue, all task IDs or a whole bound folder. Unrelated task files and
unexpected files within the bound task stay visible. Tracked changes remain visible even when
their path matches an exclusion. These rules do not replace custody or checkpoint validation,
alter repository ignore files, authorize deletions, or repair historical operation receipts.

A retained closed workspace may still be checkpointed under the same idle, writable, revision,
fork-authority and bound-task guards. Capture never reopens it or permits a new model turn.
This recovers work after a controller closed a completed session before saving its result.

The checkpoint's untracked list leaves out only what the task projection carries: the queue
identity and the bound task's own folder. Any other untracked queue file, such as a follow-up task
the agent filed, is ordinary untracked content, so restore's `git clean` cannot drop it unnoticed.

## Changelog
- 2026-11-17 — capture stopped dropping every untracked `.agent/tasks/` path; only the projection's
  own paths are left out (`checkpointTaskProjectionPath`), and restore verifies the same list.
- 2026-10-24 — exact Git/files/task restore exercised from a closed source, with closure preserved
  and discarded, busy, read-only, unbound and stale-revision capture refused.
- 2026-10-11 — reproduced two production false dirty-workspace corrections with seven generated
  paths; added creation/restore and visibility/redirect regression coverage and exact local rules.
