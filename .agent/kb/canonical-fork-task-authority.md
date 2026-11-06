---
name: canonical-fork-task-authority
description: the project owns one Markdown task; a fork owns only an exact-generation execution projection and reviewed candidate
subsystem: tasks
sources: [internal/tasks/identity.go, internal/tasks/landed_fork.go, internal/tasks/owner.go, internal/tasks/assignment.go, internal/tasks/assignment_registry.go, internal/tasks/projection.go, internal/tasks/candidate.go, internal/tasks/snapshot.go, internal/tasks/channel.go, internal/tasks/cmd.go, internal/forkspace/generation.go, internal/forkspace/execution.go, internal/forkctl/land.go, internal/cli/fork_cmd.go, internal/loop/prompts.go]
updated: 2026-11-06
---

The canonical queue is the task database. Fork work never creates a second authority: the host
claims one exact `TaskInstance` (random queue ID + task ID, with the folder inode as an additional
replacement signal and the reboot-renumbered device diagnostic) for one immutable,
hardlink-anchored `forkspace.Identity`, moves the canonical folder to in-progress, and materializes only that task
under `.coop/task-executions/<generation>/<assignment>/tasks`. Existing loop prompts, receipts,
between-review, and signoff run against this projection; the canonical queue is never mounted into
the box.

Projection state is evidence, not authority. The host opens it with rooted no-follow operations,
rejects extra tasks/symlinks/hardlinks/oversized trees, snapshots it immutably, then syncs only the
allowed Markdown/artifact surface while holding exact task authority. The box can swap the
assignment folder, or one above it, for a link while it runs, so the in-box task tools walk to the
projection from the fork workspace without following a link and refuse it when a link is on the way,
and write proposals through the outbox that walk opened. For the projection the walk is a check, not
a handle: task mutations still reach it by name and can race a swap ([[in-box-task-channel]]).
Blocked work updates the canonical blocked folder, and the fork scheduler (`runForkLoop`,
internal/cli/fork_cmd.go) then assigns its next task: a pass that exits 3 is accepted as it is,
never through `PrepareForkProjectionForRun`, because a resumed (`paused`) assignment's restore would
unblock the projection again and lose the new decision. The worker reads its answer from the
projection, never the canonical folder, and acceptance copies the projection's decision.md back over
the canonical one. So `resolveAndUnblock` (internal/tasks/cmd.go) copies the canonical decision.md
into the validated projection (`syncForkDecision`) under the canonical lease, before the task returns
to todo, and refuses, leaving the task blocked, when it cannot. The resumed restore repeats the copy
for an unblock that stopped before it, but only over a copy that asks the same question: a fork
stopped after parking a new question, before acceptance carried it here, keeps that question (stop
pauses a working assignment without accepting it). Only decision.md moves; the projection's log,
artifacts and scratch stay its own. When the task's decision.md holds an answer, the loop prompt starts with it
(`decisionResumePrompt`, internal/loop/prompts.go), and a decision.md that cannot be read stops the
launch.
A projected done task becomes `reviewing`; after all signoff and signing,
one generation candidate atomically binds HEAD/tree plus every assignment and projection digest.
Canonical tasks remain in-progress until merge lands that exact candidate. The one other done path
is a person's `coop tasks done <id> --from-fork` (internal/tasks/landed_fork.go): with the fork
stopped, no live candidate and exactly one reachable `Coop-Task` commit on the project's HEAD that
is not in the assignment's baseline, it imports the fork's proposals, completes the task, then
removes proposal records, the index and the owner record in that order, so a rerun finishes an
interrupted one. A crash between the index and owner removal followed by `coop fork rm` leaves an
owner record nothing clears; rerun --from-fork before removing the fork.

Merge writes a replayable land intent before the parent fast-forward. Recovery can therefore finish
canonical task completion, receipts, proposal import, assignment removal, and workspace teardown
exactly once after a crash. Stop merely pauses assignments. Removal/fresh/session discard either
refuse unresolved ownership or write and replay an exact-generation discard. Trailers are human
labels and consistency checks only.

`ReadProjectSnapshot` is the shared read model: it joins canonical queues (including external
assignment roots), typed owners, reverse indexes, generations, candidates, land intents, detached
workers, sandbox executions, and remote-session reservations. Corrupt or unverifiable evidence is
rendered as a problem and blocks mutation; copied queue folders are never state-ranked into truth.
Execution records live beside fork state. The per-project fallback folder older builds could use
(`~/.local/state/coop/executions/v1/<key>/`) is no longer read: anything in it is reported by path
as a problem, and the service-launch locks beside it stay.

## Changelog
- 2026-11-06 — `coop tasks unblock` copies the answer into the fork's projection, the resumed restore
  repeats it, and the loop prompt carries it; the fork's worker had resumed from a copy with no
  answer (task 2026-11-05-carry-a-coop-tasks-unblock-answer-into-the-fork). Acceptance still copies
  the projection's log.md over the canonical one, so the canonical "Unblocked:" line does not survive
  the fork's next acceptance.
- 2026-11-06 — added the person-only `--from-fork` completion of a fork's task whose commit
  already landed (task 2026-11-05-let-the-owner-complete-a-fork-held-task-whose-co).
- 2026-11-05 — a fork loop goes on with its other tasks after one blocks, and a re-blocked resumed
  task stays blocked (task 2026-11-05-keep-a-fork-loop-working-its-other-tasks-when-on).
- 2026-11-04 — dropped the "fall back" sentence: the fallback registry reader is retired (task
  2026-11-04-drop-the-old-fork-and-task-record-readers). A copied queue in a fork is no longer
  refused: the snapshot never read one, and the fork loop takes its tasks from the canonical queue.
- 2026-10-17 — the in-box task tools check the projection, and open the outbox, from the fork
  workspace: a box that swapped its assignment folder or a folder above it for a link to another
  project's .agent had them list, read and change that project's tasks. Verified against
  channel.go, dir.go and taskmcp; the check-then-use race is recorded in [[in-box-task-channel]].
- 2026-09-23 — corrected the allocator assumption: tasks remain authorized by random queue/task
  identity plus locks, semantic records and live pins; durable fork generations now use a private
  hardlink anchor. No task record migration was introduced.
- 2026-09-18 — the fork workspace generation now binds the inode too, and the three places that
  compared whole task instances with `!=` (proposal.go, candidate.go, projection.go) go through
  `sameTaskInstance`. See [[identity-fences-compare-the-inode]].
- 2026-09-18 — the instance fence compares the inode, not the device (6d92825a for ownership; the
  completion receipts, windows and pending reviews followed). Verified against identity.go,
  completion.go and lease.go; the fork workspace generation still compares the device (queued).
- 2026-08-28 — created with canonical scheduling, one-task projections, generation candidates,
  replayable landing/discard, and the unified project activity snapshot.
