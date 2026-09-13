---
name: fork-candidate-review-rounds
description: immutable reviewed fork snapshots, the replayable current manifest, fresh candidate-wide signoff, and zero-ahead merge bookkeeping
subsystem: fork
sources: [internal/tasks/candidate.go, internal/tasks/candidate_rounds.go, internal/cli/fork_cmd.go, internal/forkctl/merge.go]
updated: 2026-09-13
---

A task fork's land authority is one exact candidate ID, HEAD, tree, assignment set and projection
digest set. The mutable generation manifest at the historical candidate path points at immutable
round records under `.coop/candidate-history/<name>.<generation>/`; normal reads follow the current
reference and its direct predecessor rather than scanning history. A valid legacy v1 candidate
adopts as round one under the fork lifecycle lock. Unknown, malformed, linked or inconsistent
state refuses instead of falling back.

The live phases are `active`, `superseding`, `reviewing` and `publishing`. Supersession makes the
old round non-landable before returning its exact owners to review. The loop then freezes a pending
candidate and runs a separate read-only candidate-wide signoff; only a successful receipt for every
exact subject lets the host move it to publishing. Publication writes the immutable round, makes
each matching owner ready, and switches the manifest to active last. Every boundary is replayable,
but a changed HEAD, tree, assignment, projection, owner or candidate ID authorizes nothing. A new
HEAD must descend from the last reviewed round.

`landed` and `discarded` are passive terminal phases that retain history without keeping workspace
cleanup active. An unreviewed pending attempt is not promoted into history on discard; an already
authorized publishing attempt is. Any land or discard intent fences new review work. In batch
merge, the zero-ahead decision happens under the same lifecycle lock after land-journal replay: a
fork is skipped only when it has neither commits nor active candidate/task bookkeeping.

## Changelog
- 2026-09-13 — created from the iterative review-round implementation and its migration,
  interruption, mutation, discard and zero-ahead replay tests.
