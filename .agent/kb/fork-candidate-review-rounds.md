---
name: fork-candidate-review-rounds
description: immutable reviewed fork snapshots, the replayable current manifest, fresh candidate-wide signoff, and zero-ahead merge bookkeeping
subsystem: fork
sources: [internal/tasks/candidate.go, internal/tasks/candidate_rounds.go, internal/tasks/candidate_rework.go, internal/tasks/projection.go, internal/cli/fork_cmd.go, internal/forkctl/merge.go]
updated: 2026-11-05
---

A task fork's land authority is one exact candidate ID, HEAD, tree, assignment set and projection
digest set. The mutable generation manifest at the historical candidate path points at immutable
round records under `.coop/candidate-history/<name>.<generation>/`; normal reads follow the current
reference and its direct predecessor rather than scanning history. A bare version 1 candidate file,
which Coop v12.1.0 adopted as round one at the next change, is refused by name and left as it is, as
is a land whose candidate pointer is gone (v10.1.2 and earlier removed it before the land journal).
Unknown, malformed, linked or inconsistent state refuses instead of falling back.

The live phases are `active`, `superseding`, `reviewing`, `reworking` and `publishing`. Supersession makes the
old round non-landable before returning its exact owners to review. The loop then freezes a pending
candidate and runs a separate read-only candidate-wide signoff; only a successful receipt for every
exact subject lets the host move it to publishing. Publication writes the immutable round, makes
each matching owner ready, and switches the manifest to active last. Every boundary is replayable,
but a changed HEAD, tree, assignment, projection, owner or candidate ID authorizes nothing. A new
HEAD must descend from the last reviewed round.

A failed candidate verdict journals its exact rejected snapshot, findings and audit-reopen
authority before touching projections. Only flagged assignments can resume while `reworking`;
new canonical tasks cannot join that frozen cohort. Reopening and canonical acceptance replay
under the lifecycle lock, recovering a durable blocking intent before changing its digest.
Records-only repair uses the existing audit authority without another commit, then requires a
fresh candidate ID and complete signoff. Unauthorizable rework or a repeated identical finding
after unchanged-HEAD repair blocks with the finding. Failure cleanup preserves blocked projections;
only an explicitly unblocked canonical owner can resume them.

Audit rewrites can invalidate another projection's audit authority; the loop parks that task
with recovery guidance rather than granting replacement authority. Rewrites before an already
published round still fail the ancestry fence: preserve the repair and restore the recorded
pre-rework history named by the refusal. The immutable round is never silently replaced.

`landed` and `discarded` are passive terminal phases that retain history without keeping workspace
cleanup active. An unreviewed pending attempt is not promoted into history on discard; an already
authorized publishing attempt is. Any land or discard intent fences new review work. In batch
merge, the zero-ahead decision happens under the same lifecycle lock after land-journal replay: a
fork is skipped only when it has neither commits nor active candidate/task bookkeeping.

## Changelog
- 2026-11-05 — documented candidate rework/replay, records-only completion, dispute blocking and
  the retained audit-history fences; verified against candidate, scheduler and projection paths.
- 2026-11-04 — the version 1 candidate adoption and the land recovery that rebuilt round one are
  retired; both refuse by name (task 2026-11-04-drop-the-old-fork-and-task-record-readers).
- 2026-09-13 — created from the iterative review-round implementation and its migration,
  interruption, mutation, discard and zero-ahead replay tests.
