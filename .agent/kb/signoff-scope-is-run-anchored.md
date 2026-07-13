---
name: signoff-scope-is-run-anchored
description: the signoff reviews a run-anchored folder-diff subject list — re-anchor the baseline ONLY on a receipt-consistent round, or reworked reopens silently escape the next review
subsystem: loop
sources: [internal/cli/commands.go]
updated: 2026-07-13
---

The signoff pass does NOT review all of `99_done/` (that dir holds every prior run's history until a
human prunes it). Its subjects are a folder diff: `newlyFinished(reviewBaseline, doneTaskDirs(hosts))`,
with `reviewBaseline` anchored at run start. Empty diff ⇒ the pass is skipped outright — a fresh
`coop loop` on an already-drained queue runs no review box.

The trap is WHERE the baseline re-anchors. It moves forward only after a round whose
"REVIEW COMPLETE — reopened <N>" receipt matches the folders that actually moved, and it is taken
from the POST-review done set (the reopened tasks have already left `99_done/`, so when their rework
re-enters it they show up in the next round's diff). Two wrong placements that fail silently:

- Re-anchoring on the lost-verdict path: the untrusted round's whole subject set must be reviewed
  again; a moved baseline would diff it away and the re-run would review nothing.
- Re-anchoring from the PRE-review snapshot (e.g. `soSnap`): reopened tasks are still in that done
  set, so their rework diffs to nothing and ships unreviewed.

Accepted tradeoff, by design: done tasks from a previous CRASHED run (completed but never signed
off) are history to the new run and are not re-reviewed — there is no reviewed-marker to tell them
apart, and they passed their own iteration.

Related: [[task-state-is-the-folder]].

## Changelog
- 2026-07-13 — created with the run-scoped signoff change (verified against loop()'s round logic in internal/cli/commands.go).
